package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func identityMetadata(name string, uid types.UID, created time.Time) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "shop", UID: uid, Generation: 3, CreationTimestamp: metav1.NewTime(created)}
}

func workloadPod(name string, uid types.UID, created time.Time, owner metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: uid, CreationTimestamp: metav1.NewTime(created), OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestWorkloadMetricIdentityFollowsExactDeploymentUIDChain(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	metadata := identityMetadata("checkout", "deployment-current", created)
	currentRS := &appsv1.ReplicaSet{ObjectMeta: identityMetadata("checkout-abc", "rs-current", created.Add(time.Minute))}
	currentRS.OwnerReferences = []metav1.OwnerReference{controlledBy("apps/v1", "Deployment", "checkout", "deployment-current")}
	oldRS := &appsv1.ReplicaSet{ObjectMeta: identityMetadata("checkout-old", "rs-old", created.Add(-time.Hour))}
	oldRS.OwnerReferences = []metav1.OwnerReference{controlledBy("apps/v1", "Deployment", "checkout", "deployment-old")}
	client := kubernetesfake.NewSimpleClientset(
		currentRS, oldRS,
		workloadPod("checkout-abc-11111", "pod-current", created.Add(2*time.Minute), controlledBy("apps/v1", "ReplicaSet", currentRS.Name, currentRS.UID)),
		workloadPod("checkout-old-22222", "pod-old", created.Add(-30*time.Minute), controlledBy("apps/v1", "ReplicaSet", oldRS.Name, oldRS.UID)),
	)
	identity := testProvider(client).workloadMetricIdentity(context.Background(), httpapi.WorkloadSpec{Namespace: "shop", Kind: "Deployment", Name: "checkout", Replicas: 1}, metadata)
	if identity.UID != "deployment-current" || identity.Generation != 3 || identity.ExpectedReplicas != 1 || len(identity.ResolutionIssues) != 0 {
		t.Fatalf("workload identity = %#v", identity)
	}
	if len(identity.Pods) != 1 || identity.Pods[0].UID != "pod-current" || identity.Pods[0].Name != "checkout-abc-11111" || len(identity.Pods[0].Containers) != 1 {
		t.Fatalf("resolved pods = %#v", identity.Pods)
	}
}

func TestWorkloadMetricIdentityRejectsRecreatedDirectOwner(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	metadata := identityMetadata("database", "stateful-current", created)
	client := kubernetesfake.NewSimpleClientset(
		workloadPod("database-0", "pod-current", created.Add(time.Minute), controlledBy("apps/v1", "StatefulSet", "database", "stateful-current")),
		workloadPod("database-old", "pod-old", created.Add(-time.Hour), controlledBy("apps/v1", "StatefulSet", "database", "stateful-old")),
	)
	identity := testProvider(client).workloadMetricIdentity(context.Background(), httpapi.WorkloadSpec{Namespace: "shop", Kind: "StatefulSet", Name: "database", Replicas: 1}, metadata)
	if len(identity.Pods) != 1 || identity.Pods[0].UID != "pod-current" {
		t.Fatalf("same-name recreated owner was mixed into identity: %#v", identity.Pods)
	}
}

func TestWorkloadMetricIdentityReportsMissingImmutableMetadata(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	metadata := identityMetadata("database", "stateful-current", created)
	client := kubernetesfake.NewSimpleClientset(workloadPod("database-0", "", created.Add(time.Minute), controlledBy("apps/v1", "StatefulSet", "database", "stateful-current")))
	identity := testProvider(client).workloadMetricIdentity(context.Background(), httpapi.WorkloadSpec{Namespace: "shop", Kind: "StatefulSet", Name: "database", Replicas: 1}, metadata)
	if len(identity.Pods) != 0 || len(identity.ResolutionIssues) != 1 || !strings.Contains(identity.ResolutionIssues[0], "immutable UID") {
		t.Fatalf("missing pod UID was not fail-closed: %#v", identity)
	}
}

func TestWorkloadPopulatesMetricIdentity(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: identityMetadata("checkout", "deployment-current", created),
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}}}},
	}
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: identityMetadata("checkout-abc", "rs-current", created.Add(time.Minute))}
	replicaSet.OwnerReferences = []metav1.OwnerReference{controlledBy("apps/v1", "Deployment", "checkout", "deployment-current")}
	client := kubernetesfake.NewSimpleClientset(deployment, replicaSet, workloadPod("checkout-abc-11111", "pod-current", created.Add(2*time.Minute), controlledBy("apps/v1", "ReplicaSet", replicaSet.Name, replicaSet.UID)))
	spec, err := testProvider(client).Workload(context.Background(), "shop", "Deployment", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if spec.MetricIdentity == nil || spec.MetricIdentity.UID != "deployment-current" || len(spec.MetricIdentity.Pods) != 1 {
		t.Fatalf("metric identity = %#v", spec.MetricIdentity)
	}
}
