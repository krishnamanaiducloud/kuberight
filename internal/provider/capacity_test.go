package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func node(name string, ready bool, labels map[string]string) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3920m"), corev1.ResourceMemory: resource.MustParse("14Gi")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func capacityProvider(client *kubernetesfake.Clientset, objects ...runtime.Object) *KubernetesProvider {
	provider := testProvider(client)
	provider.dynamic = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{nodePoolResource: "NodePoolList", machineResource: "MachineList"}, objects...)
	return provider
}

func TestCapacityReadsNodesKarpenterAndClusterAutoscaler(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		node("a", true, map[string]string{"node.kubernetes.io/instance-type": "m6i.xlarge", "karpenter.sh/nodepool": "default", "karpenter.sh/capacity-type": "spot"}),
		node("b", false, nil),
		node("win", true, map[string]string{"kubernetes.io/os": "windows"}),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler", Namespace: "kube-system"}},
	)
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1", "kind": "NodePool", "metadata": map[string]any{"name": "default"},
		"spec": map[string]any{
			"disruption": map[string]any{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "5m",
				"budgets": []any{map[string]any{"nodes": "10%"}, map[string]any{"nodes": "0", "schedule": "0 9 * * mon-fri", "duration": "8h"}}},
			"limits": map[string]any{"cpu": "1000"},
		},
	}}
	capacity, err := capacityProvider(client, pool).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capacity.Nodes) != 3 || !capacity.Nodes[0].Schedulable || capacity.Nodes[1].Schedulable || capacity.Nodes[2].Schedulable {
		t.Fatalf("nodes = %#v", capacity.Nodes)
	}
	if capacity.Nodes[0].CPUMilli != 3920 || capacity.Nodes[0].CapacityType != "spot" || capacity.Nodes[0].NodePool != "default" {
		t.Fatalf("node a = %#v", capacity.Nodes[0])
	}
	auto := capacity.Autoscaling
	if !auto.Karpenter || !auto.ClusterAutoscaler || len(auto.NodePools) != 1 {
		t.Fatalf("autoscaling = %#v", auto)
	}
	if pool := auto.NodePools[0]; pool.ConsolidationPolicy != "WhenEmpty" || pool.ConsolidateAfter != "5m" || len(pool.DisruptionBudgets) != 2 || pool.DisruptionBudgets[1] != "0 nodes during 0 9 * * mon-fri for 8h" || pool.CPULimit != "1000" {
		t.Fatalf("pool = %#v", pool)
	}
}

func TestCapacityWithoutKarpenter(t *testing.T) {
	capacity, err := capacityProvider(kubernetesfake.NewSimpleClientset(node("a", true, nil))).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Autoscaling.Karpenter || capacity.Autoscaling.ClusterAutoscaler || len(capacity.Autoscaling.Warnings) != 0 {
		t.Fatalf("autoscaling = %#v", capacity.Autoscaling)
	}
}

func TestCapacityRequiresNodeAccess(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	client.Fake.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("denied"))
	})
	if _, err := capacityProvider(client).Capacity(context.Background()); err == nil {
		t.Fatal("capacity succeeded without node access")
	}
}

func TestSchedulingConstraintsFromPodSpec(t *testing.T) {
	spec := corev1.PodSpec{
		NodeSelector: map[string]string{"pool": "general"},
		Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "web", Effect: corev1.TaintEffectNoSchedule}},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}}}},
		}}},
	}
	constraints := schedulingConstraints(spec)
	if constraints.NodeSelector["pool"] != "general" || len(constraints.Tolerations) != 1 || constraints.Tolerations[0].Effect != "NoSchedule" ||
		len(constraints.RequiredTerms) != 1 || constraints.RequiredTerms[0][0].Values[0] != "amd64" {
		t.Fatalf("constraints = %+v", constraints)
	}
	if !schedulingConstraints(corev1.PodSpec{}).Empty() {
		t.Fatal("a plain pod spec must have no constraints")
	}
}

func annotated(node *corev1.Node, annotations map[string]string) *corev1.Node {
	node.Annotations = annotations
	return node
}

func machine(namespace, name, machineSet string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "machine.openshift.io/v1beta1", "kind": "Machine",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{openShiftMachineSetLabel: machineSet}},
	}}
}

func TestCapacityIdentifiesNodeGroupsAndMembershipEvidence(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		node("karpenter", true, map[string]string{"karpenter.sh/nodepool": "default", "eks.amazonaws.com/nodegroup": "ignored"}),
		node("eks", true, map[string]string{"eks.amazonaws.com/nodegroup": "general"}),
		node("eksctl", true, map[string]string{"alpha.eksctl.io/nodegroup-name": "self-managed"}),
		node("gke", true, map[string]string{"cloud.google.com/gke-nodepool": "pool-1"}),
		node("aks", true, map[string]string{"kubernetes.azure.com/agentpool": "nodepool1"}),
		annotated(node("capi", true, nil), map[string]string{"cluster.x-k8s.io/owner-kind": "MachineSet", "cluster.x-k8s.io/owner-name": "md-0-7f9c"}),
		annotated(node("ocp-read", true, nil), map[string]string{openShiftMachineAnnotation: "openshift-machine-api/ocp-worker-a-x7k2p"}),
		annotated(node("ocp-missing", true, nil), map[string]string{openShiftMachineAnnotation: "openshift-machine-api/ocp-worker-b-q9z4m"}),
		node("bare", true, map[string]string{"node.kubernetes.io/instance-type": "m6i.large"}),
	)
	capacity, err := capacityProvider(client, machine("openshift-machine-api", "ocp-worker-a-x7k2p", "ocp-worker-a")).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"karpenter":   {"default", "karpenter", "verified"},
		"eks":         {"general", "eks", "verified"},
		"eksctl":      {"self-managed", "eks", "verified"},
		"gke":         {"pool-1", "gke", "verified"},
		"aks":         {"nodepool1", "aks", "verified"},
		"capi":        {"md-0-7f9c", "cluster-api", "verified"},
		"ocp-read":    {"ocp-worker-a", "openshift", "verified"},
		"ocp-missing": {"ocp-worker-b", "openshift", "inferred"},
		"bare":        {"shape:m6i.large", "none", "inferred"},
	}
	for _, node := range capacity.Nodes {
		if got := [3]string{node.NodeGroup, node.NodeGroupProvider, node.NodeGroupMembership}; got != want[node.Name] || node.NodeGroupEvidence == "" {
			t.Errorf("%s: got %v (%q), want %v", node.Name, got, node.NodeGroupEvidence, want[node.Name])
		}
	}
	if len(capacity.Autoscaling.Warnings) != 0 {
		t.Fatalf("warnings=%v", capacity.Autoscaling.Warnings)
	}
}

func TestCapacityInfersMachineSetsWhenMachinesAreForbidden(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		annotated(node("ocp", true, nil), map[string]string{openShiftMachineAnnotation: "openshift-machine-api/ocp-worker-a-x7k2p"}),
		annotated(node("odd", true, map[string]string{"node.kubernetes.io/instance-type": "m5.xlarge"}), map[string]string{openShiftMachineAnnotation: "openshift-machine-api/hand-made"}),
	)
	provider := capacityProvider(client)
	dynamicClient := provider.dynamic.(*fake.FakeDynamicClient)
	dynamicClient.PrependReactor("list", "machines", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("denied"))
	})
	capacity, err := provider.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]httpapi.NodeCapacity{}
	for _, node := range capacity.Nodes {
		byName[node.Name] = node
	}
	if ocp := byName["ocp"]; ocp.NodeGroup != "ocp-worker-a" || ocp.NodeGroupMembership != "inferred" {
		t.Fatalf("ocp=%#v", ocp)
	}
	// A Machine name without the MachineSet suffix falls back to the shape group.
	if odd := byName["odd"]; odd.NodeGroup != "shape:m5.xlarge" || odd.NodeGroupProvider != "none" {
		t.Fatalf("odd=%#v", odd)
	}
	if len(capacity.Autoscaling.Warnings) != 1 || !strings.Contains(capacity.Autoscaling.Warnings[0], "inferred from Machine names") {
		t.Fatalf("warnings=%v", capacity.Autoscaling.Warnings)
	}
}
