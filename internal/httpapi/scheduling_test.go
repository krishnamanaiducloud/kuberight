package httpapi

import "testing"

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
