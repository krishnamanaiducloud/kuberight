package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

type reportingFakeUsage struct {
	*exactFakeUsage
	report   usage.AttributionReport
	reportOf usage.WorkloadIdentity
}

func (source *reportingFakeUsage) AttributionReport(_ context.Context, subject usage.Subject, identity usage.WorkloadIdentity, window usage.Window) (usage.AttributionReport, error) {
	source.reportOf = identity
	report := source.report
	report.Subject, report.End, report.Start = subject, window.End, window.End.Add(-window.Duration)
	return report, source.err
}

func TestAttributionDiagnosticsShowsUIDChainAndOutcome(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(2)
	spec.MetricIdentity.Owners = []usage.OwnerIdentity{{Kind: "ReplicaSet", Name: "checkout-current", UID: "rs-current", CreatedAt: fixedNow.Add(-9 * 24 * time.Hour), Replicas: 2}}
	source := &reportingFakeUsage{exactFakeUsage: &exactFakeUsage{fakeUsage: healthyUsage(600)}, report: usage.AttributionReport{
		WorkloadUID: "uid-checkout", Owners: []usage.OwnerAttribution{{Kind: "ReplicaSet", Name: "checkout-current", UID: "rs-current", Verified: true, Recorded: true}},
		Pods:     []usage.PodAttribution{{Pod: "checkout-current-aaaaa", UID: "pod-a", Attributed: true, Reason: "attributed"}},
		Families: []usage.FamilyStatus{{Name: "kube_pod_owner", Required: true, Series: 2}}, MissingFamilies: []string{},
		Eligible: true, Outcome: usage.OutcomeAttributed, Reason: "every expected replica is attributed",
	}}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/attribution?namespace=shop&kind=Deployment&workload=checkout&container=api&window=24h", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	identity := body["identity"].(map[string]any)
	owners := identity["owners"].([]any)
	if identity["uid"] != "uid-checkout" || len(owners) != 1 || owners[0].(map[string]any)["uid"] != "rs-current" || len(identity["pods"].([]any)) != 2 {
		t.Fatalf("identity = %v", identity)
	}
	report := body["report"].(map[string]any)
	if report["outcome"] != "attributed" || report["eligible"] != true || body["window"] != "24h0m0s" {
		t.Fatalf("report = %v", report)
	}
	if source.reportOf.UID != "uid-checkout" || len(source.reportOf.Owners) != 1 {
		t.Fatalf("the report did not receive the API-verified identity: %#v", source.reportOf)
	}
	for _, secret := range []string{"token", "authorization", "Bearer"} {
		if strings.Contains(strings.ToLower(response.Body.String()), strings.ToLower(secret)) {
			t.Fatalf("diagnostics must not mention %q: %s", secret, response.Body.String())
		}
	}
	api.access.mu.Lock()
	calls := append([]AccessAttributes(nil), api.access.calls...)
	api.access.mu.Unlock()
	for _, attributes := range calls {
		if attributes.Verb != "get" && attributes.Verb != "list" {
			t.Fatalf("diagnostics must stay read-only, authorized %+v", attributes)
		}
	}
}

func TestAttributionDiagnosticsReportsYoungWorkloadAsInsufficientHistory(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(2)
	source := &reportingFakeUsage{exactFakeUsage: &exactFakeUsage{fakeUsage: healthyUsage(0)}}
	source.err = fmt.Errorf("%w: the current workload has existed for less than one 5m0s observation step", usage.ErrInsufficientHistory)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodGet, "/api/v1/attribution?namespace=shop&kind=Deployment&workload=checkout&container=api", "alice", "")
	if response.Code != http.StatusOK || body["report"].(map[string]any)["outcome"] != "insufficient_history" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAttributionDiagnosticsNeedsAnAttributingSource(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(2)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: healthyUsage(10)})
	response, body := api.do(t, http.MethodGet, "/api/v1/attribution?namespace=shop&kind=Deployment&workload=checkout&container=api", "alice", "")
	if response.Code != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != "integration_not_configured" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	response, _ = api.do(t, http.MethodGet, "/api/v1/attribution?namespace=shop&kind=Deployment&workload=checkout", "alice", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a missing container must be rejected, got %d", response.Code)
	}
}

func TestGenerateReportsYoungWorkloadAsInsufficientHistory(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(2)
	source := &exactFakeUsage{fakeUsage: healthyUsage(0)}
	source.err = fmt.Errorf("%w: the current workload has existed for less than one 5m0s observation step", usage.ErrInsufficientHistory)
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","dryRun":true}`)
	if response.Code != http.StatusUnprocessableEntity || body["error"].(map[string]any)["code"] != "insufficient_history" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// Regression: a workload whose only attributed sample is the window's last
// step produced "observation end must be after observation start" (422
// invalid_recommendation_input) instead of a low-confidence result.
func TestGenerateWithOneSampleIsInsufficientDataNotInvalidInput(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(1)
	base := healthyUsage(0)
	at := fixedNow
	base.history.CPU.Points = []usage.Point{{Timestamp: at, Value: 3}}
	base.history.Memory.Points = []usage.Point{{Timestamp: at, Value: 4 << 20}}
	source := &exactFakeUsage{fakeUsage: base}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source})
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations:generate", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","dryRun":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	recommendation := body["recommendation"].(map[string]any)
	assessment := recommendation["assessment"].(map[string]any)
	provenance := recommendation["provenance"].(map[string]any)
	if assessment["status"] != "insufficient_data" || provenance["observationStart"] != at.Add(-5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("assessment=%v provenance=%v", assessment["status"], provenance)
	}
	if recommendation["cpuRequestMilli"].(float64) < 1000 || recommendation["memoryRequestBytes"].(float64) < float64(1<<30) {
		t.Fatalf("one sample must never reduce requests: %v", recommendation)
	}
}

// Forecasts must consume the same UID-attributed history as
// recommendations, so a deleted same-name workload never shapes them.
func TestForecastUsesWorkloadUIDAttribution(t *testing.T) {
	spec := checkoutSpec()
	spec.MetricIdentity = recommendationIdentity(2)
	base := healthyUsage(100)
	base.history.ReplicaCoverage = &usage.ReplicaCoverage{Complete: false, Issues: []string{"exact CPU history covers 50% of the requested window"}}
	source := &exactFakeUsage{fakeUsage: base}
	forecaster := &fakeForecaster{}
	api := newTestAPI(t, nil, Dependencies{Provider: fakeInventory{spec: spec}, Usage: source, Forecaster: forecaster})
	response, body := api.do(t, http.MethodPost, "/api/v1/forecasts", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","resource":"cpu","horizon":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	attribution := body["attribution"].(map[string]any)
	if source.historyIdentity.UID != "uid-checkout" || attribution["mode"] != "workload-uid" || attribution["complete"] != false || len(attribution["issues"].([]any)) != 1 {
		t.Fatalf("identity=%#v attribution=%v", source.historyIdentity, attribution)
	}

	// Ownership that cannot be established blocks the forecast instead of
	// falling back to pod names.
	source.err = fmt.Errorf("%w: usage exists for pods matching the workload names", usage.ErrAttributionIncomplete)
	response, body = api.do(t, http.MethodPost, "/api/v1/forecasts", "alice", `{"namespace":"shop","kind":"Deployment","workload":"checkout","container":"api","resource":"cpu","horizon":3}`)
	if response.Code != http.StatusUnprocessableEntity || body["error"].(map[string]any)["code"] != "metric_attribution_incomplete" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
