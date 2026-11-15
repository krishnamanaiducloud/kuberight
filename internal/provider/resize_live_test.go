package provider

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/usage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const liveResizeNamespace = "kuberight-live-resize"

// liveContainerResources reads the api container of every live pod.
func liveContainerResources(t *testing.T, provider *KubernetesProvider) (httpapi.WorkloadSpec, map[string]usage.PodIdentity) {
	t.Helper()
	spec, err := provider.Workload(context.Background(), liveResizeNamespace, "StatefulSet", "db")
	if err != nil {
		t.Fatal(err)
	}
	pods := map[string]usage.PodIdentity{}
	for _, pod := range spec.MetricIdentity.Pods {
		if pod.Active() {
			pods[pod.Name] = pod
		}
	}
	return spec, pods
}

func waitForLive(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestLiveInPlaceResize runs the write path against a real cluster. It needs
// Kubernetes 1.33 or later and is skipped unless KUBERIGHT_LIVE_API_SERVER
// is the URL of a `kubectl proxy`; it creates and deletes its own namespace.
func TestLiveInPlaceResize(t *testing.T) {
	server := os.Getenv("KUBERIGHT_LIVE_API_SERVER")
	if server == "" {
		t.Skip("KUBERIGHT_LIVE_API_SERVER is not set")
	}
	restConfig := &rest.Config{Host: server}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	provider := &KubernetesProvider{clusterID: "live", client: client, dynamic: dynamicClient}
	ctx := context.Background()

	_ = client.CoreV1().Namespaces().Delete(ctx, liveResizeNamespace, metav1.DeleteOptions{})
	waitForLive(t, "the previous namespace is gone", func() bool {
		_, err := client.CoreV1().Namespaces().Get(ctx, liveResizeNamespace, metav1.GetOptions{})
		return err != nil
	})
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: liveResizeNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), liveResizeNamespace, metav1.DeleteOptions{})
	})

	replicas, grace := int32(2), int64(1)
	labels := map[string]string{"app": "db"}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: liveResizeNamespace},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "db", Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{TerminationGracePeriodSeconds: &grace, Containers: []corev1.Container{{
					Name: "api", Image: "registry.k8s.io/pause:3.10",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
					},
				}}},
			},
		},
	}
	if _, err := client.AppsV1().StatefulSets(liveResizeNamespace).Create(ctx, statefulSet, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	before := usage.ContainerResources{CPURequestMilli: 100, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 128 << 20}
	after := usage.ContainerResources{CPURequestMilli: 50, MemoryRequestBytes: 32 << 20, MemoryLimitBytes: 128 << 20}
	running := func(want usage.ContainerResources) func() bool {
		return func() bool {
			_, pods := liveContainerResources(t, provider)
			for _, pod := range pods {
				if pod.Phase != "Running" || pod.Resizing || pod.Resources["api"] != want {
					return false
				}
			}
			return len(pods) == 2
		}
	}
	waitForLive(t, "both pods run the original resources", running(before))
	spec, original := liveContainerResources(t, provider)
	if !spec.OnDelete {
		t.Fatal("the OnDelete strategy was not reported")
	}

	// The template patch alone leaves the running pods on the old values:
	// the state verification must not treat as applied.
	change := automation.Change{Namespace: liveResizeNamespace, Kind: "StatefulSet", Workload: "db", Container: "api", ResourceVersion: spec.ResourceVersion, RecommendationID: "rec_live",
		Previous: automation.Resources{CPURequestMilli: 100, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 128 << 20},
		Desired:  automation.Resources{CPURequestMilli: 50, MemoryRequestBytes: 32 << 20, MemoryLimitBytes: 128 << 20}}
	if err := provider.PatchResources(ctx, change); err != nil {
		t.Fatal(err)
	}
	spec, pods := liveContainerResources(t, provider)
	if spec.Containers[0].CPURequestMilli != 50 || spec.Annotations[automation.AppliedAnnotation] != "rec_live" {
		t.Fatalf("template after the patch: %+v annotations=%v", spec.Containers[0], spec.Annotations)
	}
	if spec.AutomationWrite.IsZero() || time.Since(spec.AutomationWrite) > 10*time.Minute {
		t.Fatalf("the workload does not report when KubeRight wrote it: %s", spec.AutomationWrite)
	}
	for name, pod := range pods {
		if pod.Resources["api"] != before {
			t.Fatalf("pod %s changed without a resize: %+v", name, pod.Resources["api"])
		}
		if !pod.Ready || pod.Unsettled("api", spec.AutomationWrite) {
			t.Fatalf("a healthy pod is reported as unsettled: %+v", pod)
		}
	}

	// The API server records when KubeRight wrote the template; a restart
	// of a resized container is judged against that time.
	if target, found, err := provider.resizeTarget(ctx, change); err != nil || !found || target.changed.IsZero() || time.Since(target.changed) > 10*time.Minute {
		t.Fatalf("the time of the template patch was not found: target=%+v found=%v err=%v", target.changed, found, err)
	}

	// The first stage resizes one pod, the highest ordinal; the second
	// follows only once that pod has settled on the new resources.
	result, err := provider.ResizeInPlace(ctx, change)
	if err != nil || !result.Attempted || result.Halted || result.Pods != 2 || result.Resized != 1 || result.Failed != 0 || result.Remaining != 1 {
		t.Fatalf("first stage: result=%+v err=%v", result, err)
	}
	waitForLive(t, "only db-1 runs the new resources", func() bool {
		_, pods := liveContainerResources(t, provider)
		return pods["db-1"].Resources["api"] == after && !pods["db-1"].Resizing && pods["db-0"].Resources["api"] == before
	})
	if result, err := provider.ResizeInPlace(ctx, change); err != nil || result.Halted || result.Resized != 1 || result.Failed != 0 || result.Remaining != 0 {
		t.Fatalf("second stage: result=%+v err=%v", result, err)
	}
	waitForLive(t, "both pods run the new resources", running(after))
	if result, err := provider.ResizeInPlace(ctx, change); err != nil || result.Resized != 0 || result.Remaining != 0 || result.Pods != 2 {
		t.Fatalf("a finished resize was repeated: result=%+v err=%v", result, err)
	}

	// One pod falls behind, as after a failed resize; the retry fixes it.
	revert := []byte(`{"spec":{"containers":[{"name":"api","resources":{"requests":{"cpu":"100m","memory":"64Mi"}}}]}}`)
	if _, err := client.CoreV1().Pods(liveResizeNamespace).Patch(ctx, "db-1", types.StrategicMergePatchType, revert, metav1.PatchOptions{}, "resize"); err != nil {
		t.Fatal(err)
	}
	waitForLive(t, "db-1 is behind the template", func() bool {
		_, pods := liveContainerResources(t, provider)
		return pods["db-1"].Resources["api"] == before && !pods["db-1"].Resizing && pods["db-0"].Resources["api"] == after
	})
	if result, err := provider.ResizeInPlace(ctx, change); err != nil || result.Resized != 1 || result.Failed != 0 || result.Remaining != 0 {
		t.Fatalf("retry: result=%+v err=%v", result, err)
	}
	waitForLive(t, "the retry brought both pods to the new resources", running(after))

	// A resize the API server rejects is reported per pod and changes nothing.
	invalid := change
	invalid.Desired.MemoryRequestBytes = 256 << 20
	if result, err := provider.ResizeInPlace(ctx, invalid); err != nil || result.Resized != 0 || result.Failed != 2 || !strings.Contains(result.Error, "pod db-") {
		t.Fatalf("rejected resize: result=%+v err=%v", result, err)
	}

	// A resize no node can satisfy is accepted by the API server and left
	// pending by the kubelet. It reaches one pod, which is reported as
	// still resizing, and the next stage halts instead of spreading it.
	infeasible := change
	infeasible.Desired.CPURequestMilli = 4_000_000
	if result, err := provider.ResizeInPlace(ctx, infeasible); err != nil || result.Resized != 1 || result.Remaining != 1 {
		t.Fatalf("infeasible resize: result=%+v err=%v", result, err)
	}
	waitForLive(t, "the infeasible resize is reported as unfinished", func() bool {
		_, pods := liveContainerResources(t, provider)
		return pods["db-1"].Resizing && !pods["db-0"].Resizing
	})
	if result, err := provider.ResizeInPlace(ctx, infeasible); err != nil || !result.Halted || result.Resized != 0 || result.Remaining != 1 || !strings.Contains(result.Error, "pod db-1 carries the new resources") {
		t.Fatalf("an unfinished stage did not halt the next: result=%+v err=%v", result, err)
	}
	// The inventory reports the same pod as unsettled, which is what lets
	// automation roll back a resize that stays halted.
	if spec, pods := liveContainerResources(t, provider); pods["db-0"].Resources["api"] != after || !pods["db-1"].Unsettled("api", spec.AutomationWrite) || pods["db-0"].Unsettled("api", spec.AutomationWrite) {
		t.Fatalf("the halted stage changed db-0 or the unsettled pod is not reported: %+v", pods)
	}
	// A stage leaves a pod alone while its resize is unfinished; the
	// rollback that follows a halt reaches it and ends the pending resize.
	if result, err := provider.ResizeInPlace(ctx, change); err != nil || !result.Halted || result.Resized != 0 {
		t.Fatalf("a stage resized a pod that is still resizing: result=%+v err=%v", result, err)
	}
	restore := change
	restore.Rollback = true
	if result, err := provider.ResizeInPlace(ctx, restore); err != nil || result.Halted || result.Resized != 1 || result.Remaining != 0 {
		t.Fatalf("restore: result=%+v err=%v", result, err)
	}
	waitForLive(t, "both pods settle on the new resources", running(after))

	// Nothing was restarted or recreated along the way.
	_, final := liveContainerResources(t, provider)
	for name, pod := range final {
		if pod.UID != original[name].UID {
			t.Fatalf("pod %s was recreated: %s -> %s", name, original[name].UID, pod.UID)
		}
	}
	list, err := client.CoreV1().Pods(liveResizeNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range list.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.RestartCount != 0 {
				t.Fatalf("container %s of pod %s restarted %d times", status.Name, pod.Name, status.RestartCount)
			}
		}
	}
}
