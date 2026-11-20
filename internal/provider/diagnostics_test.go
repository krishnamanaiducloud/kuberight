package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/httpapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func diagnosticPod(name string, mutate func(*corev1.Pod)) *corev1.Pod {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", Labels: map[string]string{"pod-template-hash": "7d9f8"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "checkout-7d9f8", Controller: &controller}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	mutate(pod)
	return pod
}

func TestDiagnosticsReportObservedWorkloadProblems(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	recent := metav1.NewTime(time.Now().Add(-time.Minute))
	client := kubernetesfake.NewSimpleClientset(
		diagnosticPod("healthy", func(*corev1.Pod) {}),
		diagnosticPod("crashing", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", RestartCount: 7,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: old}},
			}}
		}),
		diagnosticPod("pulling", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
		}),
		diagnosticPod("restarting", func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", RestartCount: 6,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}}
		}),
		diagnosticPod("pending", func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/3 nodes are available: 3 Insufficient memory.", LastTransitionTime: old}}
		}),
		diagnosticPod("evicted", func(pod *corev1.Pod) {
			pod.Status.Phase, pod.Status.Reason, pod.Status.Message = corev1.PodFailed, "Evicted", "The node was low on resource: memory."
		}),
		diagnosticPod("unready", func(pod *corev1.Pod) {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: old}}
		}),
		diagnosticPod("starting", func(pod *corev1.Pod) {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: recent}}
		}),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady", Message: "container runtime is down", LastTransitionTime: old},
		}}},
	)
	result, err := testProvider(client).List(context.Background(), "diagnostics", httpapi.CollectionQuery{Page: 1, PageSize: 50})
	if err != nil || !result.Status.Available {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	items := result.Items.([]map[string]any)
	found := map[string]map[string]any{}
	for _, item := range items {
		if item["kind"] == "Node" {
			found["node/"+item["category"].(string)] = item
		} else {
			found[item["pod"].(string)+"/"+item["category"].(string)] = item
		}
		if item["evidence"] != "observed" {
			t.Fatalf("finding is not labelled observed: %v", item)
		}
	}
	for key, severity := range map[string]string{
		"crashing/CrashLoopBackOff": "critical", "crashing/OOMKilled": "critical", "pulling/ImagePullBackOff": "critical",
		"restarting/RepeatedRestarts": "warning", "pending/InsufficientMemory": "critical", "evicted/Evicted": "warning",
		"unready/NotReady": "warning", "node/NodeNotReady": "critical",
	} {
		if found[key] == nil || found[key]["severity"] != severity {
			t.Fatalf("%s: %v (all: %v)", key, found[key], items)
		}
	}
	if len(items) != 8 {
		t.Fatalf("a healthy or still-starting pod was reported, or a finding is missing: %d items", len(items))
	}
	oom := found["crashing/OOMKilled"]
	if oom["workload"] != "checkout" || oom["kind"] != "Deployment" || oom["restarts"] != int32(7) || !strings.Contains(oom["message"].(string), "256Mi") || !strings.Contains(oom["message"].(string), "exit code 137") {
		t.Fatalf("OOM finding = %v", oom)
	}
	// Critical findings come first.
	if items[0]["severity"] != "critical" || items[len(items)-1]["severity"] != "warning" {
		t.Fatalf("order: first=%v last=%v", items[0]["severity"], items[len(items)-1]["severity"])
	}
}

func TestDiagnosticOwnerKeepsNonDeploymentReplicaSets(t *testing.T) {
	controller := true
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "web", Controller: &controller}}}}
	if kind, name := diagnosticOwner(pod); kind != "StatefulSet" || name != "web" {
		t.Fatalf("owner = %s/%s", kind, name)
	}
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "standalone", Controller: &controller}}
	if kind, name := diagnosticOwner(pod); kind != "ReplicaSet" || name != "standalone" {
		t.Fatalf("a ReplicaSet without a template hash = %s/%s", kind, name)
	}
	pod.OwnerReferences = nil
	if kind, name := diagnosticOwner(pod); kind != "Pod" || name != "web-0" {
		t.Fatalf("a bare pod = %s/%s", kind, name)
	}
	pod.Name = "kube-apiserver-node-a"
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Node", Name: "node-a", Controller: &controller}}
	if kind, name := diagnosticOwner(pod); kind != "Pod" || name != "kube-apiserver-node-a" {
		t.Fatalf("a static pod's mirror = %s/%s", kind, name)
	}
}

func TestHealthSummaryCountsWorkloadsWithFindings(t *testing.T) {
	controller := true
	statefulPod := func(name string, restarts int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "data", OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &controller}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "db", RestartCount: restarts,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}},
		}
	}
	healthyDeployment := diagnosticPod("checkout-1", func(*corev1.Pod) {})
	crashing := diagnosticPod("checkout-2", func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "api", RestartCount: 9,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	})
	other := diagnosticPod("web-1", func(pod *corev1.Pod) {
		pod.Labels["pod-template-hash"] = "abc12"
		pod.OwnerReferences[0].Name = "web-abc12"
	})
	// A static pod's mirror is owned by its Node; its restarts are a pod
	// problem, not a node problem.
	mirror := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-node-a", Namespace: "kube-system", OwnerReferences: []metav1.OwnerReference{{Kind: "Node", Name: "node-a", Controller: &controller}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "etcd", RestartCount: 12}}},
	}
	client := kubernetesfake.NewSimpleClientset(healthyDeployment, crashing, other, statefulPod("db-0", 2), statefulPod("db-1", 3), mirror,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
		}}})
	summary, err := testProvider(client).HealthSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Workloads: Deployment checkout (one crashing pod), Deployment web,
	// StatefulSet db (two OOM kills), and the restarting static pod.
	if !summary.Available || summary.Pods != 6 || summary.WorkloadsWithPods != 4 || summary.WorkloadsNeedingAttention != 3 || summary.HealthyWorkloads != 1 || summary.NodesWithProblems != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Findings["OOMKilled"] != 2 || summary.Findings["CrashLoopBackOff"] != 1 || summary.Findings["RepeatedRestarts"] != 2 || summary.Findings["NodeMemoryPressure"] != 1 {
		t.Fatalf("findings = %v", summary.Findings)
	}
}
