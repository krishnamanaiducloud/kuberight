package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/forecast"
	"github.com/kuberight/kuberight/internal/notify"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// correlationUsage adds the optional correlation series to fakeUsage.
type correlationUsage struct {
	*fakeUsage
	extra usage.ExtraSeries
}

func (source *correlationUsage) Extra(context.Context, usage.Subject, usage.Window, usage.LatencyTemplate) (usage.ExtraSeries, error) {
	return source.extra, nil
}

// backtestingForecaster adds rolling-origin replay and calibration.
type backtestingForecaster struct {
	fakeForecaster
	coverage float64
}

// Forecast predicts a flat continuation 5% above the last sample with a
// calibrated interval.
func (forecaster *backtestingForecaster) Forecast(_ context.Context, request forecast.Request) (forecast.Response, error) {
	if forecaster.err != nil {
		return forecast.Response{}, forecaster.err
	}
	coverage := forecaster.coverage
	response := forecast.Response{Model: "ridge", Calibration: &forecast.Calibration{NominalCoverage: 0.9, CalibratedCoverage: &coverage, Factor: 1.2, Calibrated: true}}
	value := request.Samples[len(request.Samples)-1] * 1.05
	for range request.Horizon {
		response.Predictions = append(response.Predictions, value)
		response.ConfidenceIntervals = append(response.ConfidenceIntervals, forecast.Interval{Lower: value * 0.9, Upper: value * 1.1})
	}
	return response, nil
}

func (forecaster *backtestingForecaster) Backtest(_ context.Context, request forecast.BacktestRequest) (forecast.BacktestResponse, error) {
	if forecaster.err != nil {
		return forecast.BacktestResponse{}, forecaster.err
	}
	response := forecast.BacktestResponse{Horizon: request.Horizon, NominalCoverage: request.ConfidenceLevel, Overall: forecast.Score{MAE: 0.01, Coverage: 0.92, BaselineMAE: 0.02}}
	for origin := range request.Origins {
		response.Origins = append(response.Origins, forecast.BacktestOrigin{OriginIndex: len(request.Samples) - (request.Origins-origin)*request.Horizon, Model: "ridge", Calibrated: true})
	}
	start := len(request.Samples) - request.Horizon
	for index := range request.Horizon {
		value := request.Samples[start+index]
		response.Points = append(response.Points, forecast.BacktestPoint{Index: start + index, Actual: value, Predicted: value, Lower: value - 0.01, Upper: value + 0.01, Baseline: value})
	}
	return response, nil
}

// policyInventory adds namespace policy and node capacity to fakeInventory.
type policyInventory struct {
	fakeInventory
	policy    optimizer.NamespacePolicy
	policyErr error
	nodes     []NodeCapacity
}

func (inventory policyInventory) NamespacePolicy(context.Context, string) (optimizer.NamespacePolicy, error) {
	return inventory.policy, inventory.policyErr
}

func (inventory policyInventory) Capacity(context.Context) (ClusterCapacity, error) {
	return ClusterCapacity{Nodes: inventory.nodes}, nil
}

// signalsFor builds a day of correlated five-minute signals: latency follows
// throttling, which follows CPU.
func signalsFor(count int) *usage.Signals {
	step := 5 * time.Minute
	start := fixedNow.Add(-time.Duration(count) * step)
	signals := &usage.Signals{
		Start: start, End: fixedNow, Step: step.String(),
		CPU:        usage.Series{Resource: "cpu"},
		Memory:     usage.Series{Resource: "memory"},
		Throttling: &usage.Series{Resource: "throttling"}, Replicas: &usage.Series{Resource: "replicas"}, Latency: &usage.Series{Resource: "latency"},
		Events: []usage.Event{{Timestamp: start.Add(time.Duration(count/2) * step), Kind: usage.EventOOMKill, Count: 1}}, Warnings: []string{},
	}
	for index := range count {
		at := start.Add(time.Duration(index) * step)
		cpu := 300 + 150*math.Sin(float64(index)/7) + float64(index%3)
		throttle := math.Max(0, (cpu-380)/400)
		signals.CPU.Points = append(signals.CPU.Points, usage.Point{Timestamp: at, Value: cpu})
		signals.Memory.Points = append(signals.Memory.Points, usage.Point{Timestamp: at, Value: float64(256<<20) + float64(index%7)*1e5})
		signals.Throttling.Points = append(signals.Throttling.Points, usage.Point{Timestamp: at, Value: throttle})
		signals.Replicas.Points = append(signals.Replicas.Points, usage.Point{Timestamp: at, Value: 3})
		signals.Latency.Points = append(signals.Latency.Points, usage.Point{Timestamp: at, Value: 0.05 + throttle*0.5})
	}
	return signals
}

func storeApproved(t *testing.T, api testAPI, recommendation optimizer.Recommendation, reviewedAt time.Time) repository.RecommendationRecord {
	t.Helper()
	record, _, err := api.store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{CreatedBy: "alice", Source: repository.SourcePrometheus})
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := api.store.ReviewRecommendation(context.Background(), record.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", At: reviewedAt})
	if err != nil {
		t.Fatal(err)
	}
	return reviewed
}

func liveRecommendation() optimizer.Recommendation {
	spec := checkoutSpec()
	container := spec.Containers[0]
	return optimizer.Recommendation{
		Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name,
		CPURequestMilli: container.CPURequestMilli, CPULimitMilli: container.CPULimitMilli,
		MemoryRequestBytes: container.MemoryRequestBytes, MemoryLimitBytes: container.MemoryLimitBytes,
		CurrentCPURequestMilli: 1500, CurrentMemoryRequestBytes: 2 << 30, Confidence: "high", Risk: "low",
	}
}

const correlationPath = "/api/v1/correlation?namespace=shop&kind=Deployment&workload=checkout&container=api&window=24h"

func TestCorrelationAnalysisEndpoint(t *testing.T) {
	source := &correlationUsage{fakeUsage: healthyUsage(700), extra: usage.ExtraSeries{Warnings: []string{"requestRate series could not be read"}}}
	source.history.CPU = steadySeries("cpu", "millicores", 24*14, 300, time.Hour)
	source.signals = signalsFor(288)
	notifier := &recordingNotifier{}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source, Forecaster: &backtestingForecaster{coverage: 0.9}, Notifier: notifier})
	storeApproved(t, api, liveRecommendation(), fixedNow.Add(-6*time.Hour))

	response, body := api.do(t, http.MethodGet, correlationPath, "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	report := body["report"].(map[string]any)
	pairs := report["pairs"].([]any)
	if len(pairs) < 3 || report["caveats"].([]any)[0] != "Correlation measures association only. A strong or leading correlation does not show that one signal causes the other; confirm with the change history before acting." {
		t.Fatalf("report %v", report)
	}
	forecastEvaluation := body["forecast"].(map[string]any)
	if forecastEvaluation["available"] != true || len(forecastEvaluation["points"].([]any)) != backtestHorizon || forecastEvaluation["unit"] != "millicores" {
		t.Fatalf("forecast evaluation %v", forecastEvaluation)
	}
	if body["recommendation"] == nil || body["current"] == nil {
		t.Fatalf("recommendation and current spec missing: %v", body)
	}
	verification := body["verification"].(map[string]any)
	if verification["status"] != VerificationRegressed || verification["applied"] != true {
		t.Fatalf("verification %v", verification)
	}
	if !strings.Contains(strings.Join(toStrings(body["warnings"]), " "), "requestRate") {
		t.Fatalf("extra warnings must be surfaced: %v", body["warnings"])
	}
	// The regression is announced once, however often it is observed.
	api.do(t, http.MethodGet, correlationPath, "alice", "")
	regressions := 0
	for _, event := range notifier.events {
		if event.Type == notify.RecommendationRegressed {
			regressions++
		}
	}
	if regressions != 1 {
		t.Fatalf("regression announced %d times", regressions)
	}
	metrics, _ := api.do(t, http.MethodGet, "/metrics", "", "")
	if !strings.Contains(metrics.Body.String(), "kuberight_recommendation_regressions 1") {
		t.Fatal("regression metric missing")
	}
}

func TestCorrelationAnalysisDegradesGracefully(t *testing.T) {
	api := newTestAPI(t, nil, Dependencies{})
	for path, want := range map[string]int{
		"/api/v1/correlation?namespace=shop": http.StatusBadRequest,
		"/api/v1/correlation?namespace=shop&kind=Deployment&workload=checkout&container=api&window=900h": http.StatusBadRequest,
		correlationPath: http.StatusOK,
	} {
		if response, _ := api.do(t, http.MethodGet, path, "alice", ""); response.Code != want {
			t.Fatalf("%s: status=%d want %d", path, response.Code, want)
		}
	}
	if _, body := api.do(t, http.MethodGet, correlationPath, "alice", ""); body["status"].(map[string]any)["available"] != false {
		t.Fatalf("without Prometheus the analysis is unavailable: %v", body)
	}
	// Core metrics only: no forecaster, no extra series, no stored advice.
	source := healthyUsage(100)
	source.signals = signalsFor(288)
	plain := newTestAPI(t, nil, Dependencies{Usage: source})
	response, body := plain.do(t, http.MethodGet, correlationPath, "alice", "")
	if response.Code != http.StatusOK || body["forecast"].(map[string]any)["available"] != false || body["recommendation"] != nil {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	source.signals = nil
	if response, _ := plain.do(t, http.MethodGet, correlationPath, "alice", ""); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no data: status=%d", response.Code)
	}
}

func TestForecastEvaluationNeedsHistoryAndService(t *testing.T) {
	source := healthyUsage(10)
	source.history.CPU = steadySeries("cpu", "millicores", 24, 300, time.Hour)
	server := &apiServer{usage: source, config: fallbackConfig(), forecaster: &backtestingForecaster{}}
	subject := usage.Subject{Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api"}
	if evaluation := server.forecastEvaluation(context.Background(), subject, fixedNow); evaluation.Available || !strings.Contains(evaluation.Reason, "at least") {
		t.Fatalf("short history: %+v", evaluation)
	}
	server.forecaster = &fakeForecaster{}
	if evaluation := server.forecastEvaluation(context.Background(), subject, fixedNow); evaluation.Available {
		t.Fatal("a forecaster without backtests cannot evaluate")
	}
	source.history.CPU = steadySeries("cpu", "millicores", 24*14, 300, time.Hour)
	server.forecaster = &backtestingForecaster{fakeForecaster: fakeForecaster{err: forecast.ErrBusy}}
	if evaluation := server.forecastEvaluation(context.Background(), subject, fixedNow); evaluation.Available || !strings.Contains(evaluation.Reason, "busy") {
		t.Fatalf("busy: %+v", evaluation)
	}
	source.err = errors.New("down")
	if evaluation := server.forecastEvaluation(context.Background(), subject, fixedNow); evaluation.Available {
		t.Fatal("history failure")
	}
	for err, want := range map[error]string{forecast.ErrRejected: "requirements", forecast.ErrBusy: "busy", errors.New("x"): "did not respond"} {
		if !strings.Contains(forecastFailure(err), want) {
			t.Fatalf("%v: %s", err, forecastFailure(err))
		}
	}
}

func TestVerificationStatuses(t *testing.T) {
	source := healthyUsage(100)
	source.signals = signalsFor(48)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source})

	pending := storeApproved(t, api, liveRecommendation(), fixedNow.Add(-10*time.Minute))
	response, body := api.do(t, http.MethodGet, "/api/v1/recommendations/"+pending.ID+"/verification", "alice", "")
	if response.Code != http.StatusOK || body["status"] != VerificationPending {
		t.Fatalf("pending: status=%d body=%v", response.Code, body)
	}

	notLive := liveRecommendation()
	notLive.CPURequestMilli = 700
	notApplied := storeApproved(t, api, notLive, fixedNow.Add(-5*time.Hour))
	if _, body := api.do(t, http.MethodGet, "/api/v1/recommendations/"+notApplied.ID+"/verification", "alice", ""); body["status"] != VerificationNotApplied {
		t.Fatalf("not applied: %v", body)
	}

	// Clean signals after the change verify.
	clean := signalsFor(48)
	clean.Events = nil
	clean.Throttling = nil
	source.signals = clean
	applied := storeApproved(t, api, liveRecommendation(), fixedNow.Add(-5*time.Hour))
	if _, body := api.do(t, http.MethodGet, "/api/v1/recommendations/"+applied.ID+"/verification", "alice", ""); body["status"] != VerificationVerified {
		t.Fatalf("verified: %v", body)
	}

	record, _, _ := api.store.CreateRecommendation(context.Background(), liveRecommendation(), repository.CreateOptions{CreatedBy: "alice"})
	if response, _ := api.do(t, http.MethodGet, "/api/v1/recommendations/"+record.ID+"/verification", "alice", ""); response.Code != http.StatusConflict {
		t.Fatalf("pending review: status=%d", response.Code)
	}
	for path, want := range map[string]int{"/api/v1/recommendations/bad-id/verification": http.StatusBadRequest, "/api/v1/recommendations/rec_missing/verification": http.StatusNotFound} {
		if response, _ := api.do(t, http.MethodGet, path, "alice", ""); response.Code != want {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}

	unavailable := newTestAPI(t, nil, Dependencies{})
	stored := storeApproved(t, unavailable, liveRecommendation(), fixedNow.Add(-5*time.Hour))
	if _, body := unavailable.do(t, http.MethodGet, "/api/v1/recommendations/"+stored.ID+"/verification", "alice", ""); body["status"] != VerificationUnavailable {
		t.Fatalf("unavailable: %v", body)
	}
}

func TestVerifierPassesOverApprovedRecommendations(t *testing.T) {
	source := healthyUsage(100)
	source.signals = signalsFor(48)
	state := NewVerificationState()
	notifier := &recordingNotifier{}
	dependencies := Dependencies{Provider: fakeInventory{spec: checkoutSpec()}, Usage: source, Notifier: notifier, Verification: state}
	api := newTestAPI(t, nil, dependencies)
	storeApproved(t, api, liveRecommendation(), fixedNow.Add(-5*time.Hour))
	dependencies.Repository, dependencies.Access, dependencies.Now = api.store, api.access, func() time.Time { return fixedNow }
	cfg := fallbackConfig()
	cfg.VerificationInterval = time.Minute
	verifier, err := NewVerifier(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if checked := verifier.Pass(context.Background()); checked != 1 {
		t.Fatalf("checked %d", checked)
	}
	verifier.Pass(context.Background())
	if len(notifier.events) != 1 || notifier.events[0].Type != notify.RecommendationRegressed {
		t.Fatalf("one regression event expected: %+v", notifier.events)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verifier.Run(ctx)
	cfg.VerificationInterval = 0
	idle, _ := NewVerifier(cfg, dependencies)
	idle.Run(context.Background())
	if _, err := NewVerifier(cfg, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	if joinLimited([]string{"a", "b", "c", "d"}, 2) != "a; b; 2 more" || !matches(0, 0) || matches(10, 0) || !matches(1010, 1000) {
		t.Fatal("helpers")
	}
}

func TestGenerateUsesAssessmentContext(t *testing.T) {
	spec := checkoutSpec()
	spec.HPA = &HPASpec{Name: "checkout", MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilization: 70, CurrentReplicas: 3}
	spec.InitContainers = []InitContainerSpec{
		{ContainerSpec: ContainerSpec{Name: "migrate", CPURequestMilli: 100}},
		{ContainerSpec: ContainerSpec{Name: "proxy", CPURequestMilli: 50, MemoryRequestBytes: 64 << 20}, Sidecar: true},
	}
	source := healthyUsage(2100)
	inventory := policyInventory{
		fakeInventory: fakeInventory{spec: spec},
		policy:        optimizer.NamespacePolicy{LimitRanges: []optimizer.LimitRange{{Name: "defaults", Type: "Container", MaxCPUMilli: 4000}}},
		nodes:         []NodeCapacity{{Name: "node-a", CPUMilli: 4000, MemoryBytes: 16 << 30, Schedulable: true}, {Name: "cordoned", CPUMilli: 64000, MemoryBytes: 256 << 30}},
	}
	api := newTestAPI(t, nil, Dependencies{Provider: inventory, Usage: source, Forecaster: &backtestingForecaster{coverage: 0.9}})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	assessment := body["assessment"].(map[string]any)
	checks := map[string]string{}
	for _, item := range assessment["checks"].([]any) {
		check := item.(map[string]any)
		checks[check["name"].(string)] = check["status"].(string)
	}
	if checks["LimitRange defaults"] != "pass" || checks["ResourceQuota"] != "not_applicable" || checks["Scheduling"] != "pass" {
		t.Fatalf("checks %v", checks)
	}
	if assessment["forecast"].(map[string]any)["source"] != "ml" || assessment["autoscaling"].(map[string]any)["hpa"] == nil {
		t.Fatalf("forecast and HPA context missing: %v", assessment)
	}
	if workload := assessment["workload"].(map[string]any); workload["approximate"] == true {
		t.Fatalf("the pod template was supplied: %v", workload)
	}
	expires, _ := time.Parse(time.RFC3339, body["provenance"].(map[string]any)["expiresAt"].(string))
	if want := fixedNow.Add(time.Duration(assessment["validForSeconds"].(float64)) * time.Second); !expires.Equal(want) {
		t.Fatalf("expiry %s, want %s", expires, want)
	}

	// The native sidecar can be sized; the run-to-completion init container
	// cannot.
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", strings.Replace(generateBody, `"api"`, `"proxy"`, 1)); response.Code != http.StatusCreated {
		t.Fatalf("sidecar status=%d body=%s", response.Code, response.Body.String())
	}
	response, body = api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", strings.Replace(generateBody, `"api"`, `"migrate"`, 1))
	if response.Code != http.StatusUnprocessableEntity || body["error"].(map[string]any)["code"] != "unsupported_container" {
		t.Fatalf("init container: status=%d body=%v", response.Code, body)
	}

	// Context failures are reported, not fatal.
	inventory.policyErr = errors.New("forbidden")
	failing := newTestAPI(t, nil, Dependencies{Provider: inventory, Usage: source, Forecaster: &fakeForecaster{err: forecast.ErrUnavailable}})
	response, body = failing.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", generateBody)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d", response.Code)
	}
	warnings := strings.Join(toStrings(body["warnings"]), " ")
	if !strings.Contains(warnings, "LimitRanges and ResourceQuotas could not be read") || !strings.Contains(warnings, "forecast was unavailable") {
		t.Fatalf("warnings %s", warnings)
	}
}

func TestHourlyPeaksAndForecastPeak(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	series := usage.Series{Points: []usage.Point{
		{Timestamp: base, Value: 1}, {Timestamp: base.Add(20 * time.Minute), Value: 5}, {Timestamp: base.Add(2 * time.Hour), Value: 3},
	}}
	values, times := hourlyPeaks(series, 2)
	if len(values) != 2 || values[0] != 10 || values[1] != 6 || !times[1].Equal(base.Add(2*time.Hour)) {
		t.Fatalf("peaks %v %v", values, times)
	}
	peak, upper := forecastPeak(forecast.Response{Predictions: []float64{1, 3}, ConfidenceIntervals: []forecast.Interval{{Lower: 0, Upper: 2}, {Lower: 2, Upper: 4}}})
	if peak != 3 || upper != 4 {
		t.Fatalf("peak %v upper %v", peak, upper)
	}
}

func TestSpecContextHelpers(t *testing.T) {
	spec := checkoutSpec()
	if spec.hpaContext("api") != nil {
		t.Fatal("no HPA")
	}
	spec.HPA = &HPASpec{TargetCPUUtilization: 60, Container: "other"}
	if spec.hpaContext("api") != nil {
		t.Fatal("a container HPA for another container does not apply")
	}
	spec.HPA.Container = "api"
	if context := spec.hpaContext("api"); context == nil || !context.ContainerMetric {
		t.Fatal("container HPA")
	}
	spec.InitContainers = []InitContainerSpec{{ContainerSpec: ContainerSpec{Name: "migrate"}}}
	if !spec.IsInitContainer("migrate") || spec.IsInitContainer("api") {
		t.Fatal("init container detection")
	}
	if _, found := spec.Container("migrate"); found {
		t.Fatal("run-to-completion init containers are not sizable containers")
	}
}
