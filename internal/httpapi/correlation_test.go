package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/usage"
)

func linearSeries(resource string, start time.Time, step time.Duration, count int, value func(int) float64) *usage.Series {
	series := &usage.Series{Resource: resource, Quality: metrics.DataQuality{Issues: []metrics.QualityIssue{}, Coverage: 1}}
	for index := range count {
		series.Points = append(series.Points, usage.Point{Timestamp: start.Add(time.Duration(index) * step), Value: value(index)})
	}
	return series
}

func TestSignalsCorrelateThrottlingAndLatency(t *testing.T) {
	start := fixedNow.Add(-24 * time.Hour)
	step := 5 * time.Minute
	cpu := linearSeries("cpu", start, step, 288, func(index int) float64 { return 200 + float64(index%48)*10 })
	throttling := linearSeries("throttling", start, step, 288, func(index int) float64 { return float64(index%48) * 0.004 })
	latency := linearSeries("latency", start, step, 288, func(index int) float64 { return 0.1 + float64(index%48)*0.01 })
	memory := linearSeries("memory", start, step, 288, func(int) float64 { return 300 << 20 })
	source := healthyUsage(10)
	source.signals = &usage.Signals{
		CPU: *cpu, Memory: *memory, Throttling: throttling, Latency: latency,
		Events: []usage.Event{{Timestamp: start.Add(time.Hour), Kind: usage.EventOOMKill, Count: 2}},
	}
	api := newTestAPI(t, func(cfg *config.Config) { cfg.LatencyQuery = `p95{namespace="$namespace"}` }, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/metrics/signals?namespace=shop&kind=Deployment&workload=checkout&container=api", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	found := body["correlations"].(map[string]any)
	if found["cpuThrottling"].(float64) != 1 || found["throttlingLatency"].(float64) != 1 || found["cpuReplicas"] != nil {
		t.Fatalf("correlations=%v", found)
	}
	titles := []string{}
	for _, item := range body["insights"].([]any) {
		titles = append(titles, item.(map[string]any)["title"].(string))
	}
	joined := strings.Join(titles, "|")
	for _, want := range []string{"The CPU limit constrains peaks", "Throttling slows requests", "Latency follows CPU", "Containers were OOM-killed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing insight %q in %v", want, titles)
		}
	}
	if body["latencyConfigured"] != true || body["current"].(map[string]any)["memoryLimitBytes"].(float64) != 2<<30 {
		t.Fatalf("body=%v", body)
	}
}

func TestRolloutShiftDetectsStepChange(t *testing.T) {
	start := fixedNow.Add(-6 * time.Hour)
	rolloutAt := start.Add(3 * time.Hour)
	cpu := linearSeries("cpu", start, 5*time.Minute, 72, func(index int) float64 {
		if index >= 36 {
			return 500
		}
		return 200
	})
	at, change, ok := rolloutShift(*cpu, []usage.Event{{Timestamp: rolloutAt, Kind: usage.EventRollout}, {Timestamp: start, Kind: usage.EventRestart}})
	if !ok || !at.Equal(rolloutAt) || change != 1.5 {
		t.Fatalf("at=%v change=%v ok=%v", at, change, ok)
	}
}

func TestGenerateUsesOnlyPostRolloutData(t *testing.T) {
	step := 5 * time.Minute
	start := fixedNow.Add(-time.Duration(600) * step)
	rolloutAt := start.Add(300 * step)
	cpu := linearSeries("cpu", start, step, 600, func(index int) float64 {
		if index >= 300 {
			return 300 + float64(index%5)
		}
		return 900 + float64(index%5)
	})
	memory := linearSeries("memory", start, step, 600, func(int) float64 { return 256 << 20 })
	source := &fakeUsage{history: usage.History{Start: start, End: fixedNow, CPU: *cpu, Memory: *memory}, events: usage.Events{Available: true}, rollouts: []usage.Event{{Timestamp: rolloutAt, Kind: usage.EventRollout}}}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","dryRun":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	recommendation := body["recommendation"].(map[string]any)
	stats := recommendation["cpuStatisticsMilli"].(map[string]any)
	if stats["max"].(float64) > 310 || recommendation["dataQuality"].(map[string]any)["cpuSampleCount"].(float64) != 300 {
		t.Fatalf("pre-rollout samples were used: %v", stats)
	}
	if !strings.Contains(strings.Join(toStrings(recommendation["warnings"]), "|"), "after the rollout") {
		t.Fatalf("warnings=%v", recommendation["warnings"])
	}
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (notifier *recordingNotifier) Notify(event notify.Event) {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	notifier.events = append(notifier.events, event)
}

func TestLifecycleEventsAreNotified(t *testing.T) {
	notifier := &recordingNotifier{}
	api := newTestAPI(t, nil, Dependencies{Notifier: notifier})
	id := createRecommendation(t, api, "alice")
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations/"+id+"/review", "bob", `{"decision":"reject","comment":"peak season"}`); response.Code != http.StatusOK {
		t.Fatalf("review status=%d", response.Code)
	}
	if len(notifier.events) != 2 || notifier.events[0].Type != notify.RecommendationCreated || notifier.events[1].Type != notify.RecommendationRejected ||
		notifier.events[1].Actor != "bob" || notifier.events[1].Comment != "peak season" || notifier.events[1].RecommendationID != id {
		t.Fatalf("events=%#v", notifier.events)
	}
}

func TestWorkloadEfficiencyAttributesPodsToWorkloads(t *testing.T) {
	records := []map[string]any{
		{"namespace": "shop", "kind": "Deployment", "name": "api", "desiredReplicas": int32(2), "cpuRequestMilli": int64(1000), "memoryRequestBytes": int64(1 << 30),
			"containers": []map[string]any{{"cpuRequestMilli": int64(1000), "memoryRequestBytes": int64(1 << 30)}}},
		{"namespace": "shop", "kind": "Deployment", "name": "api-v2", "desiredReplicas": int32(1), "cpuRequestMilli": int64(500), "memoryRequestBytes": int64(1 << 29),
			"containers": []map[string]any{{"cpuRequestMilli": int64(500), "memoryRequestBytes": int64(0)}}},
	}
	source := healthyUsage(10)
	source.pods = map[string]usage.PodUsage{
		"api-7d9f8c6b5-aaaaa":    {CPUAverageMilli: 200, CPUP95Milli: 300, MemoryAverageBytes: 300 << 20, MemoryP95Bytes: 400 << 20},
		"api-7d9f8c6b5-bbbbb":    {CPUAverageMilli: 400, CPUP95Milli: 600, MemoryAverageBytes: 500 << 20, MemoryP95Bytes: 600 << 20},
		"api-v2-6c8d7b9f4-ccccc": {CPUAverageMilli: 450, CPUP95Milli: 480, MemoryAverageBytes: 100 << 20, MemoryP95Bytes: 120 << 20},
		"stray-pod":              {CPUAverageMilli: 1},
	}
	api := newTestAPI(t, nil, Dependencies{Provider: inventoryRecords{records: records}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/efficiency/workloads?namespace=shop", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	rows := map[string]map[string]any{}
	for _, item := range body["items"].([]any) {
		row := item.(map[string]any)
		rows[row["name"].(string)] = row
	}
	api1 := rows["api"]
	// Busiest pod P95 is 600m; (1000 - 600) x 2 replicas = 800m reclaimable.
	if api1["podsObserved"].(float64) != 2 || api1["cpuP95PerPodMilli"].(float64) != 600 || api1["cpuAveragePerPodMilli"].(float64) != 300 || api1["cpuReclaimableMilli"].(float64) != 800 {
		t.Fatalf("api=%v", api1)
	}
	if rows["api-v2"]["podsObserved"].(float64) != 1 || rows["api-v2"]["containersWithoutRequests"].(float64) != 1 {
		t.Fatalf("api-v2=%v", rows["api-v2"])
	}
	if body["summary"].(map[string]any)["podsUnmatched"].(float64) != 1 {
		t.Fatalf("summary=%v", body["summary"])
	}
	if response, _ := api.do(t, http.MethodGet, "/api/v1/efficiency/workloads?namespace=Bad", "alice", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid namespace status=%d", response.Code)
	}
}

func TestPackCountsNodesAndReservesDaemonSets(t *testing.T) {
	nodes := []NodeCapacity{
		{Name: "a", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true},
		{Name: "b", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true},
		{Name: "c", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true},
		{Name: "cordoned", CPUMilli: 64000, MemoryBytes: 256 << 30, Schedulable: false},
	}
	pods := []podDemand{}
	for range 6 {
		pods = append(pods, podDemand{cpuMilli: 1500, memoryBytes: 1 << 30})
	}
	daemon := podDemand{cpuMilli: 500, memoryBytes: 1 << 30}
	// 3500m free per node fits two 1500m pods, so six pods need three nodes.
	if result := pack(nodes, daemon, pods); result.NodesNeeded != 3 || result.UnplacedPods != 0 {
		t.Fatalf("current=%#v", result)
	}
	smaller := []podDemand{}
	for range 6 {
		smaller = append(smaller, podDemand{cpuMilli: 800, memoryBytes: 1 << 30})
	}
	if result := pack(nodes, daemon, smaller); result.NodesNeeded != 2 {
		t.Fatalf("recommended=%#v", result)
	}
	if result := pack(nodes, daemon, []podDemand{{cpuMilli: 9000}}); result.UnplacedPods != 1 || result.NodesNeeded != 0 {
		t.Fatalf("oversized=%#v", result)
	}
}

type capacityInventory struct {
	inventoryRecords
	cluster ClusterCapacity
}

func (inventory capacityInventory) Capacity(context.Context) (ClusterCapacity, error) {
	return inventory.cluster, nil
}

func TestCapacityShowsNodeImpactAndKarpenterRealization(t *testing.T) {
	records := []map[string]any{
		{"namespace": "shop", "kind": "Deployment", "name": "api", "desiredReplicas": int32(6),
			"containers": []map[string]any{{"name": "app", "cpuRequestMilli": int64(1500), "memoryRequestBytes": int64(1 << 30)}}},
	}
	cluster := ClusterCapacity{
		Nodes: []NodeCapacity{
			{Name: "a", CPUMilli: 3500, MemoryBytes: 16 << 30, Schedulable: true},
			{Name: "b", CPUMilli: 3500, MemoryBytes: 16 << 30, Schedulable: true},
			{Name: "c", CPUMilli: 3500, MemoryBytes: 16 << 30, Schedulable: true},
		},
		Autoscaling: NodeAutoscaling{Karpenter: true, NodePools: []NodePool{{Name: "default", ConsolidationPolicy: "WhenEmptyOrUnderutilized"}}},
	}
	api := newTestAPI(t, nil, Dependencies{Provider: capacityInventory{inventoryRecords: inventoryRecords{records: records}, cluster: cluster}})
	recommendation := `{"namespace":"shop","kind":"Deployment","workload":"api","container":"app","cpuRequestMilli":1500,"memoryRequestBytes":1073741824,"cpuSamplesMilli":` + repeatedSamples(600, 500) + `,"memorySamplesBytes":` + repeatedSamples(600, 300000000) + `,"sampleIntervalSeconds":300,"policy":{"cpuSafetyMargin":0.3,"memorySafetyMargin":0.25,"limitSafetyMargin":0.5,"minimumCpuMilli":50,"minimumMemoryBytes":67108864,"maximumCpuMilli":1000000000,"maximumMemoryBytes":2305843009213693951,"maximumReduction":0.5,"minimumSamples":30,"highConfidenceSamples":500}}`
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations", "alice", recommendation); response.Code != http.StatusCreated {
		t.Fatalf("seed status=%d", response.Code)
	}
	response, body := api.do(t, http.MethodGet, "/api/v1/capacity", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current := body["current"].(map[string]any)
	recommended := body["recommended"].(map[string]any)
	// Six 1500m pods need three 3500m nodes; six 750m pods fit on two.
	if current["nodesNeeded"].(float64) != 3 || recommended["nodesNeeded"].(float64) != 2 || body["reclaimableNodes"].(float64) != 1 {
		t.Fatalf("current=%v recommended=%v", current, recommended)
	}
	if !strings.Contains(body["realization"].(string), "Karpenter consolidates") || body["containersWithRecommendations"].(float64) != 1 {
		t.Fatalf("body=%v", body)
	}
	if _, unavailable := newTestAPI(t, nil, Dependencies{}).do(t, http.MethodGet, "/api/v1/capacity", "alice", ""); unavailable["status"].(map[string]any)["available"] != false {
		t.Fatalf("unconfigured capacity=%v", unavailable)
	}
}

func repeatedSamples(count int, value int) string {
	parts := make([]string, count)
	for index := range parts {
		parts[index] = strconv.Itoa(value + index%5)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestRealizationExplainsEachAutoscaler(t *testing.T) {
	cases := map[string]NodeAutoscaling{
		"only empty nodes":   {Karpenter: true, NodePools: []NodePool{{ConsolidationPolicy: "WhenEmpty"}}},
		"Cluster Autoscaler": {ClusterAutoscaler: true},
		"No node autoscaler": {},
	}
	for want, autoscaling := range cases {
		if text := realization(autoscaling); !strings.Contains(text, want) {
			t.Fatalf("realization(%#v) = %q, want %q", autoscaling, text, want)
		}
	}
}

type countingNotifier struct{ recordingNotifier }

func (*countingNotifier) Stats() (uint64, uint64, uint64) { return 4, 1, 0 }

func TestMetricsExportRecommendationsWhenEnabled(t *testing.T) {
	scrape := func(api testAPI) string {
		response, _ := api.do(t, http.MethodGet, "/metrics", "", "")
		return response.Body.String()
	}
	disabled := newTestAPI(t, nil, Dependencies{})
	createRecommendation(t, disabled, "alice")
	if strings.Contains(scrape(disabled), "kuberight_recommendation_cpu_request_millicores") {
		t.Fatal("recommendation metrics must be opt-in")
	}
	enabled := newTestAPI(t, func(cfg *config.Config) {
		cfg.ExportRecommendationMetrics = true
		cfg.CostCPUCoreHour, cfg.CostMemoryGiBHour = 0.04, 0.005
	}, Dependencies{Notifier: &countingNotifier{}})
	createRecommendation(t, enabled, "alice")
	body := scrape(enabled)
	for _, want := range []string{
		`kuberight_recommendation_cpu_request_millicores{namespace="shop",kind="",workload="api",container="app",state="current"} 500`,
		`kuberight_recommendation_memory_request_bytes{namespace="shop",kind="",workload="api",container="app",state="current"} 536870912`,
		`kuberight_recommendation_info{namespace="shop",kind="",workload="api",container="app",review_status="pending",risk="high",confidence="low",source="request"} 1`,
		`kuberight_recommendation_monthly_savings{namespace="shop"`,
		`kuberight_webhook_events_total{outcome="delivered"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "kuberight_recommendation_cpu_limit_millicores{") {
		t.Fatal("unset limits must be absent, not zero")
	}
}
