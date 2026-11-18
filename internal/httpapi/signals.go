package httpapi

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/usage"
)

// Insight is a plain-language finding derived from correlated signals.
type Insight struct {
	Severity string `json:"severity"` // info, warning, or critical
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type correlations struct {
	CPUThrottling     *float64 `json:"cpuThrottling,omitempty"`
	CPUReplicas       *float64 `json:"cpuReplicas,omitempty"`
	CPULatency        *float64 `json:"cpuLatency,omitempty"`
	ThrottlingLatency *float64 `json:"throttlingLatency,omitempty"`
	MemoryLatency     *float64 `json:"memoryLatency,omitempty"`
}

func maxValue(series *usage.Series) float64 {
	peak := 0.0
	if series != nil {
		for _, point := range series.Points {
			peak = math.Max(peak, point.Value)
		}
	}
	return peak
}

// rolloutShift compares mean CPU in the hour before and after each rollout
// and returns the largest relative change.
func rolloutShift(cpu usage.Series, events []usage.Event) (time.Time, float64, bool) {
	var at time.Time
	largest := 0.0
	for _, event := range events {
		if event.Kind != usage.EventRollout {
			continue
		}
		before, beforeCount := cpu.MeanBetween(event.Timestamp.Add(-time.Hour), event.Timestamp)
		after, afterCount := cpu.MeanBetween(event.Timestamp.Add(10*time.Minute), event.Timestamp.Add(70*time.Minute))
		if beforeCount < 3 || afterCount < 3 || before <= 0 {
			continue
		}
		if change := (after - before) / before; math.Abs(change) > math.Abs(largest) {
			largest, at = change, event.Timestamp
		}
	}
	return at, largest, !at.IsZero()
}

func buildInsights(signals usage.Signals, current *ContainerSpec, found correlations, stepSeconds int64) []Insight {
	insights := []Insight{}
	if signals.Throttling != nil {
		peak := maxValue(signals.Throttling)
		switch {
		case peak >= 0.05 && found.CPUThrottling != nil && *found.CPUThrottling >= 0.5:
			insights = append(insights, Insight{"warning", "The CPU limit constrains peaks", fmt.Sprintf("Throttling rises with CPU usage (r=%.2f) and reaches %.0f%% of periods. Raise or remove the CPU limit before reducing CPU.", *found.CPUThrottling, peak*100)})
		case peak >= 0.05:
			insights = append(insights, Insight{"warning", "Short bursts hit the CPU limit", fmt.Sprintf("Up to %.0f%% of periods were throttled even though average usage stays low. Bursts shorter than the sampling interval exceed the limit.", peak*100)})
		}
	}
	if found.CPULatency != nil && *found.CPULatency >= 0.6 {
		insights = append(insights, Insight{"warning", "Latency follows CPU", fmt.Sprintf("Request latency rises with CPU usage (r=%.2f). CPU is on the critical path; prefer the conservative profile.", *found.CPULatency)})
	}
	if found.ThrottlingLatency != nil && *found.ThrottlingLatency >= 0.5 {
		insights = append(insights, Insight{"critical", "Throttling slows requests", fmt.Sprintf("Latency rises with throttling (r=%.2f). The CPU limit is degrading user-facing performance.", *found.ThrottlingLatency)})
	}
	if found.MemoryLatency != nil && *found.MemoryLatency >= 0.6 {
		insights = append(insights, Insight{"info", "Latency follows memory", fmt.Sprintf("Latency rises with memory (r=%.2f), which can indicate garbage-collection pressure.", *found.MemoryLatency)})
	}
	if found.CPUReplicas != nil && *found.CPUReplicas <= -0.5 {
		insights = append(insights, Insight{"info", "Horizontal scaling absorbs load", fmt.Sprintf("Per-pod CPU falls as replicas rise (r=%.2f). Request changes alter how the autoscaler scales.", *found.CPUReplicas)})
	}
	if trend := optimizer.Trend(signals.Memory.Values(), stepSeconds); trend.Significant && trend.SlopePerDay > 0 {
		insights = append(insights, Insight{"warning", "Memory keeps growing", fmt.Sprintf("Memory grows %.1f%% per day (R² %.2f) without returning to a baseline. Investigate a possible leak before reducing memory.", trend.GrowthPerDayPercent, trend.RSquared)})
	}
	ooms, restarts := 0, 0
	for _, event := range signals.Events {
		switch event.Kind {
		case usage.EventOOMKill:
			ooms += int(event.Count)
		case usage.EventRestart:
			restarts += int(event.Count)
		}
	}
	if ooms > 0 {
		detail := fmt.Sprintf("%d OOM kill(s) in the window.", ooms)
		if current != nil && current.MemoryLimitBytes > 0 {
			detail += fmt.Sprintf(" Peak memory reached %.0f%% of the limit.", maxValue(&signals.Memory)/float64(current.MemoryLimitBytes)*100)
		}
		insights = append(insights, Insight{"critical", "Containers were OOM-killed", detail + " Memory must not be reduced."})
	}
	if restarts > 0 {
		insights = append(insights, Insight{"info", "Containers restarted", fmt.Sprintf("%d restart(s) not caused by memory limits. Check liveness probes and application errors.", restarts)})
	}
	switch spikes := spikeProfile(signals.CPU, stepSeconds); spikes.Classification {
	case optimizer.SpikesRecurring:
		when := "at the same time each day"
		if spikes.RecurringHourUTC != nil {
			when = fmt.Sprintf("daily around %02d:00 UTC", *spikes.RecurringHourUTC)
		}
		insights = append(insights, Insight{"warning", "CPU spikes recur on a schedule", fmt.Sprintf("Spikes recur %s, last about %.0f minutes, and reach %.1fx median usage. A KEDA cron trigger or HPA can serve them without permanently larger requests.", when, spikes.MedianDurationMinutes, spikes.PeakToMedian)})
	case optimizer.SpikesFrequent:
		insights = append(insights, Insight{"warning", "Frequent CPU spikes", fmt.Sprintf("%.1f spikes per day, about %.0f minutes each, reaching %.1fx median usage. Size requests from P99 and keep any CPU limit above the largest spike.", spikes.SpikesPerDay, spikes.MedianDurationMinutes, spikes.PeakToMedian)})
	}
	if at, change, ok := rolloutShift(signals.CPU, signals.Events); ok && math.Abs(change) >= 0.25 {
		insights = append(insights, Insight{"info", "A rollout changed usage", fmt.Sprintf("CPU changed %+.0f%% after the rollout at %s. Generated recommendations use only post-rollout data when enough remains.", change*100, at.Format(time.RFC3339))})
	}
	if current != nil && current.CPURequestMilli > 0 {
		if peak := maxValue(&signals.CPU); peak > 0 && peak < float64(current.CPURequestMilli)*0.3 {
			insights = append(insights, Insight{"info", "CPU is heavily over-requested", fmt.Sprintf("Peak CPU is %.0f%% of the request across the whole window.", peak/float64(current.CPURequestMilli)*100)})
		}
	}
	return insights
}

func (server *apiServer) signals(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromQuery(r)
	if err := subject.Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error()+"; namespace, kind, workload, and container are required")
		return
	}
	window, err := parseDuration(r.URL.Query().Get("window"), 24*time.Hour, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
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
	signals, err := server.usage.Signals(r.Context(), subject, usage.Window{Duration: window, Step: step, End: server.now()}, usage.LatencyTemplate(server.config.LatencyQuery))
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	var current *ContainerSpec
	if resolver, ok := server.provider.(WorkloadResolver); ok && server.provider != nil {
		if spec, specErr := resolver.Workload(r.Context(), subject.Namespace, subject.Kind, subject.Workload); specErr == nil {
			if container, found := spec.Container(subject.Container); found {
				current = &container
			}
		}
	}
	found := correlations{
		CPUThrottling: usage.Pearson(&signals.CPU, signals.Throttling),
		CPUReplicas:   usage.Pearson(&signals.CPU, signals.Replicas),
		CPULatency:    usage.Pearson(&signals.CPU, signals.Latency),
		MemoryLatency: usage.Pearson(&signals.Memory, signals.Latency),
	}
	found.ThrottlingLatency = usage.Pearson(signals.Throttling, signals.Latency)
	respondJSON(w, http.StatusOK, map[string]any{
		"status":            Availability{Available: true, Source: "prometheus"},
		"signals":           signals,
		"current":           current,
		"correlations":      found,
		"insights":          buildInsights(signals, current, found, int64(step/time.Second)),
		"spikes":            spikeProfile(signals.CPU, int64(step/time.Second)),
		"latencyConfigured": server.config.LatencyQuery != "",
	})
}

func spikeProfile(series usage.Series, stepSeconds int64) optimizer.SpikeProfile {
	var start *time.Time
	if len(series.Points) > 0 {
		first := series.Points[0].Timestamp
		start = &first
	}
	return optimizer.SpikeAnalysis(series.Values(), stepSeconds, start)
}
