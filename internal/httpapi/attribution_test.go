package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/usage"
)

type exactFakeUsage struct {
	*fakeUsage
	historyIdentity usage.WorkloadIdentity
	eventsIdentity  usage.WorkloadIdentity
}

func (source *exactFakeUsage) HistoryForIdentity(_ context.Context, subject usage.Subject, identity usage.WorkloadIdentity, window usage.Window) (usage.History, error) {
	source.historyIdentity = identity
	source.mu.Lock()
	source.windows = append(source.windows, window)
	source.mu.Unlock()
	history := source.history
	history.Subject = subject
	return history, source.err
}

func (source *exactFakeUsage) EventsForIdentity(_ context.Context, _ usage.Subject, identity usage.WorkloadIdentity, _ usage.Window) (usage.Events, error) {
	source.eventsIdentity = identity
	return source.events, nil
}

func recommendationIdentity(expected int32) *usage.WorkloadIdentity {
	created := fixedNow.Add(-10 * 24 * time.Hour)
	return &usage.WorkloadIdentity{
		UID: "uid-checkout", Generation: 12, CreatedAt: created, ExpectedReplicas: expected,
		Pods: []usage.PodIdentity{
			{Name: "checkout-current-aaaaa", UID: "pod-a", CreatedAt: created.Add(time.Hour), Phase: "Running", Containers: []string{"api"}},
			{Name: "checkout-current-bbbbb", UID: "pod-b", CreatedAt: created.Add(time.Hour), Phase: "Running", Containers: []string{"api"}},
		},
	}
}

func TestGenerateFailsClosedWhenExactMetricSourceIsUnavailable(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(3)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: healthyUsage(600)})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
	errorBody, _ := body["error"].(map[string]any)
	if response.Code != http.StatusUnprocessableEntity || errorBody["code"] != "metric_attribution_incomplete" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGenerateCarriesExactReplicaCoverageAndBlocksIncompleteReduction(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(3)
	base := healthyUsage(600)
	base.events = usage.Events{Available: true, Replicas: []usage.ReplicaEvents{
		{Pod: "checkout-current-aaaaa", UID: "pod-a", Restarts: 1},
		{Pod: "checkout-current-bbbbb", UID: "pod-b"},
	}}
	observation := func(pod, uid string) usage.ReplicaObservation {
		return usage.ReplicaObservation{
			Pod: pod, UID: uid, Phase: "Running", Active: true, CreatedAt: fixedNow.Add(-9 * 24 * time.Hour),
			CPU:    usage.Series{Resource: "cpu", Unit: "millicores", Quality: metrics.DataQuality{Coverage: 1}},
			Memory: usage.Series{Resource: "memory", Unit: "bytes", Quality: metrics.DataQuality{Coverage: 1}},
		}
	}
	base.history.ReplicaCoverage = &usage.ReplicaCoverage{
		ExpectedReplicas: 3, DiscoveredReplicas: 2, ObservedReplicas: 2, MissingReplicas: 1, ActiveReplicas: 2,
		CPUObservedReplicas: 2, MemoryObservedReplicas: 2, CPUReplicaCoverage: 2.0 / 3, MemoryReplicaCoverage: 2.0 / 3,
		CPUTemporalCoverage: 1, MemoryTemporalCoverage: 1, Complete: false,
		Issues: []string{"CPU metrics cover 2 of 3 expected replicas", "memory metrics cover 2 of 3 expected replicas"},
		Observations: []usage.ReplicaObservation{
			observation("checkout-current-aaaaa", "pod-a"), observation("checkout-current-bbbbb", "pod-b"),
		},
	}
	source := &exactFakeUsage{fakeUsage: base}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","dryRun":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	recommendation := body["recommendation"].(map[string]any)
	if recommendation["cpuRequestMilli"].(float64) < 1000 || recommendation["memoryRequestBytes"].(float64) < float64(1<<30) {
		t.Fatalf("incomplete replica metrics caused a reduction: %v", recommendation)
	}
	quality := recommendation["dataQuality"].(map[string]any)
	coverage := quality["replicaCoverage"].(map[string]any)
	if quality["sufficient"] != false || coverage["complete"] != false || coverage["missingReplicas"].(float64) != 1 || len(coverage["replicas"].([]any)) != 2 {
		t.Fatalf("replica evidence = %v", quality)
	}
	if body["replicaCoverage"].(map[string]any)["observedReplicas"].(float64) != 2 || len(source.historyIdentity.Pods) != 2 || source.eventsIdentity.UID != "uid-checkout" {
		t.Fatalf("exact identity was not used: history=%#v events=%#v body=%v", source.historyIdentity, source.eventsIdentity, body)
	}
}
