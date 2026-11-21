package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/forecast"
	"github.com/kuberight/kuberight/internal/metrics"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

const (
	hoursPerMonth        = 730
	bytesPerGiB          = 1 << 30
	maxSavingsRecords    = 10_000
	savingsPageSize      = 1_000
	historyChartPoints   = 600
	dependencyTimeout    = 3 * time.Second
	minimumForecastInput = 48
	maximumForecastInput = 10_080
)

func subjectFromQuery(r *http.Request) usage.Subject {
	query := r.URL.Query()
	return usage.Subject{Namespace: query.Get("namespace"), Kind: query.Get("kind"), Workload: query.Get("workload"), Container: query.Get("container")}
}

// chartStep coarsens the configured step so a chart never exceeds a few
// hundred points, while keeping whole-minute alignment.
func chartStep(window, minimum time.Duration) time.Duration {
	step := max(minimum, window/historyChartPoints)
	if step > time.Minute {
		step = step.Truncate(time.Minute)
	}
	return step
}

func (server *apiServer) metricsHistory(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromQuery(r)
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error()+"; namespace, kind, workload, and container are required")
		return
	}
	span, err := parseChartRange(r, server.config.RecommendationWindow, server.config.RecommendationStep, server.now())
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "metrics-history", Namespace: subject.Namespace}) {
		return
	}
	if server.usage == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": 1, "pageSize": 2, "total": 0},
			"status": Availability{Available: false, Source: "prometheus", Reason: "Prometheus is not configured"},
		})
		return
	}
	step := span.step
	history, err := server.usage.History(r.Context(), subject, span.window())
	status := Availability{Available: true, Source: "prometheus"}
	if errors.Is(err, usage.ErrNoData) {
		status.Reason = "Prometheus returned no usage for this container in the requested window"
	} else if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	body := map[string]any{
		"items":      []usage.Series{history.CPU, history.Memory},
		"pagination": map[string]int{"page": 1, "pageSize": 2, "total": 2},
		"status":     status,
		"subject":    subject, "start": history.Start, "end": history.End, "step": step.String(),
		"warnings": history.Warnings,
		"range":    dataAvailability(span, history.CPU, history.Memory),
	}
	// Current settings let a chart overlay requests and limits; their
	// absence is reported rather than treated as zero.
	if resolver, ok := server.provider.(WorkloadResolver); ok && server.provider != nil {
		if spec, specErr := resolver.Workload(r.Context(), subject.Namespace, subject.Kind, subject.Workload); specErr == nil {
			if container, found := spec.Container(subject.Container); found {
				body["current"] = container
			}
		}
	}
	respondJSON(w, http.StatusOK, body)
}

func (server *apiServer) forecastStatus(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "forecasts"}) {
		return
	}
	status := Availability{Available: server.forecaster != nil && server.usage != nil, Source: "forecasting"}
	switch {
	case server.forecaster == nil:
		status.Reason = "forecasting is not configured"
	case server.usage == nil:
		status.Reason = "forecasts require Prometheus usage history, which is not configured"
	default:
		status.Reason = "forecasts are computed on demand and are not stored; submit POST /api/v1/forecasts"
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": []any{}, "pagination": map[string]int{"page": 1, "pageSize": 0, "total": 0}, "status": status,
	})
}

type forecastRequestBody struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	Resource  string `json:"resource"`
	Horizon   int    `json:"horizon,omitempty"`
	Window    string `json:"window,omitempty"`
	// Step is "1h" (the default, up to 168 steps) or "24h" (up to 90 steps)
	// for 30- and 90-day horizons.
	Step            string  `json:"step,omitempty"`
	ConfidenceLevel float64 `json:"confidenceLevel,omitempty"`
}

type forecastPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
	Lower     float64   `json:"lower"`
	Upper     float64   `json:"upper"`
}

func (server *apiServer) createForecast(w http.ResponseWriter, r *http.Request) {
	var request forecastRequestBody
	if err := decodeJSON(w, r, &request); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	subject := usage.Subject{Namespace: request.Namespace, Kind: request.Kind, Workload: request.Workload, Container: request.Container}
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.Resource != "cpu" && request.Resource != "memory" {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "resource must be cpu or memory")
		return
	}
	if request.Horizon == 0 {
		request.Horizon = 24
	}
	step, defaultWindow, maximumHorizon := server.config.ForecastStep, server.config.ForecastWindow, 168
	switch request.Step {
	case "", "1h":
	case "24h":
		// Daily steps reach 30 and 90 days ahead. The engine needs 48
		// samples and never forecasts further than the history behind it.
		step, maximumHorizon = 24*time.Hour, 90
		defaultWindow = time.Duration(max(48, 2*request.Horizon)) * 24 * time.Hour
	default:
		respondError(w, r, http.StatusBadRequest, "invalid_request", "step must be 1h or 24h")
		return
	}
	if request.Horizon < 1 || request.Horizon > maximumHorizon {
		respondError(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("horizon must be between 1 and %d steps of %s", maximumHorizon, step))
		return
	}
	if request.ConfidenceLevel == 0 {
		request.ConfidenceLevel = 0.9
	}
	if request.ConfidenceLevel != 0.8 && request.ConfidenceLevel != 0.9 && request.ConfidenceLevel != 0.95 {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "confidenceLevel must be 0.8, 0.9, or 0.95")
		return
	}
	window, err := parseForecastWindow(request.Window, defaultWindow, step)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "forecasts", Namespace: request.Namespace}) {
		return
	}
	if server.forecaster == nil || server.usage == nil {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "forecasting requires Prometheus usage history, which is not configured")
		return
	}
	// Forecasts consume the same UID-attributed history as recommendations,
	// so a same-name predecessor's usage never shapes them. Pod-name history
	// is only used by providers that cannot resolve workload identity.
	var spec *WorkloadSpec
	if resolver, ok := server.provider.(WorkloadResolver); ok && server.provider != nil {
		if resolved, resolveErr := resolver.Workload(r.Context(), subject.Namespace, subject.Kind, subject.Workload); resolveErr == nil {
			if _, found := resolved.Container(subject.Container); found {
				spec = &resolved
			}
		}
	}
	queryWindow := usage.Window{Duration: window, Step: step, End: server.now().UTC().Truncate(time.Second)}
	attribution := "pod-name"
	var history usage.History
	if exact, ok := server.usage.(attributedUsageSource); ok && spec != nil && spec.MetricIdentity != nil {
		attribution = "workload-uid"
		history, err = exact.HistoryForIdentity(r.Context(), subject, spec.MetricIdentity.ForContainer(subject.Container), queryWindow)
	} else {
		history, err = server.usage.History(r.Context(), subject, queryWindow)
	}
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	attributionIssues := []string{}
	if history.ReplicaCoverage != nil {
		attributionIssues = append(attributionIssues, history.ReplicaCoverage.Issues...)
	}
	series := history.CPU
	scale := 1000.0 // The engine forecasts CPU in cores.
	if request.Resource == "memory" {
		series, scale = history.Memory, 1
	}
	if len(series.Points) < minimumForecastInput || len(series.Points) < request.Horizon {
		respondError(w, r, http.StatusUnprocessableEntity, "insufficient_history", fmt.Sprintf(
			"collecting history: %d samples of %s exist, and a %d-step forecast needs at least %d; widen the window, wait for more history, or check Prometheus retention",
			len(series.Points), step, request.Horizon, max(minimumForecastInput, request.Horizon)))
		return
	}
	if len(series.Points) > maximumForecastInput {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "the window contains more samples than the forecasting engine accepts; shorten it")
		return
	}
	samples := make([]float64, len(series.Points))
	timestamps := make([]time.Time, len(series.Points))
	for index, point := range series.Points {
		samples[index] = point.Value / scale
		timestamps[index] = point.Timestamp
	}
	seriesID := ""
	if spec != nil {
		seriesID = forecastSeriesID(*spec, subject.Container, request.Resource)
	}
	response, err := server.forecaster.Forecast(r.Context(), forecast.Request{
		Samples: samples, Timestamps: timestamps, Horizon: request.Horizon,
		IntervalSeconds: int(step / time.Second), Resource: request.Resource, ConfidenceLevel: request.ConfidenceLevel,
		SeriesID: seriesID,
	})
	switch {
	case errors.Is(err, forecast.ErrBusy):
		w.Header().Set("Retry-After", "1")
		respondError(w, r, http.StatusTooManyRequests, "forecasting_busy", "the forecasting engine is at capacity; retry shortly")
		return
	case errors.Is(err, forecast.ErrRejected):
		respondError(w, r, http.StatusUnprocessableEntity, "forecast_rejected", err.Error())
		return
	case err != nil:
		server.logger.Error("forecast failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "forecasting is unavailable")
		return
	}
	last := series.Points[len(series.Points)-1].Timestamp
	points := make([]forecastPoint, request.Horizon)
	for index := range points {
		interval := response.ConfidenceIntervals[index]
		points[index] = forecastPoint{
			Timestamp: last.Add(time.Duration(index+1) * step),
			Value:     roundMilli(response.Predictions[index] * scale),
			Lower:     roundMilli(math.Max(0, interval.Lower*scale)),
			Upper:     roundMilli(interval.Upper * scale),
		}
	}
	diagnostics := response
	diagnostics.Predictions, diagnostics.ConfidenceIntervals = nil, nil
	respondJSON(w, http.StatusOK, map[string]any{
		"subject": subject, "resource": request.Resource, "unit": series.Unit, "step": step.String(),
		"history": series.Points, "historyQuality": series.Quality, "forecast": points,
		"confidenceLevel": request.ConfidenceLevel, "diagnostics": diagnostics, "experimental": true,
		"warning":     "Forecasts are experimental and advisory; they never change recommendations or workloads.",
		"attribution": map[string]any{"mode": attribution, "complete": len(attributionIssues) == 0, "issues": attributionIssues},
	})
}

type savingsItem struct {
	RecommendationID      string `json:"id"`
	Cluster               string `json:"cluster,omitempty"`
	Namespace             string `json:"namespace"`
	Kind                  string `json:"kind,omitempty"`
	Workload              string `json:"workload"`
	Container             string `json:"container"`
	Replicas              int32  `json:"replicas"`
	ReviewStatus          string `json:"reviewStatus"`
	Stage                 string `json:"stage"`
	Stale                 bool   `json:"stale"`
	Approvable            bool   `json:"approvable"`
	EligibilityReason     string `json:"eligibilityReason,omitempty"`
	CostAvailable         bool   `json:"costAvailable"`
	CostUnavailableReason string `json:"costUnavailableReason,omitempty"`
	// PriceSource is "configured", "opencost", or "opencost-cluster".
	PriceSource              string   `json:"priceSource,omitempty"`
	CurrentMonthlyCost       *float64 `json:"currentMonthlyCost,omitempty"`
	RecommendedMonthlyCost   *float64 `json:"recommendedMonthlyCost,omitempty"`
	TheoreticalMonthlyChange *float64 `json:"theoreticalMonthlyChange,omitempty"`
	// EstimatedMonthlySavings is a deprecated compatibility alias for
	// TheoreticalMonthlyChange. It is not evidence of realized savings.
	EstimatedMonthlySavings *float64   `json:"estimatedMonthlySavings,omitempty"`
	VerificationStatus      string     `json:"verificationStatus"`
	ConfigurationObserved   bool       `json:"configurationObserved"`
	ConfigurationObservedAt *time.Time `json:"configurationObservedAt,omitempty"`
	CheckedAt               *time.Time `json:"checkedAt,omitempty"`
	EvidenceComplete        bool       `json:"evidenceComplete"`
	Attribution             string     `json:"attribution,omitempty"`
	Currency                string     `json:"currency"`
	CreatedAt               time.Time  `json:"createdAt"`
}

type realizedSavingsAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

type savingsSummary struct {
	TheoreticalMonthlyChange        float64 `json:"theoreticalMonthlyChange"`
	RecommendedMonthlyChange        float64 `json:"recommendedMonthlyChange"`
	ApprovedMonthlyChange           float64 `json:"approvedMonthlyChange"`
	ExternallyObservedMonthlyChange float64 `json:"externallyObservedMonthlyChange"`
	VerifiedMonthlyChange           float64 `json:"verifiedMonthlyChange"`
	// Deprecated compatibility aliases. Estimated means the active,
	// approvable recommendation stage; neither field is realized savings.
	EstimatedMonthlySavings       float64                     `json:"estimatedMonthlySavings"`
	ApprovedMonthlySavings        float64                     `json:"approvedMonthlySavings"`
	Currency                      string                      `json:"currency"`
	Recommendations               int                         `json:"recommendations"`
	PricedRecommendations         int                         `json:"pricedRecommendations"`
	Truncated                     bool                        `json:"truncated"`
	VerificationOutcomesTruncated bool                        `json:"verificationOutcomesTruncated"`
	Basis                         string                      `json:"basis"`
	Realized                      realizedSavingsAvailability `json:"realized"`
}

func roundCents(value float64) float64 { return math.Round(value*100) / 100 }

// roundMilli removes floating-point noise introduced by unit conversion.
func roundMilli(value float64) float64 { return math.Round(value*1000) / 1000 }

// monthlyCost prices requested CPU and memory with the namespace's unit
// prices (OpenCost when configured, otherwise the operator's prices). ok is
// false when no CPU and memory price is known, so callers report the cost as
// unavailable instead of zero.
func (server *apiServer) monthlyCost(ctx context.Context, namespace string, cpuMilli, memoryBytes int64, replicas int32) (cost float64, source string, ok bool) {
	prices := server.pricer.Prices(ctx, namespace)
	if prices.CPUCoreHour <= 0 || prices.MemoryGiBHour <= 0 {
		return 0, prices.Source, false
	}
	perReplica := float64(cpuMilli)/1000*prices.CPUCoreHour + float64(memoryBytes)/bytesPerGiB*prices.MemoryGiBHour
	return perReplica * float64(replicas) * hoursPerMonth, prices.Source, true
}

// unpricedReason explains why a cost could not be calculated.
func (server *apiServer) unpricedReason() string {
	if server.config.OpenCostURL != "" {
		return "OpenCost has no CPU and memory prices for this namespace yet or is unreachable, and no fallback unit prices are configured"
	}
	return "CPU and memory unit prices are not configured"
}

const (
	savingsStageTheoretical        = "theoretical"
	savingsStageRecommended        = "recommended"
	savingsStageApproved           = "approved"
	savingsStageExternallyObserved = "externally_observed"
	savingsStageVerified           = "verified"
)

// computeSavings models request-based monthly changes from the newest
// non-rejected recommendation for each container. The lifecycle totals form a
// strict funnel: an item cannot be approved, observed, or verified unless it
// is also a fresh, approvable recommendation. Realized infrastructure savings
// are intentionally unavailable without billing allocation and a counterfactual.
func (server *apiServer) computeSavings(ctx context.Context, namespace string) ([]savingsItem, savingsSummary, error) {
	summary := savingsSummary{
		Currency: server.config.CostCurrency,
		Basis:    server.savingsBasis(),
		Realized: realizedSavingsAvailability{
			Available: false,
			Reason:    "realized infrastructure savings require billing allocation and a counterfactual cost baseline; neither provider is configured",
		},
	}
	items := make([]savingsItem, 0)
	recommendationIDs := make(map[string]struct{})
	now := server.now()
	for page := 1; ; page++ {
		result, err := server.repository.ListRecommendations(ctx, repository.ListOptions{
			Page: page, PageSize: savingsPageSize, Namespace: namespace, LatestOnly: true, Sort: "createdAt", Order: "desc",
		})
		if err != nil {
			return nil, savingsSummary{}, err
		}
		for _, record := range result.Items {
			if record.ReviewStatus == repository.ReviewRejected {
				continue
			}
			approvable, eligibilityReason := record.Approvable()
			stale := record.Provenance.ExpiresAt == nil || record.Expired(now)
			if stale {
				staleReason := "recommendation validity window has expired"
				if record.Provenance.ExpiresAt == nil {
					staleReason = "recommendation has no explicit validity window"
				}
				if eligibilityReason == "" {
					eligibilityReason = staleReason
				} else {
					eligibilityReason += "; " + staleReason
				}
			}
			active := approvable && !stale
			item := savingsItem{
				RecommendationID: record.ID, Cluster: record.Cluster, Namespace: record.Namespace, Kind: record.Kind,
				Workload: record.Workload, Container: record.Container, Replicas: record.Replicas, ReviewStatus: record.ReviewStatus,
				Stage: savingsStageTheoretical, Stale: stale, Approvable: approvable, EligibilityReason: eligibilityReason,
				VerificationStatus: "not_checked", Currency: server.config.CostCurrency, CreatedAt: record.CreatedAt,
			}
			currentCost, source, priced := server.monthlyCost(ctx, record.Namespace, record.CurrentCPURequestMilli, record.CurrentMemoryRequestBytes, record.Replicas)
			recommendedCost, _, _ := server.monthlyCost(ctx, record.Namespace, record.CPURequestMilli, record.MemoryRequestBytes, record.Replicas)
			switch {
			case record.Replicas <= 0:
				item.CostUnavailableReason = "the recorded replica snapshot is zero or unavailable; KubeRight will not assume one replica"
			case !priced:
				item.CostUnavailableReason = server.unpricedReason()
			default:
				current, recommended := roundCents(currentCost), roundCents(recommendedCost)
				change := roundCents(current - recommended)
				item.CostAvailable = true
				item.PriceSource = source
				item.CurrentMonthlyCost = &current
				item.RecommendedMonthlyCost = &recommended
				item.TheoreticalMonthlyChange = &change
				item.EstimatedMonthlySavings = &change
				summary.PricedRecommendations++
				summary.TheoreticalMonthlyChange += change
				if active {
					summary.RecommendedMonthlyChange += change
				}
				if active && record.ReviewStatus == repository.ReviewApproved {
					summary.ApprovedMonthlyChange += change
				}
			}
			if active {
				item.Stage = savingsStageRecommended
				if record.ReviewStatus == repository.ReviewApproved {
					item.Stage = savingsStageApproved
				}
			}
			items = append(items, item)
			recommendationIDs[record.ID] = struct{}{}
		}
		if page*savingsPageSize >= result.Total {
			break
		}
		if page*savingsPageSize >= maxSavingsRecords {
			summary.Truncated = true
			break
		}
	}

	// Load the latest durable outcomes in one bounded repository page. When the
	// outcome population exceeds the bound, missing outcomes remain unobserved
	// and the response discloses the conservative undercount.
	if len(recommendationIDs) > 0 {
		outcomes, err := server.repository.ListVerificationOutcomes(ctx, 1, maxSavingsRecords)
		if err != nil {
			return nil, savingsSummary{}, err
		}
		summary.VerificationOutcomesTruncated = outcomes.Total > len(outcomes.Items)
		byRecommendation := make(map[string]repository.VerificationOutcome, len(outcomes.Items))
		for _, outcome := range outcomes.Items {
			if _, selected := recommendationIDs[outcome.RecommendationID]; selected {
				byRecommendation[outcome.RecommendationID] = outcome
			}
		}
		for index := range items {
			item := &items[index]
			outcome, found := byRecommendation[item.RecommendationID]
			if !found {
				continue
			}
			item.VerificationStatus = outcome.Status
			item.CheckedAt = &outcome.CheckedAt
			item.EvidenceComplete = outcome.EvidenceComplete
			item.Attribution = outcome.Attribution
			exactObservation := outcome.ConfigurationObserved && outcome.FirstObservedAt != nil && outcome.Attribution == repository.VerificationAttributionExactPodUID
			if !exactObservation {
				continue
			}
			item.ConfigurationObserved = true
			item.ConfigurationObservedAt = outcome.FirstObservedAt
			if item.Stage != savingsStageApproved {
				continue
			}
			item.Stage = savingsStageExternallyObserved
			if item.TheoreticalMonthlyChange != nil {
				summary.ExternallyObservedMonthlyChange += *item.TheoreticalMonthlyChange
			}
			if outcome.Status == repository.VerificationVerified && outcome.EvidenceComplete {
				item.Stage = savingsStageVerified
				if item.TheoreticalMonthlyChange != nil {
					summary.VerifiedMonthlyChange += *item.TheoreticalMonthlyChange
				}
			}
		}
	}

	summary.TheoreticalMonthlyChange = roundCents(summary.TheoreticalMonthlyChange)
	summary.RecommendedMonthlyChange = roundCents(summary.RecommendedMonthlyChange)
	summary.ApprovedMonthlyChange = roundCents(summary.ApprovedMonthlyChange)
	summary.ExternallyObservedMonthlyChange = roundCents(summary.ExternallyObservedMonthlyChange)
	summary.VerifiedMonthlyChange = roundCents(summary.VerifiedMonthlyChange)
	summary.EstimatedMonthlySavings = summary.RecommendedMonthlyChange
	summary.ApprovedMonthlySavings = summary.ApprovedMonthlyChange
	summary.Recommendations = len(items)
	return items, summary, nil
}

func (server *apiServer) savings(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "savings", Namespace: namespace}) {
		return
	}
	if !server.config.CostConfigured() {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": 0},
			"status": Availability{Available: false, Source: "cost_model", Reason: "unit prices are not configured; set KUBERIGHT_COST_CPU_CORE_HOUR and KUBERIGHT_COST_MEMORY_GIB_HOUR"},
		})
		return
	}
	items, summary, err := server.computeSavings(r.Context(), namespace)
	if err != nil {
		server.repositoryFailure(w, r, "compute savings", err)
		return
	}
	sort.SliceStable(items, func(left, right int) bool {
		leftChange, rightChange := math.Inf(-1), math.Inf(-1)
		if items[left].TheoreticalMonthlyChange != nil {
			leftChange = *items[left].TheoreticalMonthlyChange
		}
		if items[right].TheoreticalMonthlyChange != nil {
			rightChange = *items[right].TheoreticalMonthlyChange
		}
		if leftChange != rightChange {
			return leftChange > rightChange
		}
		return items[left].RecommendationID < items[right].RecommendationID
	})
	if r.URL.Query().Get("order") == "asc" {
		for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
			items[left], items[right] = items[right], items[left]
		}
	}
	start := min((page-1)*pageSize, len(items))
	end := min(start+pageSize, len(items))
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items[start:end], "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": len(items)},
		"summary": summary,
		"status":  Availability{Available: true, Source: server.priceSourceLabel()},
	})
}

type dependencyStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Healthy    bool   `json:"healthy"`
	Critical   bool   `json:"critical"`
	Detail     string `json:"detail"`
	// State names the condition for Prometheus and forecasting, such as
	// connected, authentication_failed, or insufficient_data.
	State string `json:"state,omitempty"`
}

// Prometheus integration states.
const (
	stateConnected      = "connected"
	stateMetricsMissing = "connected_metrics_missing"
	stateAuthentication = "authentication_failed"
	stateAuthorization  = "authorization_denied"
	stateQueryInvalid   = "query_invalid"
	stateTimeout        = "timeout"
	stateUnavailable    = "unavailable"
	stateDisabled       = "disabled"
	stateReady          = "ready"
	stateNoHistory      = "insufficient_data"
)

// requiredMetrics are the metric families without which KubeRight cannot
// recommend or forecast.
var requiredMetrics = []string{"container_cpu_usage_seconds_total", "container_memory_working_set_bytes"}

func prometheusState(kind metrics.FailureKind) string {
	switch kind {
	case metrics.FailureAuthentication, metrics.FailureCredentials:
		return stateAuthentication
	case metrics.FailureAuthorization:
		return stateAuthorization
	case metrics.FailureInvalidQuery:
		return stateQueryInvalid
	case metrics.FailureTimeout:
		return stateTimeout
	default:
		return stateUnavailable
	}
}

// describePrometheusFailure says what failed and where, without the token.
func (server *apiServer) describePrometheusFailure(err error) string {
	detail := "Prometheus did not answer"
	if failure, ok := metrics.AsQueryError(err); ok {
		detail = failure.Message
		if failure.StatusCode != 0 {
			detail = fmt.Sprintf("HTTP %d: %s", failure.StatusCode, failure.Message)
		}
		if failure.ViaProxy {
			detail += "; the request went through the HTTP proxy"
		}
	}
	if host := upstreamHost(server.config.PrometheusURL); host != "" {
		detail += " (" + host + ")"
	}
	return detail
}

// cachedCoverage probes the metric families at most every 30 seconds, so
// the overview and integration pages do not repeat the queries.
func (server *apiServer) cachedCoverage(ctx context.Context) []usage.MetricCoverage {
	reporter, ok := server.usage.(coverageReporter)
	if !ok {
		return nil
	}
	server.coverageMu.Lock()
	defer server.coverageMu.Unlock()
	if server.coverage != nil && server.now().Sub(server.coverageAt) < 30*time.Second {
		return server.coverage
	}
	coverage := server.metricCoverage(ctx, reporter)
	complete := true
	for _, item := range coverage {
		if item.State == usage.CoverageUnknown && item.Failure != "" {
			complete = false
		}
	}
	if complete {
		server.coverage, server.coverageAt = coverage, server.now()
	}
	return coverage
}

// missingRequired lists required metric families Prometheus does not hold.
func missingRequired(coverage []usage.MetricCoverage) []string {
	missing := []string{}
	for _, metric := range requiredMetrics {
		for _, item := range coverage {
			if item.Metric == metric && item.State == usage.CoverageUnavailable {
				missing = append(missing, metric)
			}
		}
	}
	return missing
}

// dependencyStatuses checks every integration concurrently with a short
// per-check timeout so a slow dependency cannot stall the caller.
func (server *apiServer) dependencyStatuses(ctx context.Context) []dependencyStatus {
	statuses, _ := server.dependencyStatusesWithCoverage(ctx)
	return statuses
}

func (server *apiServer) dependencyStatusesWithCoverage(ctx context.Context) ([]dependencyStatus, []usage.MetricCoverage) {
	type check struct {
		status dependencyStatus
		probe  readinessChecker
		ready  string
		err    error
	}
	checks := []check{
		{status: dependencyStatus{Name: "Kubernetes inventory", Critical: true, Detail: "inventory provider is not configured"}, probe: server.provider, ready: "reading live workload inventory"},
		{status: dependencyStatus{Name: "Recommendation store", Critical: true}, probe: server.repository, ready: server.repository.Kind() + " store is ready"},
		{status: dependencyStatus{Name: "Prometheus", Detail: "KUBERIGHT_PROMETHEUS_URL is not set and no cluster monitoring stack was found"}, probe: server.usage, ready: server.prometheusReady()},
		{status: dependencyStatus{Name: "Forecasting", Detail: "forecasting is not configured"}, probe: server.forecaster, ready: "built-in engine " + forecast.EngineVersion + "; forecasts need Prometheus usage history"},
	}
	if server.repository.Kind() == "memory" {
		checks[1].ready = "in-process store is ready; data is lost on restart and not shared between replicas"
	}
	var wait sync.WaitGroup
	for index := range checks {
		item := &checks[index]
		if item.probe == nil {
			continue
		}
		item.status.Configured = true
		wait.Add(1)
		go func() {
			defer wait.Done()
			checkContext, cancel := context.WithTimeout(ctx, dependencyTimeout)
			defer cancel()
			if err := item.probe.Ready(checkContext); err != nil {
				item.err = err
				item.status.Detail = "configured but not responding"
				return
			}
			item.status.Healthy = true
			item.status.Detail = item.ready
		}()
	}
	wait.Wait()

	// Prometheus is healthy only when an authenticated query succeeds and
	// the metrics KubeRight needs are there; forecasting is ready only
	// when that history exists, not merely because its code is loaded.
	prometheus, forecasting := &checks[2].status, &checks[3].status
	var coverage []usage.MetricCoverage
	switch {
	case !prometheus.Configured:
		prometheus.State = stateDisabled
	case checks[2].err != nil:
		prometheus.State = prometheusState(metrics.FailureOf(checks[2].err))
		prometheus.Detail = server.describePrometheusFailure(checks[2].err)
	default:
		prometheus.State = stateConnected
		coverage = server.cachedCoverage(ctx)
		if missing := missingRequired(coverage); len(missing) > 0 {
			prometheus.Healthy, prometheus.State = false, stateMetricsMissing
			prometheus.Detail = "connected, but these metrics are missing: " + strings.Join(missing, ", ")
		}
	}
	if forecasting.Configured {
		if prometheus.State == stateConnected {
			forecasting.State = stateReady
		} else {
			forecasting.Healthy, forecasting.State = false, stateNoHistory
			forecasting.Detail = "built-in engine " + forecast.EngineVersion + " is loaded, but there is no usage history to forecast from: Prometheus is " + strings.ReplaceAll(prometheus.State, "_", " ")
		}
	}

	statuses := make([]dependencyStatus, 0, len(checks)+2)
	for _, item := range checks {
		statuses = append(statuses, item.status)
	}
	cost := dependencyStatus{Name: "Cost model", Configured: server.config.CostConfigured(), Healthy: server.config.CostConfigured(), Detail: "unit prices are not configured; savings are not estimated"}
	if cost.Configured {
		cost.Detail = "operator-configured unit prices in " + server.config.CostCurrency
	}
	statuses = append(statuses,
		cost,
		server.gitOpsStatus(),
		server.optionalIntegration("Webhooks", server.notifier != nil, "recommendation events are delivered to the configured webhook", "KUBERIGHT_WEBHOOK_URL is not set"),
		server.optionalIntegration("Latency correlation", server.config.LatencyQuery != "", "application latency is correlated with resource signals", "KUBERIGHT_LATENCY_QUERY is not set"),
		server.optionalIntegration("Recommendation metrics", server.config.ExportRecommendationMetrics, "recommendations are exported on /metrics for Grafana and other OpenMetrics tools", "KUBERIGHT_EXPORT_RECOMMENDATION_METRICS is not enabled"),
	)
	return statuses, coverage
}

func (server *apiServer) integrations(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "integrations"}) {
		return
	}
	items, coverage := server.dependencyStatusesWithCoverage(r.Context())
	if coverage == nil {
		// Prometheus did not answer, so coverage is unknown; still list the
		// families so each one shows why.
		coverage = server.cachedCoverage(r.Context())
	}
	body := map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"status": Availability{Available: true, Source: "runtime_configuration"},
	}
	if coverage != nil {
		body["metricCoverage"] = coverage
	}
	respondJSON(w, http.StatusOK, body)
}

// coverageReporter is implemented by usage sources that can say which metric
// families Prometheus holds.
type coverageReporter interface {
	MetricCoverage(context.Context) []usage.MetricCoverage
}

// metricCoverage probes metric families with the dependency timeout. Through
// a namespace-scoped (tenancy) endpoint, cluster-level series such as node
// conditions are not visible, which is noted rather than reported as absent.
func (server *apiServer) metricCoverage(ctx context.Context, reporter coverageReporter) []usage.MetricCoverage {
	checkContext, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()
	coverage := reporter.MetricCoverage(checkContext)
	if tenant := server.config.PrometheusTenantNamespace; tenant != "" {
		for index := range coverage {
			coverage[index].Note = "counted in namespace " + tenant + " only (namespace-scoped Prometheus access)"
			if coverage[index].Metric == "kube_node_status_condition" && coverage[index].State == usage.CoverageUnavailable {
				coverage[index].State = usage.CoverageUnknown
				coverage[index].Note = "node series carry no namespace, so namespace-scoped Prometheus access cannot see them"
			}
		}
	}
	return coverage
}

func (server *apiServer) inventoryCount(ctx context.Context, resource string) (int, bool) {
	if server.provider == nil {
		return 0, false
	}
	result, err := server.provider.List(ctx, resource, CollectionQuery{Page: 1, PageSize: 1})
	if err != nil || !result.Status.Available {
		return 0, false
	}
	return result.Total, true
}

// HealthSummary counts observed workload health for the overview. Workloads
// are those that own at least one pod.
type HealthSummary struct {
	Available                 bool           `json:"available"`
	Reason                    string         `json:"reason,omitempty"`
	Pods                      int            `json:"pods"`
	WorkloadsWithPods         int            `json:"workloadsWithPods"`
	HealthyWorkloads          int            `json:"healthyWorkloads"`
	WorkloadsNeedingAttention int            `json:"workloadsNeedingAttention"`
	NodesWithProblems         int            `json:"nodesWithProblems"`
	Findings                  map[string]int `json:"findings"`
	Warnings                  []string       `json:"warnings,omitempty"`
}

// HealthSummarizer is implemented by inventory providers that can count
// workload health findings in one pass.
type HealthSummarizer interface {
	HealthSummary(context.Context) (HealthSummary, error)
}

// healthSummaryTimeout bounds the pod listing behind the overview's health
// counts, which takes longer than a readiness probe on large clusters.
const healthSummaryTimeout = 10 * time.Second

func (server *apiServer) healthSummary(ctx context.Context) *HealthSummary {
	summarizer, ok := server.provider.(HealthSummarizer)
	if !ok {
		return nil
	}
	summaryContext, cancel := context.WithTimeout(ctx, healthSummaryTimeout)
	defer cancel()
	summary, err := summarizer.HealthSummary(summaryContext)
	if err != nil {
		server.logger.Warn("workload health summary failed", "request_id", requestIDFromContext(ctx), "error", err)
		return &HealthSummary{Reason: "pod inventory is unavailable or access was denied", Findings: map[string]int{}}
	}
	return &summary
}

func (server *apiServer) overview(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "overview"}) {
		return
	}
	ctx := r.Context()
	body := map[string]any{
		"version": Version,
		"mode":    server.config.OperationMode,
		"inventory": Availability{Available: server.provider != nil, Source: "kubernetes",
			Reason: availabilityReason(server.provider != nil, "inventory provider is not configured")},
		"generatedAt": server.now().UTC(),
	}
	var (
		wait         sync.WaitGroup
		mu           sync.Mutex
		counts       = map[string]int{}
		dependencies []dependencyStatus
	)
	for _, resource := range []string{"clusters", "namespaces", "workloads"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			countContext, cancel := context.WithTimeout(ctx, dependencyTimeout)
			defer cancel()
			if count, ok := server.inventoryCount(countContext, resource); ok {
				mu.Lock()
				counts[resource] = count
				mu.Unlock()
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		dependencies = server.dependencyStatuses(ctx)
	}()
	var health *HealthSummary
	wait.Add(1)
	go func() {
		defer wait.Done()
		health = server.healthSummary(ctx)
	}()

	recommendations := map[string]any{"source": server.repository.Kind()}
	total, err := server.repository.CountRecommendations(ctx)
	if err != nil {
		wait.Wait()
		server.repositoryFailure(w, r, "count recommendations", err)
		return
	}
	recommendations["total"] = total
	for _, status := range []string{repository.ReviewPending, repository.ReviewApproved, repository.ReviewRejected} {
		result, listErr := server.repository.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 1, ReviewStatus: status})
		if listErr != nil {
			wait.Wait()
			server.repositoryFailure(w, r, "count recommendations", listErr)
			return
		}
		recommendations[status] = result.Total
	}
	body["recommendations"] = recommendations
	if server.config.CostConfigured() {
		if _, summary, savingsErr := server.computeSavings(ctx, ""); savingsErr == nil {
			body["estimatedMonthlySavings"] = summary.EstimatedMonthlySavings
			body["approvedMonthlySavings"] = summary.ApprovedMonthlySavings
			body["currency"] = summary.Currency
		}
	}
	wait.Wait()
	for resource, count := range counts {
		body[resource] = count
	}
	body["dependencies"] = dependencies
	if health != nil {
		body["health"] = health
	}
	respondJSON(w, http.StatusOK, body)
}

func availabilityReason(available bool, reason string) string {
	if available {
		return ""
	}
	return reason
}

func (server *apiServer) optionalIntegration(name string, configured bool, ready, missing string) dependencyStatus {
	if configured {
		return dependencyStatus{Name: name, Configured: true, Healthy: true, Detail: ready}
	}
	return dependencyStatus{Name: name, Detail: missing}
}

func (server *apiServer) savingsBasis() string {
	prices := "operator-configured unit prices"
	if server.config.OpenCostURL != "" {
		prices = "per-namespace unit prices derived from OpenCost allocation (configured prices as fallback)"
	}
	return "requested CPU and memory x the recorded replica snapshot x " + prices + " x 730 hours; modeled changes, not billing or realized savings"
}

// priceSourceLabel names where savings prices come from.
func (server *apiServer) priceSourceLabel() string {
	if server.config.OpenCostURL != "" {
		return "opencost_unit_prices"
	}
	return "configured_unit_prices"
}

// prometheusReady names where usage history comes from. The endpoint host
// is shown; credentials never are.
func (server *apiServer) prometheusReady() string {
	detail := "usage history is queryable"
	if host := upstreamHost(server.config.PrometheusURL); host != "" {
		detail += " at " + host
	}
	if source := server.config.PrometheusSource; source != "" && source != "configured" {
		detail += " (" + source + ")"
	}
	return detail
}

func upstreamHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// parseForecastWindow bounds a forecast's lookback: 48 hours to 30 days at
// hourly steps, and up to 180 days at daily steps, so a 90-day horizon can
// be backed by as much history as it reaches ahead.
func parseForecastWindow(raw string, fallback, step time.Duration) (time.Duration, error) {
	window := fallback
	if raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return 0, errors.New("window must be a duration such as 336h")
		}
		window = parsed
	}
	maximum := 30 * 24 * time.Hour
	if step >= 24*time.Hour {
		maximum = 180 * 24 * time.Hour
	}
	if window < 48*time.Hour || window > maximum || window/step+1 > config.MaximumQueryPoints {
		return 0, fmt.Errorf("window must be between 48h and %s at %s steps", maximum, step)
	}
	return window, nil
}
