package httpapi

import "testing"

func zonedNodes(zones ...string) []NodeCapacity {
	nodes := make([]NodeCapacity, 0, len(zones))
	for index, zone := range zones {
		name := string(rune('a' + index))
		nodes = append(nodes, NodeCapacity{Name: name, CPUMilli: 8000, MemoryBytes: 32 << 30, Schedulable: true,
			Labels: map[string]string{hostnameTopologyKey: name, "topology.kubernetes.io/zone": zone}})
	}
	return nodes
}

func replicas(count int, pod podDemand) []podDemand {
	pods := make([]podDemand, count)
	for index := range pods {
		pods[index] = pod
	}
	return pods
}

func selector(app string) *LabelSelector {
	return &LabelSelector{MatchLabels: map[string]string{"app": app}}
}

func TestPackHonorsRequiredPodAntiAffinityInBothDirections(t *testing.T) {
	nodes := zonedNodes("z1", "z1", "z1")
	web := podDemand{cpuMilli: 500, memoryBytes: 1 << 30, workload: "shop/Deployment/web", namespace: "shop", constraints: SchedulingConstraints{Labels: map[string]string{"app": "web"}}}
	if result := pack(nodes, podDemand{}, replicas(3, web)); result.NodesNeeded != 1 {
		t.Fatalf("unconstrained replicas should share a node: %+v", result)
	}
	web.constraints.PodAntiAffinity = []PodAffinityTerm{{Selector: selector("web"), TopologyKey: hostnameTopologyKey}}
	if result := pack(nodes, podDemand{}, replicas(3, web)); result.NodesNeeded != 3 || result.UnplacedPods != 0 {
		t.Fatalf("one replica per node: %+v", result)
	}
	result := pack(nodes[:2], podDemand{}, replicas(3, web))
	if result.NodesNeeded != 2 || result.UnplacedPods != 1 || result.PodRulePods != 1 || result.NoEligibleNodePods != 0 || result.UnplacedWorkloads[0] != "shop/Deployment/web" {
		t.Fatalf("the third replica has room but no allowed node: %+v", result)
	}
	// Zone anti-affinity allows one replica in the only zone.
	zonal := web
	zonal.constraints.PodAntiAffinity = []PodAffinityTerm{{Selector: selector("web"), TopologyKey: "topology.kubernetes.io/zone"}}
	if result := pack(nodes, podDemand{}, replicas(2, zonal)); result.UnplacedPods != 1 {
		t.Fatalf("zone anti-affinity: %+v", result)
	}
	// The batch pods carry no rules, but the api anti-affinity also keeps
	// them off its node: required anti-affinity is symmetric.
	batch := podDemand{cpuMilli: 100, memoryBytes: 1 << 20, workload: "shop/Deployment/batch", namespace: "shop", constraints: SchedulingConstraints{Labels: map[string]string{"app": "batch"}}}
	guarded := podDemand{cpuMilli: 4000, memoryBytes: 1 << 30, workload: "shop/Deployment/api", namespace: "shop", constraints: SchedulingConstraints{
		Labels: map[string]string{"app": "api"}, PodAntiAffinity: []PodAffinityTerm{{Selector: selector("batch"), TopologyKey: hostnameTopologyKey}},
	}}
	if result := pack(nodes, podDemand{}, []podDemand{guarded, batch}); result.NodesNeeded != 2 {
		t.Fatalf("symmetric anti-affinity: %+v", result)
	}
	// Another namespace is outside a term without namespaces.
	other := batch
	other.workload, other.namespace = "lab/Deployment/batch", "lab"
	if result := pack(nodes, podDemand{}, []podDemand{guarded, other}); result.NodesNeeded != 1 {
		t.Fatalf("anti-affinity crossed namespaces: %+v", result)
	}
	guarded.constraints.PodAntiAffinity[0].AllNamespaces = true
	if result := pack(nodes, podDemand{}, []podDemand{guarded, other}); result.NodesNeeded != 2 {
		t.Fatalf("a namespace selector must reach other namespaces: %+v", result)
	}
}

func TestPackHonorsRequiredPodAffinity(t *testing.T) {
	nodes := zonedNodes("z1", "z2")
	cache := podDemand{cpuMilli: 7000, memoryBytes: 1 << 30, workload: "shop/StatefulSet/cache", namespace: "shop", constraints: SchedulingConstraints{Labels: map[string]string{"app": "cache"}}}
	// The web pods are larger than the cache by memory, so without the
	// affinity ordering they would be placed first, on the first node.
	web := podDemand{cpuMilli: 500, memoryBytes: 8 << 30, workload: "shop/Deployment/web", namespace: "shop", constraints: SchedulingConstraints{
		Labels: map[string]string{"app": "web"}, PodAffinity: []PodAffinityTerm{{Selector: selector("cache"), TopologyKey: hostnameTopologyKey}},
	}}
	cacheOnB := cache
	cacheOnB.pinnedNode = "b"
	result := pack(nodes, podDemand{}, append(replicas(2, web), cacheOnB))
	if result.NodesNeeded != 1 || result.UnplacedPods != 0 || result.PinnedPods != 1 {
		t.Fatalf("web must join the cache node: %+v", result)
	}
	if result := pack(nodes, podDemand{}, append(replicas(2, web), cache)); result.NodesNeeded != 1 || result.UnplacedPods != 0 {
		t.Fatalf("dependent pods are placed after their target: %+v", result)
	}
	if result := pack(nodes, podDemand{}, replicas(2, web)); result.UnplacedPods != 2 || result.PodRulePods != 2 {
		t.Fatalf("affinity to a missing workload cannot be met: %+v", result)
	}
	// A term that matches the pod itself lets the first replica start
	// anywhere; the rest follow it.
	self := web
	self.constraints.PodAffinity = []PodAffinityTerm{{Selector: selector("web"), TopologyKey: "topology.kubernetes.io/zone"}}
	if result := pack(nodes, podDemand{}, replicas(3, self)); result.NodesNeeded != 1 || result.UnplacedPods != 0 {
		t.Fatalf("self affinity: %+v", result)
	}
}

func TestPackHonorsTopologySpread(t *testing.T) {
	nodes := zonedNodes("z1", "z2", "z3")
	web := podDemand{cpuMilli: 500, memoryBytes: 1 << 30, workload: "shop/Deployment/web", namespace: "shop", constraints: SchedulingConstraints{
		Labels: map[string]string{"app": "web"}, TopologySpread: []TopologySpread{{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", Selector: selector("web")}},
	}}
	if result := pack(nodes, podDemand{}, replicas(4, web)); result.NodesNeeded != 3 || result.UnplacedPods != 0 {
		t.Fatalf("zone spread keeps every zone in use: %+v", result)
	}
	// maxSkew 2 lets two replicas share the first zone.
	web.constraints.TopologySpread[0].MaxSkew = 2
	if result := pack(nodes, podDemand{}, replicas(2, web)); result.NodesNeeded != 1 {
		t.Fatalf("maxSkew 2: %+v", result)
	}
	// Nodes without the key are not eligible for a DoNotSchedule rule.
	unlabeled := []NodeCapacity{{Name: "x", CPUMilli: 8000, MemoryBytes: 32 << 30, Schedulable: true}}
	if result := pack(unlabeled, podDemand{}, replicas(1, web)); result.UnplacedPods != 1 || result.PodRulePods != 1 {
		t.Fatalf("missing topology label: %+v", result)
	}
	host := web
	host.constraints.TopologySpread = []TopologySpread{{MaxSkew: 1, TopologyKey: hostnameTopologyKey, Selector: selector("web")}}
	if result := pack(nodes, podDemand{}, replicas(2, host)); result.NodesNeeded != 2 {
		t.Fatalf("hostname spread caps each node at maxSkew: %+v", result)
	}
	// Seven replicas on three nodes need three per node at most.
	if result := pack(nodes, podDemand{}, replicas(7, host)); result.NodesNeeded != 3 || result.UnplacedPods != 0 {
		t.Fatalf("hostname spread with more replicas than nodes: %+v", result)
	}
}

func TestPackKeepsPodsThatDisruptionBudgetsPin(t *testing.T) {
	nodes := zonedNodes("z1", "z1")
	small := podDemand{cpuMilli: 100, memoryBytes: 1 << 20, workload: "shop/Deployment/web", namespace: "shop"}
	if result := pack(nodes, podDemand{}, replicas(2, small)); result.NodesNeeded != 1 {
		t.Fatalf("unpinned pods share a node: %+v", result)
	}
	pinned := small
	pinned.pinnedNode = "b"
	result := pack(nodes, podDemand{}, []podDemand{small, pinned})
	if result.NodesNeeded != 2 || result.PinnedPods != 1 || result.CPURequested != 200 {
		t.Fatalf("a pinned pod keeps its node: %+v", result)
	}
	// A pod pinned to a node that is gone or cordoned is moved instead.
	pinned.pinnedNode = "gone"
	if result := pack(nodes, podDemand{}, []podDemand{small, pinned}); result.NodesNeeded != 1 || result.PinnedPods != 0 {
		t.Fatalf("pin to a missing node: %+v", result)
	}
}

func TestLabelSelectorSemantics(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "front"}
	var none *LabelSelector
	if none.matches(labels) || !(&LabelSelector{}).matches(labels) {
		t.Fatal("a nil selector selects nothing and an empty one everything")
	}
	if (&LabelSelector{MatchLabels: map[string]string{"app": "db"}}).matches(labels) ||
		!(&LabelSelector{MatchExpressions: []NodeRequirement{{Key: "tier", Operator: "In", Values: []string{"front"}}, {Key: "canary", Operator: "DoesNotExist"}}}).matches(labels) {
		t.Fatal("selector expressions")
	}
	term := PodAffinityTerm{Selector: selector("web"), Namespaces: []string{"lab"}}
	if term.selects("shop", "shop", labels) || !term.selects("shop", "lab", labels) {
		t.Fatal("explicit namespaces replace the own namespace")
	}
}
