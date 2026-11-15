package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// resizedPod is a running, ready pod that already carries the resources of
// resizeChange.
func resizedPod(name string) *corev1.Pod {
	pod := readyPod(name)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	return pod
}

// readyPod is a running, ready pod that still carries the template resources.
func readyPod(name string) *corev1.Pod {
	pod := resizePod(name, "db", corev1.PodRunning)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

func patchedPods(patched []string) string {
	names := make([]string, 0, len(patched))
	for _, patch := range patched {
		name, _, _ := strings.Cut(patch, " ")
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

func TestResizeInPlaceGrowsTheBatchWithTheResizedPods(t *testing.T) {
	// Each stage starts from the pods the previous one resized: one pod,
	// then as many as already carry the new resources.
	for _, stage := range []struct {
		resized          int
		patched          string
		wanted, remained int
	}{
		{0, "db-4", 1, 4},
		{1, "db-3", 1, 3},
		{2, "db-2,db-1", 2, 1},
		{4, "db-0", 1, 0},
		{5, "", 0, 0},
	} {
		objects := []runtime.Object{resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType)}
		for ordinal := 0; ordinal < 5; ordinal++ {
			if name := fmt.Sprintf("db-%d", ordinal); ordinal >= 5-stage.resized {
				objects = append(objects, resizedPod(name))
			} else {
				objects = append(objects, readyPod(name))
			}
		}
		client := kubernetesfake.NewSimpleClientset(objects...)
		patched := recordResizes(client, nil)
		result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
		if err != nil || !result.Attempted || result.Halted || result.Pods != 5 || result.Resized != stage.wanted || result.Failed != 0 ||
			result.Remaining != stage.remained || patchedPods(*patched) != stage.patched {
			t.Fatalf("with %d resized pods: result=%+v err=%v patched=%s", stage.resized, result, err, patchedPods(*patched))
		}
	}
}

func TestResizeInPlaceStartsWithTheHighestOrdinal(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), readyPod("db-0"), readyPod("db-9"), readyPod("db-10"))
	patched := recordResizes(client, nil)
	if result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange()); err != nil || patchedPods(*patched) != "db-10" || result.Remaining != 2 {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}
}

func TestResizeInPlaceHaltsWhileAResizedPodIsUnsettled(t *testing.T) {
	changed := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	restarted := func(at time.Time) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "db", RestartCount: 1,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(at)}}}}
		}
	}
	condition := func(kind corev1.PodConditionType, status corev1.ConditionStatus) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: kind, Status: status})
		}
	}
	for name, test := range map[string]struct {
		mutate func(*corev1.Pod)
		halted bool
	}{
		"not ready":               {func(pod *corev1.Pod) { pod.Status.Conditions[0].Status = corev1.ConditionFalse }, true},
		"no readiness reported":   {func(pod *corev1.Pod) { pod.Status.Conditions = nil }, true},
		"resize pending":          {condition(corev1.PodResizePending, corev1.ConditionTrue), true},
		"resize in progress":      {condition(corev1.PodResizeInProgress, corev1.ConditionTrue), true},
		"restarted after change":  {restarted(changed.Add(time.Minute)), true},
		"restarted before change": {restarted(changed.Add(-time.Minute)), false},
		"another container restart": {func(pod *corev1.Pod) {
			restarted(changed.Add(time.Minute))(pod)
			pod.Status.ContainerStatuses[0].Name = "proxy"
		}, false},
		"resize finished": {condition(corev1.PodResizeInProgress, corev1.ConditionFalse), false},
	} {
		workload := resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType)
		written := metav1.NewTime(changed)
		workload.ManagedFields = []metav1.ManagedFieldsEntry{
			{Manager: "kubectl", Time: &metav1.Time{Time: changed.Add(time.Hour)}},
			{Manager: automation.FieldManager, Time: &written},
		}
		canary := resizedPod("db-2")
		test.mutate(canary)
		client := kubernetesfake.NewSimpleClientset(workload, readyPod("db-0"), readyPod("db-1"), canary)
		patched := recordResizes(client, nil)
		result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
		if err != nil || !result.Attempted || result.Halted != test.halted {
			t.Fatalf("%s: result=%+v err=%v", name, result, err)
		}
		if test.halted {
			if len(*patched) != 0 || result.Resized != 0 || result.Failed != 0 || result.Remaining != 2 || !strings.Contains(result.Error, "pod db-2 carries the new resources") {
				t.Fatalf("%s: a halted resize touched pods: result=%+v patched=%s", name, result, patchedPods(*patched))
			}
		} else if patchedPods(*patched) != "db-1" || result.Remaining != 1 {
			t.Fatalf("%s: result=%+v patched=%s", name, result, patchedPods(*patched))
		}
	}
}

func TestResizeInPlaceLeavesUnreadyPodsForALaterPass(t *testing.T) {
	unready := func(name string) *corev1.Pod {
		pod := readyPod(name)
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		return pod
	}
	resizing := readyPod("db-3")
	resizing.Status.Conditions = append(resizing.Status.Conditions, corev1.PodCondition{Type: corev1.PodResizeInProgress, Status: corev1.ConditionTrue})
	// An old restart says nothing about a pod that never took the change.
	restarted := readyPod("db-1")
	restarted.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "db", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}}}

	// The highest ordinals are unwell, so the first healthy pod goes first.
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), readyPod("db-0"), restarted, unready("db-2"), resizing)
	patched := recordResizes(client, nil)
	result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
	if err != nil || result.Halted || result.Pods != 4 || result.Resized != 1 || result.Remaining != 3 || patchedPods(*patched) != "db-1" {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}

	// With only unwell pods left, the pass says so instead of reporting
	// progress it did not make.
	client = kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), resizedPod("db-0"), unready("db-1"), unready("db-2"))
	patched = recordResizes(client, nil)
	result, err = testProvider(client).ResizeInPlace(context.Background(), resizeChange())
	if err != nil || !result.Halted || result.Resized != 0 || result.Remaining != 2 || len(*patched) != 0 || !strings.Contains(result.Error, "pod db-1 is not ready or is still resizing; pods left on their resources until they are ready: 2") {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}
}

func TestResizeInPlaceDoesNotHaltWhenEveryPodIsResized(t *testing.T) {
	// An unready pod is not a reason to report a halt once nothing is left
	// to resize.
	unready := resizedPod("db-1")
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), resizedPod("db-0"), unready)
	patched := recordResizes(client, nil)
	if result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange()); err != nil || result.Halted || result.Remaining != 0 || len(*patched) != 0 {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}
}

func TestResizeInPlaceRollsBackEveryPodAtOnce(t *testing.T) {
	// A rollback restores values that ran before: it is neither staged nor
	// held back by an unhealthy pod, which is often why it happens.
	change := resizeChange()
	change.Desired, change.Previous, change.Rollback = change.Previous, change.Desired, true
	unready := resizedPod("db-2")
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	restored := readyPod("db-3")
	restored.Status.Conditions[0].Status = corev1.ConditionFalse
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), resizedPod("db-0"), resizedPod("db-1"), unready, restored)
	patched := recordResizes(client, nil)
	result, err := testProvider(client).ResizeInPlace(context.Background(), change)
	if err != nil || result.Halted || result.Pods != 4 || result.Resized != 3 || result.Remaining != 0 || patchedPods(*patched) != "db-0,db-1,db-2" {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}
}

func TestResizeInPlaceARejectedPodDoesNotUseUpTheBatch(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), readyPod("db-0"), readyPod("db-1"), readyPod("db-2"))
	patched := recordResizes(client, func(pod string) error {
		if pod == "db-2" {
			return fmt.Errorf("the resize is not allowed")
		}
		return nil
	})
	result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
	if err != nil || result.Resized != 1 || result.Failed != 1 || result.Remaining != 1 || patchedPods(*patched) != "db-2,db-1" || !strings.Contains(result.Error, "pod db-2") {
		t.Fatalf("result=%+v err=%v patched=%s", result, err, patchedPods(*patched))
	}
}
