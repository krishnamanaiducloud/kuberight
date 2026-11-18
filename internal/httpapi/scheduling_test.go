package httpapi

import (
	"strings"
	"testing"
)

func TestPackHonorsTaintsSelectorsAndRequiredAffinity(t *testing.T) {
	general := NodeCapacity{Name: "general", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true, Labels: map[string]string{"pool": "general", "kubernetes.io/arch": "amd64"}}
	gpu := NodeCapacity{Name: "gpu", CPUMilli: 8000, MemoryBytes: 32 << 30, Schedulable: true, Labels: map[string]string{"pool": "gpu", "kubernetes.io/arch": "amd64"},
		Taints: []NodeTaint{{Key: "nvidia.com/gpu", Value: "present", Effect: "NoSchedule"}}}
	arm := NodeCapacity{Name: "arm", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true, Labels: map[string]string{"pool": "general", "kubernetes.io/arch": "arm64"},
		Taints: []NodeTaint{{Key: "soft", Effect: "PreferNoSchedule"}}}
	nodes := []NodeCapacity{general, gpu, arm}
	plain := podDemand{cpuMilli: 3000, memoryBytes: 1 << 30, workload: "shop/Deployment/web"}
	// Without constraints three 3-core pods would spread over all three
	// nodes, including the tainted GPU node.
	if result := pack(nodes, podDemand{}, []podDemand{plain, plain, plain}); result.UnplacedPods != 1 || result.NodesNeeded != 2 {
		t.Fatalf("taints were ignored: %+v", result)
	}
	gpuPod := podDemand{cpuMilli: 1000, memoryBytes: 1 << 30, workload: "ml/Deployment/train", constraints: SchedulingConstraints{
		NodeSelector: map[string]string{"pool": "gpu"},
		Tolerations:  []Toleration{{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"}},
	}}
	if result := pack(nodes, podDemand{}, []podDemand{gpuPod}); result.UnplacedPods != 0 || result.NodesNeeded != 1 {
		t.Fatalf("a tolerating, selected pod was not placed on its node: %+v", result)
	}
	amdOnly := podDemand{cpuMilli: 3000, memoryBytes: 1 << 30, workload: "shop/StatefulSet/db", constraints: SchedulingConstraints{
		RequiredTerms: [][]NodeRequirement{{{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"amd64"}}}},
	}}
	// The only untainted amd64 node fits one of these two pods.
	result := pack(nodes, podDemand{}, []podDemand{amdOnly, amdOnly})
	if result.UnplacedPods != 1 || result.NoEligibleNodePods != 0 || len(result.UnplacedWorkloads) != 1 || result.UnplacedWorkloads[0] != "shop/StatefulSet/db" {
		t.Fatalf("required affinity: %+v", result)
	}
	impossible := podDemand{cpuMilli: 100, memoryBytes: 1 << 20, workload: "shop/Deployment/edge", constraints: SchedulingConstraints{NodeSelector: map[string]string{"pool": "edge"}}}
	if result := pack(nodes, podDemand{}, []podDemand{impossible}); result.NoEligibleNodePods != 1 || result.UnplacedPods != 1 {
		t.Fatalf("a pod with no eligible node: %+v", result)
	}
}

func TestTolerationAndRequirementSemantics(t *testing.T) {
	taint := NodeTaint{Key: "dedicated", Value: "batch", Effect: "NoExecute"}
	for name, test := range map[string]struct {
		toleration Toleration
		want       bool
	}{
		"equal match":      {Toleration{Key: "dedicated", Operator: "Equal", Value: "batch", Effect: "NoExecute"}, true},
		"default equal":    {Toleration{Key: "dedicated", Value: "batch"}, true},
		"wrong value":      {Toleration{Key: "dedicated", Value: "web"}, false},
		"wrong effect":     {Toleration{Key: "dedicated", Operator: "Exists", Effect: "NoSchedule"}, false},
		"exists any key":   {Toleration{Operator: "Exists"}, true},
		"exists other key": {Toleration{Key: "other", Operator: "Exists"}, false},
	} {
		if got := test.toleration.tolerates(taint); got != test.want {
			t.Fatalf("%s: tolerates=%v", name, got)
		}
	}
	labels := map[string]string{"zone": "a"}
	for name, test := range map[string]struct {
		requirement NodeRequirement
		want        bool
	}{
		"in":             {NodeRequirement{Key: "zone", Operator: "In", Values: []string{"a", "b"}}, true},
		"not in":         {NodeRequirement{Key: "zone", Operator: "NotIn", Values: []string{"a"}}, false},
		"not in missing": {NodeRequirement{Key: "rack", Operator: "NotIn", Values: []string{"1"}}, true},
		"exists":         {NodeRequirement{Key: "zone", Operator: "Exists"}, true},
		"does not exist": {NodeRequirement{Key: "zone", Operator: "DoesNotExist"}, false},
		"gt is unknown":  {NodeRequirement{Key: "zone", Operator: "Gt", Values: []string{"1"}}, false},
	} {
		if got := test.requirement.matches(labels); got != test.want {
			t.Fatalf("%s: matches=%v", name, got)
		}
	}
}

func TestNodeGroupImpactsSplitPackingByGroup(t *testing.T) {
	large := func(name string) NodeCapacity {
		return NodeCapacity{Name: name, CPUMilli: 8000, MemoryBytes: 32 << 30, Schedulable: true, InstanceType: "m6i.2xlarge", CapacityType: "on-demand",
			NodeGroup: "general", NodeGroupProvider: NodeGroupEKS, NodeGroupMembership: MembershipVerified, NodeGroupEvidence: "label eks.amazonaws.com/nodegroup"}
	}
	small := NodeCapacity{Name: "s1", CPUMilli: 2000, MemoryBytes: 8 << 30, Schedulable: true, NodeGroup: "shape:2000m-8192Mi", NodeGroupProvider: NodeGroupNone, NodeGroupMembership: MembershipInferred}
	nodes := []NodeCapacity{large("l1"), large("l2"), small}
	nodes[1].NodeGroupMembership, nodes[1].NodeGroupEvidence = MembershipInferred, "Machine name general-abcde"
	demand := func(cpu int64) []podDemand {
		pods := []podDemand{}
		for range 8 {
			pods = append(pods, podDemand{cpuMilli: cpu, memoryBytes: 1 << 30})
		}
		return pods
	}
	// Eight 1800m pods fill both large nodes; eight 900m pods fit on one.
	current, recommended := pack(nodes, podDemand{}, demand(1800)), pack(nodes, podDemand{}, demand(900))
	groups := nodeGroupImpacts(nodes, NodeAutoscaling{}, nil, current, recommended)
	if len(groups) != 2 {
		t.Fatalf("groups=%#v", groups)
	}
	general := groups[0]
	if general.Name != "general" || general.Nodes != 2 || general.CurrentNodesNeeded != 2 || general.RecommendedNodesNeeded != 1 || general.ReclaimableNodes != 1 ||
		general.CPUMilli != 16000 || len(general.InstanceTypes) != 1 || general.CapacityTypes[0] != "on-demand" {
		t.Fatalf("general=%#v", general)
	}
	// One inferred member makes the whole group inferred, with that evidence.
	if general.Membership != MembershipInferred || general.Evidence != "Machine name general-abcde" || !strings.Contains(general.Realization, "lower its desired size") {
		t.Fatalf("general membership=%#v", general)
	}
	if shape := groups[1]; shape.Provider != NodeGroupNone || shape.CurrentNodesNeeded != 0 || !strings.Contains(shape.Realization, "manually") {
		t.Fatalf("shape=%#v", shape)
	}
}

func TestGroupRealizationFollowsTheGroupsAutoscaler(t *testing.T) {
	for _, test := range []struct {
		group             NodeGroupImpact
		policy            string
		clusterAutoscaler bool
		want              string
	}{
		{NodeGroupImpact{Provider: NodeGroupKarpenter}, "WhenEmpty", false, "removes only empty nodes"},
		{NodeGroupImpact{Provider: NodeGroupKarpenter}, "WhenEmptyOrUnderutilized", false, "consolidates underutilized nodes"},
		{NodeGroupImpact{Provider: NodeGroupKarpenter}, "", false, "could not be read"},
		{NodeGroupImpact{Provider: NodeGroupEKS}, "", true, "Cluster Autoscaler"},
		{NodeGroupImpact{Provider: NodeGroupGKE}, "", false, "node-pool autoscaler"},
		{NodeGroupImpact{Provider: NodeGroupOpenShift}, "", false, "MachineAutoscaler"},
		{NodeGroupImpact{Provider: NodeGroupClusterAPI}, "", false, "MachineDeployment"},
	} {
		if got := groupRealization(test.group, test.policy, test.clusterAutoscaler); !strings.Contains(got, test.want) {
			t.Errorf("%s/%q: %q does not mention %q", test.group.Provider, test.policy, got, test.want)
		}
	}
}

func TestNodeGroupLimitsBoundTheReclaimableNodes(t *testing.T) {
	node := func(name string) NodeCapacity {
		return NodeCapacity{Name: name, CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true, NodeGroup: "general", NodeGroupProvider: NodeGroupEKS, NodeGroupMembership: MembershipVerified}
	}
	nodes := []NodeCapacity{node("a"), node("b"), node("c"), node("d")}
	pods := func(cpu int64) []podDemand {
		return replicas(4, podDemand{cpuMilli: cpu, memoryBytes: 1 << 30})
	}
	// Four 3500m pods need four nodes; four 900m pods fit on one.
	current, recommended := pack(nodes, podDemand{}, pods(3500)), pack(nodes, podDemand{}, pods(900))
	limits := map[string]NodeGroupLimit{nodeGroupKey(nodes[0]): {MinSize: 3, MaxSize: 6, Source: "Cluster Autoscaler status kube-system/cluster-autoscaler-status: eks-general-1", Membership: MembershipInferred}}
	group := nodeGroupImpacts(nodes, NodeAutoscaling{ClusterAutoscaler: true}, limits, current, recommended)[0]
	if group.CurrentNodesNeeded != 4 || group.RecommendedNodesNeeded != 1 || group.ReclaimableNodes != 1 || !group.LimitedByMinimum || group.ExceedsMaximum {
		t.Fatalf("the minimum size keeps three nodes: %#v", group)
	}
	if group.Limit == nil || !strings.Contains(group.Realization, "down to its minimum of 3 (maximum 6)") {
		t.Fatalf("realization=%q", group.Realization)
	}
	limits[nodeGroupKey(nodes[0])] = NodeGroupLimit{MinSize: 0, MaxSize: 0, Source: "MachineAutoscaler openshift-machine-api/general", Membership: MembershipVerified}
	if group := nodeGroupImpacts(nodes, NodeAutoscaling{}, limits, current, recommended)[0]; !group.ExceedsMaximum || group.ReclaimableNodes != 3 {
		t.Fatalf("a recommendation above the maximum is flagged: %#v", group)
	}
}
