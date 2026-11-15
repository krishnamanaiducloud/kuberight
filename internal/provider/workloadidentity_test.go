package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/usage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestPodIdentityReportsPodResourcesAndUnfinishedResizes(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	list := func(cpu, memory string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
	}
	pod := workloadPod("database-0", "pod-current", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), controlledBy("apps/v1", "StatefulSet", "database", "stateful-current"))
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: list("250m", "512Mi"), Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "proxy", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: list("50m", "64Mi")}},
		{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: list("1", "1Gi")}},
	}
	identity := podIdentity(*pod)
	if got := identity.Resources["api"]; got != (usage.ContainerResources{CPURequestMilli: 250, MemoryRequestBytes: 512 << 20, MemoryLimitBytes: 1 << 30}) {
		t.Fatalf("api resources = %+v", got)
	}
	if _, found := identity.Resources["migrate"]; found || identity.Resources["proxy"].CPURequestMilli != 50 || identity.Resizing {
		t.Fatalf("identity = %+v", identity)
	}
	for _, condition := range []corev1.PodConditionType{corev1.PodResizePending, corev1.PodResizeInProgress} {
		pod.Status.Conditions = []corev1.PodCondition{{Type: condition, Status: corev1.ConditionTrue}}
		if !podIdentity(*pod).Resizing {
			t.Fatalf("%s was not reported as an unfinished resize", condition)
		}
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if podIdentity(*pod).Resizing {
		t.Fatal("a ready pod was reported as resizing")
	}
}
