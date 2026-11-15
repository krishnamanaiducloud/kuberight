package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/automation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func resizeTemplate(policy ...corev1.ContainerResizePolicy) corev1.PodTemplateSpec {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "db"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "db:1", Resources: resources, ResizePolicy: policy}}},
	}
}

func resizeStatefulSet(strategy appsv1.StatefulSetUpdateStrategyType, policy ...corev1.ContainerResizePolicy) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", UID: "sts-1"},
		Spec: appsv1.StatefulSetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: strategy},
			Template:       resizeTemplate(policy...),
		},
	}
}

func resizePod(name, owner string, phase corev1.PodPhase) *corev1.Pod {
	controller := true
	template := resizeTemplate()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: template.Labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", UID: "sts-1", Controller: &controller}}},
		Spec:   template.Spec,
		Status: corev1.PodStatus{Phase: phase},
	}
}

func resizeChange() automation.Change {
	return automation.Change{Namespace: "shop", Kind: "StatefulSet", Workload: "db", Container: "db", ResourceVersion: "7", RecommendationID: "rec_1",
		Previous: automation.Resources{CPURequestMilli: 1000, MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 4 << 30},
		Desired:  automation.Resources{CPURequestMilli: 500, MemoryRequestBytes: 1 << 30, MemoryLimitBytes: 4 << 30}}
}

// recordResizes captures every pods/resize patch and lets the test decide
// the API answer.
func recordResizes(client *kubernetesfake.Clientset, answer func(pod string) error) *[]string {
	patched := &[]string{}
	client.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		if patch.GetSubresource() != "resize" {
			return true, nil, errors.New("pods must only be patched through the resize subresource")
		}
		*patched = append(*patched, patch.GetName()+" "+string(patch.GetPatch()))
		if answer != nil {
			if err := answer(patch.GetName()); err != nil {
				return true, nil, err
			}
		}
		return true, &corev1.Pod{}, nil
	})
	return patched
}

func TestResizeInPlaceResizesRunningPodsOfOnDeleteWorkloads(t *testing.T) {
	foreign := resizePod("other-0", "", corev1.PodRunning)
	foreign.OwnerReferences[0].UID = "someone-else"
	client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType),
		resizePod("db-0", "db", corev1.PodRunning), resizePod("db-1", "db", corev1.PodRunning), resizePod("db-2", "db", corev1.PodPending), foreign)
	patched := recordResizes(client, nil)
	result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
	// Only running pods of this workload count, and the first pass resizes
	// one of them.
	if err != nil || !result.Attempted || result.Pods != 2 || result.Resized != 1 || result.Failed != 0 || result.Remaining != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(*patched) != 1 || !strings.HasPrefix((*patched)[0], "db-1 ") {
		t.Fatalf("patched = %v", *patched)
	}
	var body struct {
		Spec struct {
			Containers []struct {
				Name      string
				Resources map[string]map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix((*patched)[0], "db-1 ")), &body); err != nil {
		t.Fatal(err)
	}
	container := body.Spec.Containers[0]
	if container.Name != "db" || container.Resources["requests"]["cpu"] != "500m" || container.Resources["requests"]["memory"] != "1024Mi" ||
		container.Resources["limits"]["memory"] != "4096Mi" || len(container.Resources["limits"]) != 1 {
		t.Fatalf("resize body = %+v", container)
	}
}

func TestResizeInPlaceLeavesPodsAloneWhenItDoesNotApply(t *testing.T) {
	restart := corev1.ContainerResizePolicy{ResourceName: corev1.ResourceMemory, RestartPolicy: corev1.RestartContainer}
	addLimit := resizeChange()
	addLimit.Desired.CPULimitMilli = 1000
	deployment := resizeChange()
	deployment.Kind = "Deployment"
	for name, test := range map[string]struct {
		workload *appsv1.StatefulSet
		change   automation.Change
		reason   string
	}{
		"rolling update":       {resizeStatefulSet(appsv1.RollingUpdateStatefulSetStrategyType), resizeChange(), "rolls the change out"},
		"deployment":           {resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), deployment, "rolls the change out"},
		"restart on resize":    {resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType, restart), resizeChange(), "resize policy restarts"},
		"limit would be added": {resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType), addLimit, "cannot add or remove a limit"},
	} {
		client := kubernetesfake.NewSimpleClientset(test.workload, resizePod("db-0", "db", corev1.PodRunning))
		patched := recordResizes(client, nil)
		result, err := testProvider(client).ResizeInPlace(context.Background(), test.change)
		if err != nil || result.Attempted || !strings.Contains(result.Reason, test.reason) || len(*patched) != 0 {
			t.Fatalf("%s: result=%+v err=%v patched=%v", name, result, err, *patched)
		}
	}
}

func TestResizeInPlaceReportsClusterAndPodFailures(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	workload := func() []runtime.Object {
		return []runtime.Object{resizeStatefulSet(appsv1.OnDeleteStatefulSetStrategyType),
			resizePod("db-0", "db", corev1.PodRunning), resizePod("db-1", "db", corev1.PodRunning)}
	}
	for name, test := range map[string]struct {
		answer                   func(pod string) error
		resized, failed, patches int
		message                  string
	}{
		"forbidden stops at the first pod":      {func(string) error { return apierrors.NewForbidden(pods, "db-1", errors.New("no")) }, 0, 2, 1, "permission is not granted"},
		"no subresource stops at the first pod": {func(string) error { return apierrors.NewNotFound(pods, "db-1") }, 0, 2, 1, "does not serve the pods/resize"},
		"one rejected pod does not stop the rest": {func(pod string) error {
			if pod == "db-1" {
				return apierrors.NewBadRequest("the resize would change the QoS class")
			}
			return nil
		}, 1, 1, 2, "pod db-1"},
	} {
		client := kubernetesfake.NewSimpleClientset(workload()...)
		patched := recordResizes(client, test.answer)
		result, err := testProvider(client).ResizeInPlace(context.Background(), resizeChange())
		if err != nil || !result.Attempted || result.Pods < 1 || result.Resized != test.resized || result.Failed != test.failed ||
			len(*patched) != test.patches || !strings.Contains(result.Error, test.message) {
			t.Fatalf("%s: result=%+v err=%v patched=%d", name, result, err, len(*patched))
		}
	}
}

func TestResizeInPlaceCoversOnDeleteDaemonSets(t *testing.T) {
	controller := true
	template := resizeTemplate()
	daemon := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop", UID: "ds-1"},
		Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: template.Labels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}, Template: template},
	}
	pod := resizePod("agent-x", "", corev1.PodRunning)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent", UID: "ds-1", Controller: &controller}}
	client := kubernetesfake.NewSimpleClientset(daemon, pod)
	patched := recordResizes(client, nil)
	change := resizeChange()
	change.Kind, change.Workload = "DaemonSet", "agent"
	result, err := testProvider(client).ResizeInPlace(context.Background(), change)
	if err != nil || result.Resized != 1 || len(*patched) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWorkloadReportsTheOnDeleteStrategy(t *testing.T) {
	for strategy, want := range map[appsv1.StatefulSetUpdateStrategyType]bool{appsv1.OnDeleteStatefulSetStrategyType: true, appsv1.RollingUpdateStatefulSetStrategyType: false} {
		client := kubernetesfake.NewSimpleClientset(resizeStatefulSet(strategy))
		spec, err := testProvider(client).Workload(context.Background(), "shop", "StatefulSet", "db")
		if err != nil || spec.OnDelete != want {
			t.Fatalf("%s: onDelete=%v err=%v", strategy, spec.OnDelete, err)
		}
	}
	daemon := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop", UID: "ds-1"},
		Spec: appsv1.DaemonSetSpec{UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}, Template: resizeTemplate()}}
	if spec, err := testProvider(kubernetesfake.NewSimpleClientset(daemon)).Workload(context.Background(), "shop", "DaemonSet", "agent"); err != nil || !spec.OnDelete {
		t.Fatalf("daemonset: onDelete=%v err=%v", spec.OnDelete, err)
	}
}
