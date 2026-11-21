package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

// Chart ranges. Recommendation windows keep their own, narrower bounds
// (1h-720h) because they size requests.
const (
	minimumChartRange = time.Minute
	maximumChartRange = 365 * 24 * time.Hour
	// minimumChartStep stays above common 15s-30s scrape intervals.
	minimumChartStep = 15 * time.Second
	// fineStepRange is the range below which charts use steps finer than
	// the configured recommendation step.
	fineStepRange = 12 * time.Hour
)

// chartRange is a validated, UTC query range for usage charts.
type chartRange struct {
	start, end time.Time
	duration   time.Duration
	step       time.Duration
	// custom is set when the caller gave explicit start and end times.
	custom bool
}

func (span chartRange) window() usage.Window {
	return usage.Window{Duration: span.duration, Step: span.step, End: span.end}
}

// parseChartRange reads either window (a Go duration ending now) or start
// and end (RFC 3339, any offset; evaluated in UTC). The range must be between
// one minute and 365 days and may not end in the future. The step keeps a
// chart at about historyChartPoints points: never finer than 15s, and, for
// ranges of 12h or more, never finer than the configured step, so long ranges
// stay within Prometheus's point limit.
func parseChartRange(r *http.Request, fallback, configuredStep time.Duration, now time.Time) (chartRange, error) {
	query := r.URL.Query()
	now = now.UTC().Truncate(time.Second)
	span := chartRange{end: now}
	rawStart, rawEnd := query.Get("start"), query.Get("end")
	switch {
	case rawStart != "" || rawEnd != "":
		if rawStart == "" || rawEnd == "" {
			return chartRange{}, errors.New("start and end must be given together")
		}
		if query.Get("window") != "" {
			return chartRange{}, errors.New("give either window or start and end, not both")
		}
		start, err := time.Parse(time.RFC3339, rawStart)
		if err != nil {
			return chartRange{}, errors.New("start must be an RFC 3339 timestamp such as 2026-11-01T00:00:00Z")
		}
		end, err := time.Parse(time.RFC3339, rawEnd)
		if err != nil {
			return chartRange{}, errors.New("end must be an RFC 3339 timestamp such as 2026-11-08T00:00:00Z")
		}
		start, end = start.UTC().Truncate(time.Second), end.UTC().Truncate(time.Second)
		if end.After(now.Add(time.Minute)) {
			return chartRange{}, errors.New("end cannot be in the future")
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) {
			return chartRange{}, errors.New("end must be after start")
		}
		span.start, span.end, span.duration, span.custom = start, end, end.Sub(start), true
	default:
		span.duration = fallback
		if raw := query.Get("window"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return chartRange{}, errors.New("window must be a duration such as 24h")
			}
			span.duration = parsed
		}
		span.start = span.end.Add(-span.duration)
	}
	if span.duration < minimumChartRange || span.duration > maximumChartRange {
		return chartRange{}, fmt.Errorf("the range must be between %s and %s (365 days)", minimumChartRange, maximumChartRange)
	}
	minimum := configuredStep
	if span.duration < fineStepRange {
		minimum = min(configuredStep, max(minimumChartStep, span.duration/historyChartPoints))
	}
	span.step = chartStep(span.duration, minimum)
	return span, nil
}

// dataAvailability says when returned samples begin. A range that starts
// before Prometheus has data (retention, or before the workload ran) is
// reported instead of presented as complete history.
func dataAvailability(span chartRange, series ...usage.Series) map[string]any {
	var first *time.Time
	for _, item := range series {
		if len(item.Points) > 0 && (first == nil || item.Points[0].Timestamp.Before(*first)) {
			at := item.Points[0].Timestamp.UTC()
			first = &at
		}
	}
	availability := map[string]any{"requestedStart": span.start, "requestedEnd": span.end}
	if first == nil {
		availability["note"] = "Prometheus returned no samples in this range"
		return availability
	}
	availability["dataStart"] = *first
	if first.Sub(span.start) > max(2*span.step, 10*time.Minute) {
		availability["note"] = "Prometheus has samples for this container from " + first.Format(time.RFC3339) +
			"; the earlier part of the range is before the workload ran or beyond the data source's retention"
	}
	return availability
}
