package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/forecast"
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
	requestedWindow := r.URL.Query().Get("window")
	window, err := parseDuration(requestedWindow, server.config.RecommendationWindow, time.Hour)
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
	step := chartStep(window, server.config.RecommendationStep)
	history, err := server.usage.History(r.Context(), subject, usage.Window{Duration: window, Step: step, End: server.now()})
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
		status.Reason = "the forecasting service is not configured"
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
	Namespace       string  `json:"namespace"`
	Kind            string  `json:"kind"`
	Workload        string  `json:"workload"`
	Container       string  `json:"container"`
	Resource        string  `json:"resource"`
	Horizon         int     `json:"horizon,omitempty"`
	Window          string  `json:"window,omitempty"`
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
	if request.Horizon < 1 || request.Horizon > 168 {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "horizon must be between 1 and 168 steps")
		return
	}
	if request.ConfidenceLevel == 0 {
		request.ConfidenceLevel = 0.9
	}
	if request.ConfidenceLevel != 0.8 && request.ConfidenceLevel != 0.9 && request.ConfidenceLevel != 0.95 {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "confidenceLevel must be 0.8, 0.9, or 0.95")
		return
	}
	step := server.config.ForecastStep
	window, err := parseWindow(request.Window, server.config.ForecastWindow, step, 48*time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "forecasts", Namespace: request.Namespace}) {
		return
	}
	if server.forecaster == nil || server.usage == nil {
		respondError(w, r, http.StatusServiceUnavailable, "integration_not_configured", "forecasting requires both the forecasting service and Prometheus to be configured")
		return
	}
	history, err := server.usage.History(r.Context(), subject, usage.Window{Duration: window, Step: step, End: server.now()})
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	series := history.CPU
	scale := 1000.0 // Forecasting service CPU units are cores.
	if request.Resource == "memory" {
		series, scale = history.Memory, 1
	}
	if len(series.Points) < minimumForecastInput {
		respondError(w, r, http.StatusUnprocessableEntity, "insufficient_history", "at least 48 usage samples are required for a forecast; widen the window or wait for more history")
		return
	}
	if len(series.Points) > maximumForecastInput {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "the window contains more samples than the forecasting service accepts; shorten it")
		return
	}
	samples := make([]float64, len(series.Points))
	timestamps := make([]time.Time, len(series.Points))
	for index, point := range series.Points {
		samples[index] = point.Value / scale
		timestamps[index] = point.Timestamp
	}
	response, err := server.forecaster.Forecast(r.Context(), forecast.Request{
		Samples: samples, Timestamps: timestamps, Horizon: request.Horizon,
		IntervalSeconds: int(step / time.Second), Resource: request.Resource, ConfidenceLevel: request.ConfidenceLevel,
	})
	switch {
	case errors.Is(err, forecast.ErrBusy):
		w.Header().Set("Retry-After", "1")
		respondError(w, r, http.StatusTooManyRequests, "forecasting_busy", "the forecasting service is at capacity; retry shortly")
		return
	case errors.Is(err, forecast.ErrRejected):
		respondError(w, r, http.StatusUnprocessableEntity, "forecast_rejected", err.Error())
		return
	case err != nil:
		server.logger.Error("forecast failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "the forecasting service is unavailable")
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
		"warning": "Forecasts are experimental and advisory; they never change recommendations or workloads.",
	})
}

type savingsItem struct {
	RecommendationID        string    `json:"id"`
	Cluster                 string    `json:"cluster,omitempty"`
	Namespace               string    `json:"namespace"`
	Kind                    string    `json:"kind,omitempty"`
	Workload                string    `json:"workload"`
	Container               string    `json:"container"`
	Replicas                int32     `json:"replicas"`
	ReviewStatus            string    `json:"reviewStatus"`
	CurrentMonthlyCost      float64   `json:"currentMonthlyCost"`
	RecommendedMonthlyCost  float64   `json:"recommendedMonthlyCost"`
	EstimatedMonthlySavings float64   `json:"estimatedMonthlySavings"`
	Currency                string    `json:"currency"`
	CreatedAt               time.Time `json:"createdAt"`
}

type savingsSummary struct {
	EstimatedMonthlySavings float64 `json:"estimatedMonthlySavings"`
	ApprovedMonthlySavings  float64 `json:"approvedMonthlySavings"`
	Currency                string  `json:"currency"`
	Recommendations         int     `json:"recommendations"`
	Truncated               bool    `json:"truncated"`
	Basis                   string  `json:"basis"`
}

func roundCents(value float64) float64 { return math.Round(value*100) / 100 }

// roundMilli removes floating-point noise introduced by unit conversion.
func roundMilli(value float64) float64 { return math.Round(value*1000) / 1000 }

func (server *apiServer) monthlyCost(cpuMilli, memoryBytes int64, replicas int32) float64 {
	perReplica := float64(cpuMilli)/1000*server.config.CostCPUCoreHour + float64(memoryBytes)/bytesPerGiB*server.config.CostMemoryGiBHour
	return perReplica * float64(replicas) * hoursPerMonth
}

// computeSavings estimates request-based cost changes from the newest
// recommendation for each container. Rejected recommendations are excluded.
func (server *apiServer) computeSavings(ctx context.Context, namespace string) ([]savingsItem, savingsSummary, error) {
	summary := savingsSummary{
		Currency: server.config.CostCurrency,
		Basis:    "requested CPU and memory x operator-configured unit prices x 730 hours; not billing data",
	}
	items := make([]savingsItem, 0)
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
			replicas := max(record.Replicas, 1)
			current := server.monthlyCost(record.CurrentCPURequestMilli, record.CurrentMemoryRequestBytes, replicas)
			recommended := server.monthlyCost(record.CPURequestMilli, record.MemoryRequestBytes, replicas)
			item := savingsItem{
				RecommendationID: record.ID, Cluster: record.Cluster, Namespace: record.Namespace, Kind: record.Kind,
				Workload: record.Workload, Container: record.Container, Replicas: replicas, ReviewStatus: record.ReviewStatus,
				CurrentMonthlyCost: roundCents(current), RecommendedMonthlyCost: roundCents(recommended),
				EstimatedMonthlySavings: roundCents(current - recommended), Currency: server.config.CostCurrency, CreatedAt: record.CreatedAt,
			}
			summary.EstimatedMonthlySavings += current - recommended
			if record.ReviewStatus == repository.ReviewApproved {
				summary.ApprovedMonthlySavings += current - recommended
			}
			items = append(items, item)
		}
		if page*savingsPageSize >= result.Total {
			break
		}
		if page*savingsPageSize >= maxSavingsRecords {
			summary.Truncated = true
			break
		}
	}
	summary.EstimatedMonthlySavings = roundCents(summary.EstimatedMonthlySavings)
	summary.ApprovedMonthlySavings = roundCents(summary.ApprovedMonthlySavings)
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
		if items[left].EstimatedMonthlySavings != items[right].EstimatedMonthlySavings {
			return items[left].EstimatedMonthlySavings > items[right].EstimatedMonthlySavings
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
		"status":  Availability{Available: true, Source: "configured_unit_prices"},
	})
}

type dependencyStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Healthy    bool   `json:"healthy"`
	Critical   bool   `json:"critical"`
	Detail     string `json:"detail"`
}

// dependencyStatuses checks every integration concurrently with a short
// per-check timeout so a slow dependency cannot stall the caller.
func (server *apiServer) dependencyStatuses(ctx context.Context) []dependencyStatus {
	type check struct {
		status dependencyStatus
		probe  readinessChecker
		ready  string
	}
	checks := []check{
		{status: dependencyStatus{Name: "Kubernetes inventory", Critical: true, Detail: "inventory provider is not configured"}, probe: server.provider, ready: "reading live workload inventory"},
		{status: dependencyStatus{Name: "Recommendation store", Critical: true}, probe: server.repository, ready: server.repository.Kind() + " store is ready"},
		{status: dependencyStatus{Name: "Prometheus", Detail: "KUBERIGHT_PROMETHEUS_URL is not set"}, probe: server.usage, ready: "usage history is queryable"},
		{status: dependencyStatus{Name: "Forecasting service", Detail: "KUBERIGHT_ML_URL is not set"}, probe: server.forecaster, ready: "forecasting service is ready"},
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
				item.status.Detail = "configured but not responding"
				return
			}
			item.status.Healthy = true
			item.status.Detail = item.ready
		}()
	}
	wait.Wait()
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
		dependencyStatus{Name: "GitOps", Detail: "not implemented; apply approved recommendations through your own change process"},
	)
	return statuses
}

func (server *apiServer) integrations(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "integrations"}) {
		return
	}
	items := server.dependencyStatuses(r.Context())
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"status": Availability{Available: true, Source: "runtime_configuration"},
	})
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
	respondJSON(w, http.StatusOK, body)
}

func availabilityReason(available bool, reason string) string {
	if available {
		return ""
	}
	return reason
}
