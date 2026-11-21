package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/usage"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// Only the ReplicaSet whose controller reference carries the current
	// Deployment UID is a verified owner; the same-name predecessor's is not.
	if len(identity.Owners) != 1 || identity.Owners[0].UID != "rs-current" || identity.Owners[0].Name != "checkout-abc" || identity.Owners[0].Kind != "ReplicaSet" || !identity.Owners[0].CreatedAt.Equal(created.Add(time.Minute)) {
		t.Fatalf("verified owners = %#v", identity.Owners)
	}
}

func TestWorkloadMetricIdentityKeepsEveryRetainedRevisionAndCronJobJobs(t *testing.T) {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	metadata := identityMetadata("checkout", "deployment-current", created)
	objects := []runtime.Object{}
	for index, name := range []string{"checkout-r3", "checkout-r1", "checkout-r2"} {
		replicaSet := &appsv1.ReplicaSet{ObjectMeta: identityMetadata(name, types.UID("rs-"+name), created.Add(time.Duration(index)*time.Hour))}
		replicaSet.OwnerReferences = []metav1.OwnerReference{controlledBy("apps/v1", "Deployment", "checkout", "deployment-current")}
		replicaSet.Status.Replicas = int32(index)
		objects = append(objects, replicaSet)
	}
	identity := testProvider(kubernetesfake.NewSimpleClientset(objects...)).workloadMetricIdentity(context.Background(), httpapi.WorkloadSpec{Namespace: "shop", Kind: "Deployment", Name: "checkout"}, metadata)
	if len(identity.Owners) != 3 || identity.Owners[0].Name != "checkout-r3" || identity.Owners[2].Name != "checkout-r2" || identity.Owners[2].Replicas != 2 {
		t.Fatalf("rolling-update revisions must all be kept, oldest first: %#v", identity.Owners)
	}

	cron := identityMetadata("nightly", "cron-current", created)
	job := &batchv1.Job{ObjectMeta: identityMetadata("nightly-29000000", "job-current", created.Add(time.Hour))}
	job.OwnerReferences = []metav1.OwnerReference{controlledBy("batch/v1", "CronJob", "nightly", "cron-current")}
	job.Status.Active = 1
	stale := &batchv1.Job{ObjectMeta: identityMetadata("nightly-28000000", "job-old", created.Add(-time.Hour))}
	stale.OwnerReferences = []metav1.OwnerReference{controlledBy("batch/v1", "CronJob", "nightly", "cron-old")}
	identity = testProvider(kubernetesfake.NewSimpleClientset(job, stale)).workloadMetricIdentity(context.Background(), httpapi.WorkloadSpec{Namespace: "shop", Kind: "CronJob", Name: "nightly"}, cron)
	if len(identity.Owners) != 1 || identity.Owners[0].UID != "job-current" || identity.Owners[0].Kind != "Job" || identity.Owners[0].Replicas != 1 {
		t.Fatalf("CronJob owners = %#v", identity.Owners)
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
	// A resize the kubelet has not seen yet has no condition, only a spec
	// generation ahead of the observed one.
	for name, test := range map[string]struct {
		generation, observed int64
		resizing             bool
	}{
		"not yet observed":       {3, 2, true},
		"observed":               {3, 3, false},
		"generation not tracked": {3, 0, false},
	} {
		unseen := pod.DeepCopy()
		unseen.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		unseen.Generation, unseen.Status.ObservedGeneration = test.generation, test.observed
		if got := podIdentity(*unseen); got.Resizing != test.resizing || got.Unsettled("api", time.Time{}) != test.resizing {
			t.Fatalf("%s: resizing=%v", name, got.Resizing)
		}
	}
	if identity.Ready || identity.Restarted != nil || !identity.Unsettled("api", time.Time{}) {
		t.Fatalf("a pod without a Ready condition must be unsettled: %+v", identity)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if podIdentity(*pod).Resizing {
		t.Fatal("a ready pod was reported as resizing")
	}

	// A restart counts only for the named container and only after the
	// time the caller cares about.
	finished := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "api", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(finished)}}},
		{Name: "other"},
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "proxy", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(finished.Add(time.Hour))}}},
	}
	identity = podIdentity(*pod)
	if !identity.Ready || len(identity.Restarted) != 2 || !identity.Restarted["api"].Equal(finished) || !identity.Restarted["proxy"].Equal(finished.Add(time.Hour)) {
		t.Fatalf("identity = %+v", identity)
	}
	for name, test := range map[string]struct {
		container string
		since     time.Time
		unsettled bool
	}{
		"restarted after":        {"api", finished.Add(-time.Second), true},
		"restarted before":       {"api", finished, false},
		"time not recorded":      {"api", time.Time{}, false},
		"another container":      {"other", finished.Add(-time.Second), false},
		"sidecar restarted late": {"proxy", finished, true},
	} {
		if got := identity.Unsettled(test.container, test.since); got != test.unsettled {
			t.Fatalf("%s: unsettled = %v", name, got)
		}
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if !podIdentity(*pod).Unsettled("other", time.Time{}) {
		t.Fatal("an unready pod must be unsettled")
	}
}
