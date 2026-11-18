package provider

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func ownedPod(name, owner, kind, nodeName string, labels map[string]string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels, OwnerReferences: []metav1.OwnerReference{{Kind: kind, Name: owner, Controller: &controller}}},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestBlocksEvictionsUsesStatusOrAlwaysBlockingSpecs(t *testing.T) {
	zero, one, all := intstr.FromInt32(0), intstr.FromInt32(1), intstr.FromString("100%")
	for name, test := range map[string]struct {
		budget policyv1.PodDisruptionBudget
		want   bool
	}{
		"maxUnavailable 0":         {policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero}}, true},
		"maxUnavailable 1":         {policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &one}}, false},
		"minAvailable 100%":        {policyv1.PodDisruptionBudget{Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &all}}, true},
		"minAvailable equals pods": {policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &one}, Status: policyv1.PodDisruptionBudgetStatus{ObservedGeneration: 2, ExpectedPods: 1, DesiredHealthy: 1}}, true},
		"minAvailable below pods":  {policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &one}, Status: policyv1.PodDisruptionBudgetStatus{ObservedGeneration: 2, ExpectedPods: 3, DesiredHealthy: 1}}, false},
		// Stale status falls back to the spec.
		"stale status": {policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero}, Status: policyv1.PodDisruptionBudgetStatus{ObservedGeneration: 2, ExpectedPods: 3, DesiredHealthy: 1}}, true},
	} {
		if got := blocksEvictions(&test.budget); got != test.want {
			t.Fatalf("%s: blocks=%v", name, got)
		}
	}
}

func TestCapacityPinsPodsThatDisruptionBudgetsProtect(t *testing.T) {
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	controller := true
	web, db := map[string]string{"app": "web"}, map[string]string{"app": "db"}
	client := kubernetesfake.NewSimpleClientset(
		node("a", true, nil), node("b", true, nil),
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-5d9", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &controller}}}},
		ownedPod("web-5d9-1", "web-5d9", "ReplicaSet", "a", web),
		ownedPod("web-5d9-2", "web-5d9", "ReplicaSet", "b", web),
		// Not yet scheduled, so not pinned.
		ownedPod("web-5d9-3", "web-5d9", "ReplicaSet", "", web),
		ownedPod("db-0", "db", "StatefulSet", "a", db),
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero, Selector: &metav1.LabelSelector{MatchLabels: web}}},
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &one, Selector: &metav1.LabelSelector{MatchLabels: db}}},
		// A budget without a selector covers no pods.
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "shop"}, Spec: policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &zero}},
	)
	capacity, err := capacityProvider(client).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pins := capacity.Pinned["shop/Deployment/web"]
	if len(capacity.Pinned) != 1 || len(pins) != 2 || pins[0] != "a" || pins[1] != "b" {
		t.Fatalf("pins = %#v", capacity.Pinned)
	}
	if len(capacity.EvictionBlockers) != 1 || !strings.Contains(capacity.EvictionBlockers[0], "shop/web allows no voluntary evictions; 2 pods") {
		t.Fatalf("blockers = %#v", capacity.EvictionBlockers)
	}
}

func TestCapacityWarnsWhenDisruptionBudgetsAreForbidden(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(node("a", true, nil))
	client.PrependReactor("list", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, "", nil)
	})
	capacity, err := capacityProvider(client).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(capacity.Pinned) != 0 || !strings.Contains(strings.Join(capacity.Autoscaling.Warnings, "\n"), "PodDisruptionBudgets could not be read") {
		t.Fatalf("capacity = %#v", capacity)
	}
}
