package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/correlation"
	"github.com/kuberight/kuberight/internal/forecast"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

const (
	defaultCorrelationWindow = 7 * 24 * time.Hour
	maxCorrelationWindow     = 30 * 24 * time.Hour
	forecastIntervalLevel    = 0.9
	backtestHorizon          = 24
	backtestOrigins          = 3
)

// extraSource is implemented by usage sources that can read the optional
// correlation series.
type extraSource interface {
	Extra(ctx context.Context, subject usage.Subject, window usage.Window, requestRate usage.LatencyTemplate) (usage.ExtraSeries, error)
}

// backtester is implemented by forecasters that can replay history.
type backtester interface {
	Backtest(ctx context.Context, request forecast.BacktestRequest) (forecast.BacktestResponse, error)
}

// allowed evaluates authorization without writing a response, for optional
// parts of a response.
func (server *apiServer) allowed(r *http.Request, attributes AccessAttributes) bool {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		return false
	}
	if attributes.APIGroup == "" {
		attributes.APIGroup = apiGroup
	}
	return server.access.Authorize(r.Context(), principal, attributes) == nil
}

// assessmentContext adds the pod template, HPA, namespace admission policy,
// node shapes, and demand forecast to a recommendation input. Missing
// context is reported as unknown by the assessment, never as passing.
func (server *apiServer) assessmentContext(ctx context.Context, input *optimizer.Input, spec WorkloadSpec, history usage.History) []string {
	warnings := []string{}
	input.Pod = spec.podContext()
	input.HPA = spec.hpaContext(input.Container)
	if reader, ok := server.provider.(NamespacePolicyReader); ok {
		if policy, err := reader.NamespacePolicy(ctx, spec.Namespace); err == nil {
			input.NamespacePolicy = &policy
		} else {
			warnings = append(warnings, "LimitRanges and ResourceQuotas could not be read, so admission checks are unknown")
		}
	}
	if reader, ok := server.provider.(CapacityReader); ok {
		if capacity, err := reader.Capacity(ctx); err == nil {
			nodes := make([]optimizer.NodeShape, 0, len(capacity.Nodes))
			for _, node := range capacity.Nodes {
				if node.Schedulable {
					nodes = append(nodes, optimizer.NodeShape{Name: node.Name, CPUMilli: node.CPUMilli, MemoryBytes: node.MemoryBytes})
				}
			}
			input.Nodes = nodes
		}
	}
	policy := optimizer.DefaultPolicy()
	if input.Policy != nil && input.Policy.TrendHorizonDays > 0 {
		policy.TrendHorizonDays = input.Policy.TrendHorizonDays
	}
	if forecasted, warning := server.demandForecast(ctx, history, policy.TrendHorizonDays, spec, input.Container); forecasted != nil {
		input.Forecast = forecasted
	} else if warning != "" {
		warnings = append(warnings, warning)
	}
	return warnings
}

// hourlyPeaks aggregates a series into hourly maxima, so forecasts cover
// peaks rather than averages.
func hourlyPeaks(series usage.Series, scale float64) ([]float64, []time.Time) {
	buckets := map[time.Time]float64{}
	for _, point := range series.Points {
		hour := point.Timestamp.UTC().Truncate(time.Hour)
		if value, ok := buckets[hour]; !ok || point.Value > value {
			buckets[hour] = point.Value
		}
	}
	hours := make([]time.Time, 0, len(buckets))
	for hour := range buckets {
		hours = append(hours, hour)
	}
	sort.Slice(hours, func(left, right int) bool { return hours[left].Before(hours[right]) })
	values := make([]float64, len(hours))
	for index, hour := range hours {
		values[index] = buckets[hour] * scale
	}
	return values, hours
}

func forecastPeak(response forecast.Response) (peak, upper float64) {
	for index, prediction := range response.Predictions {
		peak = math.Max(peak, prediction)
		upper = math.Max(upper, response.ConfidenceIntervals[index].Upper)
	}
	return peak, upper
}

// demandForecast asks the forecasting service for CPU and memory over the
// trend horizon. It is optional: any failure leaves sizing to history and
// trends and is reported as a warning.
func (server *apiServer) demandForecast(ctx context.Context, history usage.History, horizonDays int, spec WorkloadSpec, container string) (*optimizer.ForecastContext, string) {
	if server.forecaster == nil {
		return nil, ""
	}
	horizon := min(168, max(1, horizonDays*24))
	cpu, cpuTimes := hourlyPeaks(history.CPU, 0.001)
	memory, memoryTimes := hourlyPeaks(history.Memory, 1)
	cpuSeriesID := forecastSeriesID(spec, container, "cpu")
	cpuResponse, err := server.forecaster.Forecast(ctx, forecast.Request{
		Samples: cpu, Timestamps: cpuTimes, Horizon: horizon, IntervalSeconds: 3600, Resource: "cpu", ConfidenceLevel: forecastIntervalLevel,
		SeriesID: cpuSeriesID, Persist: cpuSeriesID != "",
	})
	if err != nil {
		return nil, "the demand forecast was unavailable (" + forecastFailure(err) + "); sizing used history and trends only"
	}
	memorySeriesID := forecastSeriesID(spec, container, "memory")
	memoryResponse, err := server.forecaster.Forecast(ctx, forecast.Request{
		Samples: memory, Timestamps: memoryTimes, Horizon: horizon, IntervalSeconds: 3600, Resource: "memory", ConfidenceLevel: forecastIntervalLevel,
		SeriesID: memorySeriesID, Persist: memorySeriesID != "",
	})
	if err != nil {
		return nil, "the memory forecast was unavailable (" + forecastFailure(err) + "); sizing used history and trends only"
	}
	cpuPeak, cpuUpper := forecastPeak(cpuResponse)
	memoryPeak, memoryUpper := forecastPeak(memoryResponse)
	result := &optimizer.ForecastContext{
		Horizon:      time.Duration(horizon) * time.Hour,
		CPUPeakMilli: cpuPeak * 1000, CPUUpperMilli: cpuUpper * 1000,
		MemoryPeakBytes: memoryPeak, MemoryUpperBytes: memoryUpper,
		IntervalLevel: forecastIntervalLevel, Model: cpuResponse.Model,
	}
	// Coverage counts only when both resources were calibrated on held-out
	// data; the weaker of the two is reported.
	if cpuResponse.Calibration != nil && memoryResponse.Calibration != nil &&
		cpuResponse.Calibration.CalibratedCoverage != nil && memoryResponse.Calibration.CalibratedCoverage != nil {
		coverage := math.Min(*cpuResponse.Calibration.CalibratedCoverage, *memoryResponse.Calibration.CalibratedCoverage)
		result.EmpiricalCoverage = &coverage
	}
	return result, ""
}

// forecastSeriesID deliberately hashes the live workload UID and its complete
// scope. The forecasting service can persist this opaque identity without
// leaking names, and a deleted/recreated workload receives a different model.
func forecastSeriesID(spec WorkloadSpec, container, resource string) string {
	if strings.TrimSpace(spec.UID) == "" || strings.TrimSpace(container) == "" || (resource != "cpu" && resource != "memory") {
		return ""
	}
	identity := strings.Join([]string{spec.Cluster, spec.Namespace, spec.Kind, spec.UID, container, resource}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return "kuberight-" + hex.EncodeToString(digest[:])
}

func forecastFailure(err error) string {
	switch {
	case errors.Is(err, forecast.ErrRejected):
		return "the history does not meet the forecasting service's requirements"
	case errors.Is(err, forecast.ErrBusy):
		return "the forecasting service is busy"
	default:
		return "the forecasting service did not respond"
	}
}

// ForecastPoint is a forecast step aligned with what actually happened.
type ForecastPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Actual    float64   `json:"actual"`
	Predicted float64   `json:"predicted"`
	Lower     float64   `json:"lower"`
	Upper     float64   `json:"upper"`
	Baseline  float64   `json:"baseline"`
}

// ForecastEvaluation is a rolling-origin backtest of the CPU forecast.
type ForecastEvaluation struct {
	Available       bool                      `json:"available"`
	Reason          string                    `json:"reason,omitempty"`
	Resource        string                    `json:"resource,omitempty"`
	Unit            string                    `json:"unit,omitempty"`
	NominalCoverage float64                   `json:"nominalCoverage,omitempty"`
	Calibrated      bool                      `json:"calibrated"`
	Overall         *forecast.Score           `json:"overall,omitempty"`
	Origins         []forecast.BacktestOrigin `json:"origins,omitempty"`
	Points          []ForecastPoint           `json:"points,omitempty"`
}

// forecastEvaluation replays the CPU history: forecasts made from past
// origins are compared with the usage that followed.
func (server *apiServer) forecastEvaluation(ctx context.Context, subject usage.Subject, end time.Time) ForecastEvaluation {
	replayer, ok := server.forecaster.(backtester)
	if server.forecaster == nil || !ok {
		return ForecastEvaluation{Reason: "the forecasting service is not configured"}
	}
	history, err := server.usage.History(ctx, subject, usage.Window{Duration: server.config.ForecastWindow, Step: time.Hour, End: end})
	if err != nil {
		return ForecastEvaluation{Reason: "CPU history for the forecast evaluation could not be read"}
	}
	cpu, times := hourlyPeaks(history.CPU, 0.001)
	needed := 48 + backtestHorizon*backtestOrigins
	if len(cpu) < needed {
		return ForecastEvaluation{Reason: "the forecast evaluation needs at least " + time.Duration(needed*int(time.Hour)).String() + " of hourly CPU history"}
	}
	response, err := replayer.Backtest(ctx, forecast.BacktestRequest{
		Samples: cpu, Horizon: backtestHorizon, Origins: backtestOrigins, IntervalSeconds: 3600, Resource: "cpu", ConfidenceLevel: forecastIntervalLevel,
	})
	if err != nil {
		return ForecastEvaluation{Reason: "the forecast evaluation failed: " + forecastFailure(err)}
	}
	evaluation := ForecastEvaluation{
		Available: true, Resource: "cpu", Unit: "millicores", NominalCoverage: response.NominalCoverage,
		Calibrated: len(response.Origins) > 0, Origins: response.Origins,
	}
	overall := response.Overall
	overall.MAE, overall.RMSE, overall.BaselineMAE = overall.MAE*1000, overall.RMSE*1000, overall.BaselineMAE*1000
	evaluation.Overall = &overall
	for _, origin := range response.Origins {
		evaluation.Calibrated = evaluation.Calibrated && origin.Calibrated
	}
	for _, point := range response.Points {
		if point.Index < 0 || point.Index >= len(times) {
			continue
		}
		evaluation.Points = append(evaluation.Points, ForecastPoint{
			Timestamp: times[point.Index], Actual: point.Actual * 1000, Predicted: point.Predicted * 1000,
			Lower: point.Lower * 1000, Upper: point.Upper * 1000, Baseline: point.Baseline * 1000,
		})
	}
	return evaluation
}

// latestRecommendation returns the newest stored recommendation for the
// container, or nil.
func (server *apiServer) latestRecommendation(ctx context.Context, subject usage.Subject) *repository.RecommendationRecord {
	result, err := server.repository.ListRecommendations(ctx, repository.ListOptions{
		Page: 1, PageSize: 100, Namespace: subject.Namespace, Workload: subject.Workload, LatestOnly: true,
	})
	if err != nil {
		return nil
	}
	for index := range result.Items {
		record := result.Items[index]
		if record.Kind == subject.Kind && record.Container == subject.Container {
			return &record
		}
	}
	return nil
}

func (server *apiServer) correlationAnalysis(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromQuery(r)
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error()+"; namespace, kind, workload, and container are required")
		return
	}
	window, err := parseDuration(r.URL.Query().Get("window"), defaultCorrelationWindow, time.Hour)
	if err != nil || window > maxCorrelationWindow {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "window must be a duration between 1h and 720h")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "metrics-history", Namespace: subject.Namespace}) {
		return
	}
	if server.usage == nil {
		respondJSON(w, http.StatusOK, map[string]any{"status": Availability{Available: false, Source: "prometheus", Reason: "Prometheus is not configured"}})
		return
	}
	step := chartStep(window, server.config.RecommendationStep)
	end := server.now().UTC().Truncate(time.Second)
	queryWindow := usage.Window{Duration: window, Step: step, End: end}
	signals, err := server.usage.Signals(r.Context(), subject, queryWindow, usage.LatencyTemplate(server.config.LatencyQuery))
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	extra := usage.ExtraSeries{Warnings: []string{}}
	if source, ok := server.usage.(extraSource); ok {
		if read, extraErr := source.Extra(r.Context(), subject, queryWindow, usage.LatencyTemplate(server.config.RequestRateQuery)); extraErr == nil {
			extra = read
		}
	}
	series := map[string]*usage.Series{
		correlation.CPU: &signals.CPU, correlation.Memory: &signals.Memory, correlation.Throttling: signals.Throttling,
		correlation.Replicas: signals.Replicas, correlation.Latency: signals.Latency, correlation.RequestRate: extra.RequestRate,
		correlation.NodePressure: extra.NodePressure, "cpuTotal": extra.CPUTotal,
	}
	report := correlation.Analyze(correlation.Input{Start: signals.Start, End: signals.End, Step: step, Series: series, Events: signals.Events})

	var current *ContainerSpec
	if resolver, ok := server.provider.(WorkloadResolver); ok && server.provider != nil {
		if spec, specErr := resolver.Workload(r.Context(), subject.Namespace, subject.Kind, subject.Workload); specErr == nil {
			if container, found := spec.Container(subject.Container); found {
				current = &container
			}
		}
	}
	response := map[string]any{
		"status":     Availability{Available: true, Source: "prometheus"},
		"signals":    signals,
		"extra":      extra,
		"current":    current,
		"report":     report,
		"forecast":   server.forecastEvaluation(r.Context(), subject, end),
		"warnings":   append(append([]string{}, signals.Warnings...), extra.Warnings...),
		"configured": map[string]bool{"latency": server.config.LatencyQuery != "", "requestRate": server.config.RequestRateQuery != ""},
	}
	if server.allowed(r, AccessAttributes{Verb: "list", Resource: "recommendations", Namespace: subject.Namespace}) {
		if record := server.latestRecommendation(r.Context(), subject); record != nil {
			response["recommendation"] = record
			if record.ReviewStatus == repository.ReviewApproved {
				// Correlation is a GET operation. It may expose an already durable
				// outcome, but must never refresh evidence or send notifications.
				if verdict, verificationErr := server.repository.GetVerificationOutcome(r.Context(), record.ID); verificationErr == nil {
					response["verification"] = verdict
				}
			}
		}
	}
	respondJSON(w, http.StatusOK, response)
}
