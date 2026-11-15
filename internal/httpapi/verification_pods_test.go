package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

// podResources marks every pod of the verification workload as running the
// given values for the "api" container.
func podResources(spec *WorkloadSpec, resources usage.ContainerResources) {
	for index := range spec.MetricIdentity.Pods {
		spec.MetricIdentity.Pods[index].Resources = map[string]usage.ContainerResources{"api": resources}
	}
}

func TestVerificationWaitsUntilLivePodsRunTheApprovedResources(t *testing.T) {
	target := verificationSpec().Containers[0]
	approved := usage.ContainerResources{CPURequestMilli: target.CPURequestMilli, CPULimitMilli: target.CPULimitMilli,
		MemoryRequestBytes: target.MemoryRequestBytes, MemoryLimitBytes: target.MemoryLimitBytes}
	previous := usage.ContainerResources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30}

	for name, mutate := range map[string]func(*WorkloadSpec){
		// An OnDelete workload, or a rollout that has not replaced this pod.
		"one pod still runs the previous values": func(spec *WorkloadSpec) {
			spec.MetricIdentity.Pods[1].Resources = map[string]usage.ContainerResources{"api": previous}
		},
		// The pod spec was resized in place but the kubelet has not applied it.
		"one pod has an unfinished in-place resize": func(spec *WorkloadSpec) { spec.MetricIdentity.Pods[1].Resizing = true },
	} {
		spec := verificationSpec()
		podResources(&spec, approved)
		mutate(&spec)
		source := verificationUsage()
		api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
		record := storeApproved(t, api, liveRecommendation(), fixedNow.Add(-5*time.Hour))
		response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/verification:refresh", "alice", "")
		reasons := strings.Join(stringsOf(body["reasons"]), "; ")
		if response.Code != http.StatusOK || body["status"] != VerificationNotApplied || body["configurationObserved"] == true || body["firstObservedAt"] != nil ||
			!strings.Contains(reasons, "1 of 2 live pods do not run them yet") {
			t.Fatalf("%s: status=%d body=%v", name, response.Code, body)
		}
		if source.historyIdentity.UID != "" || source.eventsIdentity.UID != "" {
			t.Fatalf("%s: evidence was read from pods that do not run the change", name)
		}
		outcome, err := api.store.GetVerificationOutcome(context.Background(), record.ID)
		if err != nil || outcome.FirstObservedAt != nil {
			t.Fatalf("%s: the post-change window started early: outcome=%+v err=%v", name, outcome, err)
		}
	}

	// Once every live pod runs the approved values the window starts.
	spec := verificationSpec()
	podResources(&spec, approved)
	// A terminating pod from the previous revision does not hold it back.
	deleted := fixedNow.Add(-time.Minute)
	spec.MetricIdentity.Pods = append(spec.MetricIdentity.Pods, usage.PodIdentity{Name: "checkout-old-ccccc", UID: "pod-c", CreatedAt: fixedNow.Add(-48 * time.Hour),
		DeletedAt: &deleted, Phase: "Running", Containers: []string{"api"}, Resources: map[string]usage.ContainerResources{"api": previous}})
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: verificationUsage()})
	record := storeApproved(t, api, liveRecommendation(), fixedNow.Add(-5*time.Hour))
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/verification:refresh", "alice", "")
	if response.Code != http.StatusOK || body["status"] != VerificationPending || body["configurationObserved"] != true || body["firstObservedAt"] == nil {
		t.Fatalf("all pods on target: status=%d body=%v", response.Code, body)
	}
}

func TestPodsBehindTargetIgnoresUnreportedAndInactivePods(t *testing.T) {
	record := storeApproved(t, newTestAPI(t, nil, Dependencies{}), liveRecommendation(), fixedNow)
	other := usage.ContainerResources{CPURequestMilli: 1}
	identity := usage.WorkloadIdentity{Pods: []usage.PodIdentity{
		{Name: "unreported", Phase: "Running"},
		{Name: "failed", Phase: "Failed", Resources: map[string]usage.ContainerResources{"api": other}},
		{Name: "missing-container", Phase: "Running", Resources: map[string]usage.ContainerResources{"sidecar": other}},
	}}
	if behind, reported := podsBehindTarget(identity, record); behind != 1 || reported != 1 {
		t.Fatalf("behind=%d reported=%d, want only the pod without the container counted", behind, reported)
	}
}
