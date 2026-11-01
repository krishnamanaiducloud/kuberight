package provider

import (
	"context"
	"errors"
	"testing"

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
	provider.dynamic = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{nodePoolResource: "NodePoolList"}, objects...)
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
