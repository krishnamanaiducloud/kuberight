package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/forecast"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

var fixedNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// recordingAccess authenticates the bearer token as the username and records
// every authorization request.
type recordingAccess struct {
	mu    sync.Mutex
	calls []AccessAttributes
	deny  func(AccessAttributes) bool
}

func (access *recordingAccess) Authenticate(_ context.Context, token string) (Principal, error) {
	return Principal{Username: token}, nil
}

func (access *recordingAccess) Authorize(_ context.Context, _ Principal, attributes AccessAttributes) error {
	access.mu.Lock()
	access.calls = append(access.calls, attributes)
	access.mu.Unlock()
	if access.deny != nil && access.deny(attributes) {
		return newAccessError(http.StatusForbidden, "forbidden", "denied")
	}
	return nil
}

func (access *recordingAccess) Ready(context.Context) error { return nil }

type fakeInventory struct {
	spec WorkloadSpec
	err  error
}

func (inventory fakeInventory) List(context.Context, string, CollectionQuery) (CollectionResult, error) {
	return CollectionResult{Items: []map[string]any{{"name": "x"}}, Total: 3, Status: Availability{Available: true, Source: "kubernetes-api"}}, nil
}
func (inventory fakeInventory) Ready(context.Context) error { return nil }
func (inventory fakeInventory) Workload(_ context.Context, namespace, kind, name string) (WorkloadSpec, error) {
	if inventory.err != nil {
		return WorkloadSpec{}, inventory.err
	}
	if namespace != inventory.spec.Namespace || kind != inventory.spec.Kind || name != inventory.spec.Name {
		return WorkloadSpec{}, ErrWorkloadNotFound
	}
	return inventory.spec, nil
}

type fakeUsage struct {
	hotspots   []usage.ContainerHotspot
	pods       map[string]usage.PodUsage
	signals    *usage.Signals
	rollouts   []usage.Event
	namespaces map[string]usage.NamespaceUsage
	history    usage.History
	err        error
	events     usage.Events
	ready      error
	windows    []usage.Window
	mu         sync.Mutex
}

func (source *fakeUsage) History(_ context.Context, subject usage.Subject, window usage.Window) (usage.History, error) {
	source.mu.Lock()
	source.windows = append(source.windows, window)
	source.mu.Unlock()
	history := source.history
	history.Subject = subject
	return history, source.err
}
func (source *fakeUsage) Events(context.Context, usage.Subject, usage.Window) (usage.Events, error) {
	return source.events, nil
}
func (source *fakeUsage) NamespaceUsage(_ context.Context, namespaces []string, _ time.Duration, _ time.Time) (map[string]usage.NamespaceUsage, error) {
	result := map[string]usage.NamespaceUsage{}
	for _, namespace := range namespaces {
		if value, ok := source.namespaces[namespace]; ok {
			result[namespace] = value
		}
	}
	return result, source.err
}
func (source *fakeUsage) PodUsage(context.Context, string, time.Duration, time.Time) (map[string]usage.PodUsage, error) {
	return source.pods, source.err
}
func (source *fakeUsage) Signals(_ context.Context, subject usage.Subject, _ usage.Window, _ usage.LatencyTemplate) (usage.Signals, error) {
	if source.signals == nil {
		return usage.Signals{}, usage.ErrNoData
	}
	signals := *source.signals
	signals.Subject = subject
	return signals, source.err
}
func (source *fakeUsage) Rollouts(context.Context, usage.Subject, time.Time, time.Time) ([]usage.Event, error) {
	return source.rollouts, nil
}
func (source *fakeUsage) Hotspots(context.Context, []string, time.Duration, time.Time) ([]usage.ContainerHotspot, error) {
	return source.hotspots, source.err
}
func (source *fakeUsage) Ready(context.Context) error { return source.ready }

type fakeForecaster struct {
	request forecast.Request
	err     error
}

func (forecaster *fakeForecaster) Forecast(_ context.Context, request forecast.Request) (forecast.Response, error) {
	forecaster.request = request
	if forecaster.err != nil {
		return forecast.Response{}, forecaster.err
	}
	response := forecast.Response{Model: "test-model", Confidence: 0.7}
	for index := range request.Horizon {
		value := request.Samples[len(request.Samples)-1] + float64(index)
		response.Predictions = append(response.Predictions, value)
		response.ConfidenceIntervals = append(response.ConfidenceIntervals, forecast.Interval{Lower: value - 0.1, Upper: value + 0.1})
	}
	return response, nil
}
func (forecaster *fakeForecaster) Ready(context.Context) error { return nil }

func checkoutSpec() WorkloadSpec {
	return WorkloadSpec{
		Cluster: "prod", Namespace: "shop", Kind: "Deployment", Name: "checkout", UID: "uid-checkout", Generation: 12,
		ResourceVersion: "38491", SourceSpecFingerprint: strings.Repeat("a", 64), Replicas: 3,
		Containers: []ContainerSpec{{Name: "api", CPURequestMilli: 1000, CPULimitMilli: 2000, MemoryRequestBytes: 1 << 30, MemoryLimitBytes: 2 << 30}},
		Warnings:   []string{},
	}
}

func steadySeries(resource, unit string, count int, value float64, step time.Duration) usage.Series {
	series := usage.Series{Resource: resource, Unit: unit, Quality: metrics.DataQuality{Issues: []metrics.QualityIssue{}, Coverage: 1}}
	start := fixedNow.Add(-time.Duration(count) * step)
	for index := range count {
		series.Points = append(series.Points, usage.Point{Timestamp: start.Add(time.Duration(index) * step), Value: value + float64(index%5)})
	}
	return series
}

func healthyUsage(count int) *fakeUsage {
	return &fakeUsage{
		history: usage.History{
			CPU:    steadySeries("cpu", "millicores", count, 200, 5*time.Minute),
			Memory: steadySeries("memory", "bytes", count, 256<<20, 5*time.Minute),
		},
		events: usage.Events{Available: true},
	}
}

type testAPI struct {
	handler http.Handler
	access  *recordingAccess
	store   *repository.Memory
}

func newTestAPI(t *testing.T, mutate func(*config.Config), dependencies Dependencies) testAPI {
	t.Helper()
	cfg := fallbackConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	access := &recordingAccess{}
	store := repository.NewMemory(1000)
	dependencies.Access = access
	dependencies.Repository = store
	dependencies.Now = func() time.Time { return fixedNow }
	handler, err := NewHandler(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	return testAPI{handler: handler, access: access, store: store}
}

func (api testAPI) do(t *testing.T, method, path, identity, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+identity)
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response, decoded
}

const generateBody = `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api"}`

func TestGenerateRecommendationFromPrometheusAndInventory(t *testing.T) {
	source := healthyUsage(600)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body["source"] != repository.SourcePrometheus || body["createdBy"] != "alice" || body["reviewStatus"] != repository.ReviewPending {
		t.Fatalf("unexpected provenance: %v", body)
	}
	if body["currentCpuRequestMilli"].(float64) != 1000 || body["replicas"].(float64) != 3 || body["cluster"] != "prod" {
		t.Fatalf("live workload settings were not used: %v", body)
	}
	// 600 healthy samples justify the maximum 20 percent reduction.
	if body["cpuRequestMilli"].(float64) != 800 || body["confidence"] != "high" {
		t.Fatalf("cpu=%v confidence=%v", body["cpuRequestMilli"], body["confidence"])
	}
	if len(source.windows) != 1 || source.windows[0].Duration != config.DefaultRecommendationWindow || source.windows[0].Step != config.DefaultRecommendationStep || !source.windows[0].End.Equal(fixedNow) {
		t.Fatalf("unexpected query window: %#v", source.windows)
	}
	audit, _ := api.store.ListAuditEvents(context.Background(), 1, 10)
	if audit.Total != 1 || audit.Items[0].Action != "create" {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestGenerateRecommendationBlocksReductionsOnPoorData(t *testing.T) {
	source := healthyUsage(600)
	source.history.CPU.Quality = metrics.DataQuality{Issues: []metrics.QualityIssue{metrics.IssueSparse}, Coverage: 0.4}
	source.events = usage.Events{Available: false}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body["cpuRequestMilli"].(float64) < 1000 || body["confidence"] != "low" {
		t.Fatalf("sparse data must not reduce requests: %v", body)
	}
	warnings := strings.Join(toStrings(body["warnings"]), "|")
	if !strings.Contains(warnings, "kube-state-metrics") {
		t.Fatalf("missing event-availability warning: %s", warnings)
	}
}

func toStrings(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.(string))
	}
	return result
}

func TestGenerateRecommendationFailureModes(t *testing.T) {
	cases := map[string]struct {
		dependencies Dependencies
		body         string
		mutate       func(*config.Config)
		status       int
		code         string
	}{
		"no prometheus":      {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}}, generateBody, nil, http.StatusServiceUnavailable, "integration_not_configured"},
		"no inventory":       {Dependencies{Usage: healthyUsage(10)}, generateBody, nil, http.StatusServiceUnavailable, "integration_not_configured"},
		"invalid subject":    {Dependencies{Usage: healthyUsage(10)}, `{"namespace":"shop","kind":"Deployment","workload":"Checkout","container":"api"}`, nil, http.StatusBadRequest, "invalid_request"},
		"invalid window":     {Dependencies{Usage: healthyUsage(10)}, `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","window":"10m"}`, nil, http.StatusBadRequest, "invalid_request"},
		"missing workload":   {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: healthyUsage(10)}, `{"namespace":"shop","kind":"Deployment","workload":"other","container":"api"}`, nil, http.StatusNotFound, "workload_not_found"},
		"missing container":  {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: healthyUsage(10)}, `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"sidecar"}`, nil, http.StatusNotFound, "container_not_found"},
		"out of scope":       {Dependencies{Provider: fakeInventory{err: ErrWorkloadOutOfScope}, Usage: healthyUsage(10)}, generateBody, nil, http.StatusUnprocessableEntity, "workload_out_of_scope"},
		"no usage data":      {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: &fakeUsage{err: usage.ErrNoData}}, generateBody, nil, http.StatusUnprocessableEntity, "no_usage_data"},
		"prometheus down":    {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: &fakeUsage{err: metrics.ErrUpstream}}, generateBody, nil, http.StatusServiceUnavailable, "dependency_unavailable"},
		"observe mode":       {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: healthyUsage(10)}, generateBody, func(cfg *config.Config) { cfg.OperationMode = config.ModeObserve }, http.StatusForbidden, "operation_disabled"},
		"unknown field":      {Dependencies{Usage: healthyUsage(10)}, `{"namespace":"shop","replicas":2}`, nil, http.StatusBadRequest, "invalid_request"},
		"inventory failure":  {Dependencies{Provider: fakeInventory{err: errors.New("boom")}, Usage: healthyUsage(10)}, generateBody, nil, http.StatusServiceUnavailable, "dependency_unavailable"},
		"unsupported policy": {Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: healthyUsage(10)}, `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","policy":{"cpuSafetyMargin":99}}`, nil, http.StatusUnprocessableEntity, "invalid_recommendation_input"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			api := newTestAPI(t, test.mutate, test.dependencies)
			response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", test.body)
			problem, _ := body["error"].(map[string]any)
			if response.Code != test.status || problem["code"] != test.code {
				t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), test.status, test.code)
			}
		})
	}
}

// createRecommendation stores a recommendation calculated from enough steady
// samples to pass every validation, so it can be approved.
func createRecommendation(t *testing.T, api testAPI, identity string) string {
	t.Helper()
	cpu, memory := make([]string, 120), make([]string, 120)
	for index := range cpu {
		cpu[index] = strconv.Itoa(100 + index%10)
		memory[index] = strconv.Itoa(200_000_000 + (index%10)*1_000_000)
	}
	payload := `{"namespace":"shop","workload":"api","container":"app","cpuRequestMilli":500,"memoryRequestBytes":536870912,"sampleIntervalSeconds":300,"cpuSamplesMilli":[` +
		strings.Join(cpu, ",") + `],"memorySamplesBytes":[` + strings.Join(memory, ",") + `]}`
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations", identity, payload)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	return body["id"].(string)
}

func TestUnsafeRecommendationsCannotBeApproved(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations", "alice", `{"namespace":"shop","workload":"api","container":"app","cpuRequestMilli":500,"memoryRequestBytes":536870912,"cpuSamplesMilli":[100,120],"memorySamplesBytes":[200000000,210000000]}`)
	if response.Code != http.StatusCreated || body["assessment"].(map[string]any)["status"] != "insufficient_data" {
		t.Fatalf("create status=%d body=%v", response.Code, body)
	}
	path := "/api/v1/recommendations/" + body["id"].(string) + "/review"
	response, body = api.do(t, http.MethodPost, path, "bob", `{"decision":"approve"}`)
	if response.Code != http.StatusConflict || body["error"].(map[string]any)["code"] != "recommendation_not_approvable" {
		t.Fatalf("approving insufficient data: status=%d body=%v", response.Code, body)
	}
	if response, _ := api.do(t, http.MethodPost, path, "bob", `{"decision":"reject","comment":"not enough history"}`); response.Code != http.StatusOK {
		t.Fatalf("rejecting must stay possible: status=%d", response.Code)
	}
}

func TestReviewWorkflowEnforcesSeparationOfDuties(t *testing.T) {
	api := newTestAPI(t, func(cfg *config.Config) { cfg.RequireDistinctApprover = true }, Dependencies{})
	id := createRecommendation(t, api, "alice")
	path := "/api/v1/recommendations/" + id + "/review"

	if response, body := api.do(t, http.MethodPost, path, "alice", `{"decision":"approve"}`); response.Code != http.StatusForbidden || body["error"].(map[string]any)["code"] != "separation_of_duties" {
		t.Fatalf("self-approval status=%d body=%v", response.Code, body)
	}
	if response, _ := api.do(t, http.MethodPost, path, "bob", `{"decision":"reject"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("rejection without comment status=%d", response.Code)
	}
	if response, _ := api.do(t, http.MethodPost, path, "bob", `{"decision":"apply"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown decision status=%d", response.Code)
	}

	pending, pendingBody := api.do(t, http.MethodGet, "/api/v1/approvals", "bob", "")
	if pending.Code != http.StatusOK || pendingBody["pagination"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("approvals queue status=%d body=%v", pending.Code, pendingBody)
	}

	response, body := api.do(t, http.MethodPost, path, "bob", `{"decision":"approve","comment":"  verified in staging  "}`)
	if response.Code != http.StatusOK || body["reviewStatus"] != repository.ReviewApproved || body["reviewedBy"] != "bob" || body["reviewComment"] != "verified in staging" || body["applied"] != false {
		t.Fatalf("approve status=%d body=%v", response.Code, body)
	}
	if response, _ := api.do(t, http.MethodPost, path, "carol", `{"decision":"reject","comment":"too late"}`); response.Code != http.StatusConflict {
		t.Fatalf("second review status=%d", response.Code)
	}
	_, pendingBody = api.do(t, http.MethodGet, "/api/v1/approvals", "bob", "")
	if pendingBody["pagination"].(map[string]any)["total"].(float64) != 0 {
		t.Fatalf("approved recommendation is still pending: %v", pendingBody)
	}
	_, approvedBody := api.do(t, http.MethodGet, "/api/v1/approvals?reviewStatus=approved", "bob", "")
	if approvedBody["pagination"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("approved filter: %v", approvedBody)
	}

	var reviewAttributes *AccessAttributes
	for index, call := range api.access.calls {
		if call.Subresource == "approval" {
			reviewAttributes = &api.access.calls[index]
		}
	}
	if reviewAttributes == nil || reviewAttributes.Verb != "update" || reviewAttributes.Resource != "recommendations" || reviewAttributes.Namespace != "shop" || reviewAttributes.Name != id {
		t.Fatalf("review authorization = %#v", reviewAttributes)
	}
	audit, _ := api.store.ListAuditEvents(context.Background(), 1, 10)
	if audit.Total != 2 || audit.Items[0].Action != "approve" || audit.Items[0].Actor != "bob" {
		t.Fatalf("audit = %#v", audit.Items)
	}
}

func TestApprovalRejectsChangedOrExpiredLiveRecommendation(t *testing.T) {
	t.Run("workload changed", func(t *testing.T) {
		inventory := &fakeInventory{spec: checkoutSpec()}
		api := newTestAPI(t, nil, Dependencies{Provider: inventory, Usage: healthyUsage(600)})
		response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
		if response.Code != http.StatusCreated {
			t.Fatalf("generate status=%d body=%s", response.Code, response.Body.String())
		}
		inventory.spec.Generation++
		response, body = api.do(t, http.MethodPost, "/api/v1/recommendations/"+body["id"].(string)+"/review", "bob", `{"decision":"approve"}`)
		if response.Code != http.StatusConflict || body["error"].(map[string]any)["code"] != "recommendation_stale" {
			t.Fatalf("approval status=%d body=%v", response.Code, body)
		}
	})

	t.Run("observation expired", func(t *testing.T) {
		inventory := &fakeInventory{spec: checkoutSpec()}
		api := newTestAPI(t, nil, Dependencies{Provider: inventory})
		start, end, expiry := fixedNow.Add(-48*time.Hour), fixedNow.Add(-25*time.Hour), fixedNow.Add(-time.Hour)
		recommendation := optimizer.Recommendation{
			Cluster: "prod", Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api", Confidence: "high", Risk: "low",
			CurrentCPURequestMilli: 1000, CurrentMemoryRequestBytes: 1 << 30, CurrentCPULimitMilli: 2000, CurrentMemoryLimitBytes: 2 << 30,
			CPURequestMilli: 900, MemoryRequestBytes: 1 << 30, CPULimitMilli: 2000, MemoryLimitBytes: 2 << 30,
			Provenance: optimizer.Provenance{WorkloadUID: "uid-checkout", WorkloadGeneration: 12, SourceSpecFingerprint: strings.Repeat("a", 64),
				ObservationStart: &start, ObservationEnd: &end, ExpiresAt: &expiry, QueryFingerprint: strings.Repeat("b", 64),
				PolicyFingerprint: strings.Repeat("c", 64), OptimizerVersion: optimizer.AlgorithmVersion},
		}
		created, _, err := api.store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{Source: repository.SourcePrometheus, CreatedBy: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+created.ID+"/review", "bob", `{"decision":"approve"}`)
		if response.Code != http.StatusConflict || body["error"].(map[string]any)["code"] != "recommendation_stale" {
			t.Fatalf("approval status=%d body=%v", response.Code, body)
		}
	})
}

func TestReviewIsDeniedWithoutApprovalPermission(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	id := createRecommendation(t, api, "alice")
	api.access.deny = func(attributes AccessAttributes) bool { return attributes.Subresource == "approval" }
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations/"+id+"/review", "bob", `{"decision":"approve"}`); response.Code != http.StatusForbidden {
		t.Fatalf("status=%d", response.Code)
	}
	record, _ := api.store.GetRecommendation(context.Background(), id)
	if record.ReviewStatus != repository.ReviewPending {
		t.Fatalf("denied review changed state: %s", record.ReviewStatus)
	}
}

func TestReviewNotFoundAndObserveMode(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations/rec_00000000000000000000000000000000/review", "bob", `{"decision":"approve"}`); response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
	observe := newTestAPI(t, func(cfg *config.Config) { cfg.OperationMode = config.ModeObserve }, Dependencies{})
	if response, _ := observe.do(t, http.MethodPost, "/api/v1/recommendations/rec_00000000000000000000000000000000/review", "bob", `{"decision":"approve"}`); response.Code != http.StatusForbidden {
		t.Fatalf("observe status=%d", response.Code)
	}
}

func TestSavingsUseConfiguredPricesAndLatestRecommendations(t *testing.T) {
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.CostCPUCoreHour, cfg.CostMemoryGiBHour, cfg.CostCurrency = 0.04, 0.005, "EUR"
	}, Dependencies{})
	ctx := context.Background()
	expires := fixedNow.Add(time.Hour)
	base := optimizer.Recommendation{Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "app", Replicas: 2, Confidence: "high", Risk: "low",
		CurrentCPURequestMilli: 1000, CurrentMemoryRequestBytes: 2 << 30, Provenance: optimizer.Provenance{ExpiresAt: &expires}}
	stale := base
	stale.CPURequestMilli, stale.MemoryRequestBytes = 100, 1<<30
	if _, _, err := api.store.CreateRecommendation(ctx, stale, repository.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	latest := base
	latest.CPURequestMilli, latest.MemoryRequestBytes = 500, 1<<30
	if _, _, err := api.store.CreateRecommendation(ctx, latest, repository.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rejected := base
	rejected.Workload, rejected.CPURequestMilli, rejected.MemoryRequestBytes = "worker", 1, 1
	record, _, _ := api.store.CreateRecommendation(ctx, rejected, repository.CreateOptions{})
	if _, err := api.store.ReviewRecommendation(ctx, record.ID, repository.ReviewDecision{Status: repository.ReviewRejected, Reviewer: "bob", Comment: "no", At: fixedNow}); err != nil {
		t.Fatal(err)
	}

	response, body := api.do(t, http.MethodGet, "/api/v1/savings", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%v, want only the latest non-rejected recommendation", items)
	}
	item := items[0].(map[string]any)
	// Per replica-hour: current 1 core*0.04 + 2 GiB*0.005 = 0.05,
	// recommended 0.5*0.04 + 1*0.005 = 0.025; x2 replicas x730 hours.
	if item["currentMonthlyCost"].(float64) != 73 || item["recommendedMonthlyCost"].(float64) != 36.5 || item["theoreticalMonthlyChange"].(float64) != 36.5 || item["estimatedMonthlySavings"].(float64) != 36.5 || item["currency"] != "EUR" {
		t.Fatalf("item=%v", item)
	}
	if item["stage"] != savingsStageRecommended || item["stale"] != false || item["costAvailable"] != true {
		t.Fatalf("item lifecycle=%v", item)
	}
	summary := body["summary"].(map[string]any)
	if summary["theoreticalMonthlyChange"].(float64) != 36.5 || summary["recommendedMonthlyChange"].(float64) != 36.5 || summary["approvedMonthlyChange"].(float64) != 0 || summary["estimatedMonthlySavings"].(float64) != 36.5 || summary["approvedMonthlySavings"].(float64) != 0 {
		t.Fatalf("summary=%v", summary)
	}
	if realized := summary["realized"].(map[string]any); realized["available"] != false || !strings.Contains(realized["reason"].(string), "counterfactual") {
		t.Fatalf("realized=%v", realized)
	}

	_, overview := api.do(t, http.MethodGet, "/api/v1/overview", "alice", "")
	if overview["estimatedMonthlySavings"].(float64) != 36.5 || overview["currency"] != "EUR" {
		t.Fatalf("overview savings=%v", overview)
	}
}

func TestSavingsLifecycleIsConservativeAndNeverAssumesAReplica(t *testing.T) {
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.CostCPUCoreHour, cfg.CostMemoryGiBHour, cfg.CostCurrency = 0.04, 0.005, "USD"
	}, Dependencies{})
	ctx := context.Background()
	fresh, expired := fixedNow.Add(time.Hour), fixedNow.Add(-time.Hour)
	base := optimizer.Recommendation{
		Namespace: "shop", Kind: "Deployment", Container: "app", Replicas: 1, Confidence: "high", Risk: "low",
		CurrentCPURequestMilli: 1000, CurrentMemoryRequestBytes: 2 << 30,
		CPURequestMilli: 500, MemoryRequestBytes: 1 << 30,
		Provenance: optimizer.Provenance{ExpiresAt: &fresh},
	}
	create := func(workload string, replicas int32, expiry *time.Time) repository.RecommendationRecord {
		t.Helper()
		recommendation := base
		recommendation.Workload, recommendation.Replicas, recommendation.Provenance.ExpiresAt = workload, replicas, expiry
		record, _, err := api.store.CreateRecommendation(ctx, recommendation, repository.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	review := func(record repository.RecommendationRecord) {
		t.Helper()
		if _, err := api.store.ReviewRecommendation(ctx, record.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "reviewer", At: fixedNow}); err != nil {
			t.Fatal(err)
		}
	}
	observed := create("observed", 2, &fresh)
	verified := create("verified", 1, &fresh)
	stale := create("stale", 1, &expired)
	zero := create("scaled-zero", 0, &fresh)
	review(observed)
	review(verified)

	firstObserved := fixedNow.Add(-2 * time.Hour)
	for _, outcome := range []repository.VerificationRun{
		{RecommendationID: observed.ID, Status: repository.VerificationPending, ConfigurationObserved: true, FirstObservedAt: &firstObserved,
			ObservedGeneration: 2, ObservedSpecFingerprint: strings.Repeat("a", 64), Attribution: repository.VerificationAttributionExactPodUID, CheckedAt: fixedNow},
		{RecommendationID: verified.ID, Status: repository.VerificationVerified, ConfigurationObserved: true, FirstObservedAt: &firstObserved,
			ObservedGeneration: 3, ObservedSpecFingerprint: strings.Repeat("b", 64), EvidenceComplete: true, Attribution: repository.VerificationAttributionExactPodUID, CheckedAt: fixedNow},
	} {
		if _, err := api.store.RecordVerification(ctx, outcome); err != nil {
			t.Fatal(err)
		}
	}

	response, body := api.do(t, http.MethodGet, "/api/v1/savings?pageSize=25", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	items := body["items"].([]any)
	byWorkload := make(map[string]map[string]any, len(items))
	for _, raw := range items {
		item := raw.(map[string]any)
		byWorkload[item["workload"].(string)] = item
	}
	if item := byWorkload["observed"]; item["stage"] != savingsStageExternallyObserved || item["configurationObserved"] != true || item["verificationStatus"] != repository.VerificationPending {
		t.Fatalf("observed item=%v", item)
	}
	if item := byWorkload["verified"]; item["stage"] != savingsStageVerified || item["evidenceComplete"] != true || item["verificationStatus"] != repository.VerificationVerified {
		t.Fatalf("verified item=%v", item)
	}
	if item := byWorkload["stale"]; item["stage"] != savingsStageTheoretical || item["stale"] != true {
		t.Fatalf("stale item=%v", item)
	}
	if item := byWorkload["scaled-zero"]; item["replicas"].(float64) != 0 || item["costAvailable"] != false || item["currentMonthlyCost"] != nil || !strings.Contains(item["costUnavailableReason"].(string), "will not assume one") {
		t.Fatalf("scaled-zero item=%v", item)
	}
	summary := body["summary"].(map[string]any)
	for field, want := range map[string]float64{
		"theoreticalMonthlyChange": 73,
		"recommendedMonthlyChange": 54.75,
		"approvedMonthlyChange": 54.75,
		"externallyObservedMonthlyChange": 54.75,
		"verifiedMonthlyChange": 18.25,
	} {
		if got := summary[field].(float64); got != want {
			t.Fatalf("%s=%v want %v; summary=%v", field, got, want, summary)
		}
	}
	if summary["pricedRecommendations"].(float64) != 3 || summary["recommendations"].(float64) != 4 {
		t.Fatalf("summary counts=%v", summary)
	}
}

func TestForecastConvertsUnitsAndTimestamps(t *testing.T) {
	source := healthyUsage(100)
	forecaster := &fakeForecaster{}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source, Forecaster: forecaster})
	response, body := api.do(t, http.MethodPost, "/api/v1/forecasts", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","resource":"cpu","horizon":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if forecaster.request.IntervalSeconds != 3600 || forecaster.request.Resource != "cpu" || len(forecaster.request.Samples) != 100 || forecaster.request.Samples[0] != 0.2 || !forecaster.request.Persist || !strings.HasPrefix(forecaster.request.SeriesID, "kuberight-") {
		t.Fatalf("forecast request = %#v", forecaster.request)
	}
	points := body["forecast"].([]any)
	first := points[0].(map[string]any)
	lastHistory := source.history.CPU.Points[99]
	wantTime := lastHistory.Timestamp.Add(time.Hour).Format(time.RFC3339)
	if len(points) != 3 || first["timestamp"] != wantTime || first["value"].(float64) != lastHistory.Value {
		t.Fatalf("first point=%v want time %s value %v", first, wantTime, lastHistory.Value)
	}
	if body["unit"] != "millicores" || body["experimental"] != true {
		t.Fatalf("body=%v", body)
	}
	if diagnostics := body["diagnostics"].(map[string]any); diagnostics["model"] != "test-model" || diagnostics["predictions"] != nil {
		t.Fatalf("diagnostics=%v", diagnostics)
	}
}

func TestForecastFailureModes(t *testing.T) {
	body := `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","resource":"memory"}`
	cases := map[string]struct {
		dependencies Dependencies
		body         string
		status       int
	}{
		"not configured":   {Dependencies{Usage: healthyUsage(100)}, body, http.StatusServiceUnavailable},
		"short history":    {Dependencies{Usage: healthyUsage(10), Forecaster: &fakeForecaster{}}, body, http.StatusUnprocessableEntity},
		"busy":             {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{err: forecast.ErrBusy}}, body, http.StatusTooManyRequests},
		"rejected":         {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{err: forecast.ErrRejected}}, body, http.StatusUnprocessableEntity},
		"down":             {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{err: forecast.ErrUnavailable}}, body, http.StatusServiceUnavailable},
		"bad resource":     {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{}}, strings.Replace(body, "memory", "disk", 1), http.StatusBadRequest},
		"bad confidence":   {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{}}, strings.Replace(body, `"memory"`, `"memory","confidenceLevel":0.5`, 1), http.StatusBadRequest},
		"horizon too long": {Dependencies{Usage: healthyUsage(100), Forecaster: &fakeForecaster{}}, strings.Replace(body, `"memory"`, `"memory","horizon":500`, 1), http.StatusBadRequest},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			api := newTestAPI(t, nil, test.dependencies)
			if response, _ := api.do(t, http.MethodPost, "/api/v1/forecasts", "alice", test.body); response.Code != test.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
		})
	}
}

func TestMetricsHistoryReturnsSeriesAndCurrentSettings(t *testing.T) {
	source := healthyUsage(50)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/metrics/history?namespace=shop&kind=Deployment&workload=checkout&container=api&window=24h", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if items := body["items"].([]any); len(items) != 2 || items[0].(map[string]any)["resource"] != "cpu" {
		t.Fatalf("items=%v", items)
	}
	if current := body["current"].(map[string]any); current["cpuRequestMilli"].(float64) != 1000 {
		t.Fatalf("current=%v", current)
	}
	if source.windows[0].Duration != 24*time.Hour || source.windows[0].Step != 5*time.Minute {
		t.Fatalf("window=%#v", source.windows[0])
	}
	if response, _ := api.do(t, http.MethodGet, "/api/v1/metrics/history?namespace=shop", "alice", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("missing subject status=%d", response.Code)
	}
	unconfigured := newTestAPI(t, nil, Dependencies{})
	_, unavailable := unconfigured.do(t, http.MethodGet, "/api/v1/metrics/history?namespace=shop&kind=Deployment&workload=checkout&container=api", "alice", "")
	if unavailable["status"].(map[string]any)["available"] != false {
		t.Fatalf("unconfigured history = %v", unavailable)
	}
}

func TestChartStepBoundsPointCount(t *testing.T) {
	if step := chartStep(30*24*time.Hour, 5*time.Minute); step != 72*time.Minute {
		t.Fatalf("30d step=%s", step)
	}
	if step := chartStep(time.Hour, 5*time.Minute); step != 5*time.Minute {
		t.Fatalf("1h step=%s", step)
	}
}

func TestOverviewReportsCountsAndDependencies(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: &fakeUsage{ready: errors.New("down")}})
	createRecommendation(t, api, "alice")
	response, body := api.do(t, http.MethodGet, "/api/v1/overview", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body["workloads"].(float64) != 3 || body["clusters"].(float64) != 3 {
		t.Fatalf("counts missing: %v", body)
	}
	recommendations := body["recommendations"].(map[string]any)
	if recommendations["total"].(float64) != 1 || recommendations["pending"].(float64) != 1 || recommendations["source"] != "memory" {
		t.Fatalf("recommendations=%v", recommendations)
	}
	states := map[string]map[string]any{}
	for _, item := range body["dependencies"].([]any) {
		dependency := item.(map[string]any)
		states[dependency["name"].(string)] = dependency
	}
	if states["Prometheus"]["configured"] != true || states["Prometheus"]["healthy"] != false {
		t.Fatalf("Prometheus status=%v", states["Prometheus"])
	}
	if states["Forecasting service"]["configured"] != false || states["Recommendation store"]["healthy"] != true {
		t.Fatalf("dependencies=%v", states)
	}
	if _, ok := body["estimatedMonthlySavings"]; ok {
		t.Fatal("savings must be absent without configured prices")
	}
}

type failingStore struct{ *repository.Memory }

func (failingStore) Ready(context.Context) error { return errors.New("database down") }

func TestReadinessTreatsStoreAsCriticalAndIntegrationsAsOptional(t *testing.T) {
	cfg := fallbackConfig()
	handler, err := NewHandler(cfg, Dependencies{Access: &recordingAccess{}, Usage: &fakeUsage{ready: errors.New("down")}})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"usageMetrics":"degraded"`) || !strings.Contains(response.Body.String(), `"forecasting":"not_configured"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	handler, err = NewHandler(cfg, Dependencies{Access: &recordingAccess{}, Repository: failingStore{repository.NewMemory(1)}})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"repository":"unavailable"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestListRecommendationsValidatesReviewFilters(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	createRecommendation(t, api, "alice")
	if response, _ := api.do(t, http.MethodGet, "/api/v1/recommendations?reviewStatus=maybe", "alice", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
	if response, _ := api.do(t, http.MethodGet, "/api/v1/recommendations?latest=yes", "alice", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
	response, body := api.do(t, http.MethodGet, "/api/v1/recommendations?latest=true&reviewStatus=pending", "alice", "")
	if response.Code != http.StatusOK || body["pagination"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
}

func TestIntegrationsReportLiveHealth(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{Forecaster: &fakeForecaster{}})
	_, body := api.do(t, http.MethodGet, "/api/v1/settings/integrations", "alice", "")
	found := false
	for _, item := range body["items"].([]any) {
		dependency := item.(map[string]any)
		if dependency["name"] == "Forecasting service" {
			found = dependency["configured"] == true && dependency["healthy"] == true
		}
	}
	if !found {
		t.Fatalf("forecasting health missing: %v", body)
	}
}

func TestTelemetryLabelsReviewRoute(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/recommendations/rec_abc/review", nil)
	if route := telemetryRoute(request); route != "POST /api/v1/recommendations/{id}/review" {
		t.Fatalf("route=%s", route)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/recommendations/a/b/review", nil)
	if route := telemetryRoute(request); route != "unmatched" {
		t.Fatalf("nested route=%s", route)
	}
}

func TestGenerateDryRunPreviewsWithoutStoring(t *testing.T) {
	source := healthyUsage(600)
	source.events = usage.Events{Available: true, ThrottlingAvailable: true, ThrottlingRatio: 0.2}
	api := newTestAPI(t, func(cfg *config.Config) { cfg.OperationMode = config.ModeObserve }, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","profile":"aggressive","dryRun":true}`)
	if response.Code != http.StatusOK || body["dryRun"] != true {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	recommendation := body["recommendation"].(map[string]any)
	// Throttling blocks the CPU reduction that the aggressive profile would make.
	if recommendation["cpuRequestMilli"].(float64) != 1000 || recommendation["cpuThrottlingRatio"].(float64) != 0.2 {
		t.Fatalf("recommendation=%v", recommendation)
	}
	if policy := recommendation["effectivePolicy"].(map[string]any); policy["cpuPercentile"].(float64) != 0.9 || policy["maximumReduction"].(float64) != 0.4 {
		t.Fatalf("aggressive profile was not applied: %v", policy)
	}
	if count, _ := api.store.CountRecommendations(context.Background()); count != 0 {
		t.Fatalf("dry run stored %d recommendations", count)
	}
	if audit, _ := api.store.ListAuditEvents(context.Background(), 1, 10); audit.Total != 0 {
		t.Fatalf("dry run was audited: %#v", audit)
	}
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody); response.Code != http.StatusForbidden {
		t.Fatalf("observe mode stored generation status=%d", response.Code)
	}
}

func TestGenerateProfilesChangeTheResult(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: healthyUsage(1200)})
	values := map[string]float64{}
	for _, profile := range []string{"conservative", "balanced", "aggressive"} {
		response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","dryRun":true,"profile":"`+profile+`"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", profile, response.Code, response.Body.String())
		}
		values[profile] = body["recommendation"].(map[string]any)["cpuRequestMilli"].(float64)
	}
	if !(values["conservative"] > values["balanced"] && values["balanced"] > values["aggressive"]) {
		t.Fatalf("profile ordering = %v", values)
	}
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","profile":"reckless"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown profile status=%d", response.Code)
	}
}

func TestPoliciesListBuiltInProfiles(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	_, body := api.do(t, http.MethodGet, "/api/v1/policies", "alice", "")
	items := body["items"].([]any)
	if len(items) != 3 || items[1].(map[string]any)["name"] != "balanced" || items[1].(map[string]any)["default"] != true {
		t.Fatalf("policies=%v", items)
	}
}

type inventoryRecords struct {
	fakeInventory
	records []map[string]any
}

func (inventory inventoryRecords) List(context.Context, string, CollectionQuery) (CollectionResult, error) {
	return CollectionResult{Items: inventory.records, Total: len(inventory.records), Status: Availability{Available: true, Source: "kubernetes-api"}}, nil
}

func TestEfficiencyComparesRequestsWithUsage(t *testing.T) {
	records := []map[string]any{
		{"namespace": "shop", "desiredReplicas": int32(4), "cpuRequestMilli": int64(500), "memoryRequestBytes": int64(1 << 30),
			"containers": []map[string]any{{"cpuRequestMilli": int64(500), "memoryRequestBytes": int64(1 << 30)}}},
		{"namespace": "shop", "desiredReplicas": int32(1), "cpuRequestMilli": int64(0), "memoryRequestBytes": int64(0),
			"containers": []map[string]any{{"cpuRequestMilli": int64(0), "memoryRequestBytes": int64(0)}}},
		{"namespace": "unmetered", "desiredReplicas": int32(2), "cpuRequestMilli": int64(100), "memoryRequestBytes": int64(1 << 28)},
	}
	source := healthyUsage(10)
	source.namespaces = map[string]usage.NamespaceUsage{"shop": {CPUAverageMilli: 500, CPUP95Milli: 800, MemoryAverageBytes: 1 << 30, MemoryP95Bytes: 2 << 30}}
	api := newTestAPI(t, func(cfg *config.Config) { cfg.CostCPUCoreHour, cfg.CostMemoryGiBHour = 0.04, 0.005 }, Dependencies{Provider: inventoryRecords{records: records}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/efficiency?window=72h", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	items := body["items"].([]any)
	shop := items[0].(map[string]any)
	// 4 replicas x 500m = 2000m requested; average 500m = 25%; P95 800m leaves 1200m.
	if shop["namespace"] != "shop" || shop["cpuRequestedMilli"].(float64) != 2000 || shop["cpuEfficiency"].(float64) != 0.25 || shop["cpuReclaimableMilli"].(float64) != 1200 {
		t.Fatalf("shop=%v", shop)
	}
	// 4 GiB requested, 2 GiB P95: 2 GiB reclaimable.
	if shop["memoryReclaimableBytes"].(float64) != float64(2<<30) || shop["containersWithoutRequests"].(float64) != 1 || shop["pods"].(float64) != 5 {
		t.Fatalf("shop memory=%v", shop)
	}
	// 1.2 cores x 0.04 + 2 GiB x 0.005 = 0.058/h x 730 h.
	if shop["reclaimableMonthlyCost"].(float64) != 42.34 {
		t.Fatalf("reclaimable cost=%v", shop["reclaimableMonthlyCost"])
	}
	unmetered := items[1].(map[string]any)
	if unmetered["usageAvailable"] != false || unmetered["cpuAverageMilli"] != nil {
		t.Fatalf("missing usage must not read as zero: %v", unmetered)
	}
	summary := body["summary"].(map[string]any)
	if summary["cpuRequestedMilli"].(float64) != 2000 || summary["namespaces"].(float64) != 2 || summary["window"] != "72h0m0s" {
		t.Fatalf("summary=%v", summary)
	}
	if response, _ := newTestAPI(t, nil, Dependencies{Usage: source}).do(t, http.MethodGet, "/api/v1/efficiency", "alice", ""); response.Code != http.StatusOK {
		t.Fatalf("unconfigured status=%d", response.Code)
	}
}
