package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// Event kinds on a signal timeline.
const (
	EventRestart = "restart"
	EventOOMKill = "oom_kill"
	EventRollout = "rollout"
)

// Event is a point-in-time occurrence that can explain a change in usage.
type Event struct {
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
	Count     int64     `json:"count,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Signals are the series and events for one container on a shared time axis.
// Optional series are nil when their source metrics are absent.
type Signals struct {
	Subject    Subject   `json:"subject"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Step       string    `json:"step"`
	CPU        Series    `json:"cpu"`
	Memory     Series    `json:"memory"`
	Throttling *Series   `json:"throttling,omitempty"`
	Replicas   *Series   `json:"replicas,omitempty"`
	Latency    *Series   `json:"latency,omitempty"`
	Events     []Event   `json:"events"`
	Warnings   []string  `json:"warnings"`
}

// LatencyTemplate is an operator-supplied PromQL expression with $namespace,
// $workload, and $container placeholders, for example a p95 request latency
// in seconds from a service mesh or HTTP server histogram.
type LatencyTemplate string

var templatePlaceholders = regexp.MustCompile(`\$(namespace|workload|container)\b`)

// Validate requires at least one placeholder so the series is scoped.
func (template LatencyTemplate) Validate() error {
	text := string(template)
	if text == "" {
		return nil
	}
	if len(text) > 2048 || strings.ContainsAny(text, "\r\n") {
		return errors.New("latency query must be a single line of at most 2048 bytes")
	}
	if !templatePlaceholders.MatchString(text) {
		return errors.New("latency query must use $namespace, $workload, or $container")
	}
	return nil
}

// Render substitutes validated names. Subject validation restricts names to
// DNS-1123 characters, so substitution cannot change the expression's
// structure inside a quoted matcher.
func (template LatencyTemplate) Render(subject Subject) string {
	return strings.NewReplacer("$namespace", subject.Namespace, "$workload", subject.Workload, "$container", subject.Container).Replace(string(template))
}

// ThrottlingRatioExpression returns the throttled share of CFS periods at
// each step.
func ThrottlingRatioExpression(subject Subject, step time.Duration) string {
	selector := subject.selector()
	window := rateWindow(step)
	return "sum(rate(container_cpu_cfs_throttled_periods_total{" + selector + "}[" + window + "])) / sum(rate(container_cpu_cfs_periods_total{" + selector + "}[" + window + "]))"
}

// ReplicasExpression counts pods that reported CPU usage at each step.
func ReplicasExpression(subject Subject, step time.Duration) string {
	return "count(sum by (pod) (rate(container_cpu_usage_seconds_total{" + subject.selector() + "}[" + rateWindow(step) + "])))"
}

// Signals reads every series concurrently. Only CPU or memory failures are
// errors; optional series that fail are reported as warnings.
func (source *Source) Signals(ctx context.Context, subject Subject, window Window, latency LatencyTemplate) (Signals, error) {
	if err := subject.Validate(); err != nil {
		return Signals{}, err
	}
	if window.End.IsZero() {
		window.End = source.now()
	}
	end := window.End.UTC().Truncate(time.Second)
	start := end.Add(-window.Duration)
	signals := Signals{Subject: subject, Start: start, End: end, Step: window.Step.String(), Events: []Event{}, Warnings: []string{}}
	type query struct {
		name, unit, expression string
		required               bool
		result                 metrics.RangeResult
		err                    error
	}
	queries := []*query{
		{name: "cpu", unit: "millicores", expression: CPUExpression(subject, window.Step), required: true},
		{name: "memory", unit: "bytes", expression: MemoryExpression(subject, window.Step), required: true},
		{name: "throttling", unit: "ratio", expression: ThrottlingRatioExpression(subject, window.Step)},
		{name: "replicas", unit: "pods", expression: ReplicasExpression(subject, window.Step)},
		{name: "restarts", unit: "count", expression: "sum(increase(kube_pod_container_status_restarts_total{" + subject.selector() + "}[" + promDuration(window.Step) + "]))"},
		{name: "oom", unit: "flag", expression: "max(kube_pod_container_status_last_terminated_reason{" + subject.selector() + `,reason="OOMKilled"})`},
	}
	if latency != "" {
		queries = append(queries, &query{name: "latency", unit: "seconds", expression: latency.Render(subject)})
	}
	var wait sync.WaitGroup
	for _, item := range queries {
		wait.Add(1)
		go func() {
			defer wait.Done()
			item.result, item.err = source.client.QueryRange(ctx, metrics.RangeQuery{Expression: item.expression, Start: start, End: end, Step: window.Step})
		}()
	}
	wait.Wait()

	series := map[string]*Series{}
	for _, item := range queries {
		if item.err != nil {
			if item.required {
				return Signals{}, fmt.Errorf("%s query: %w", item.name, item.err)
			}
			signals.Warnings = append(signals.Warnings, item.name+" series could not be read")
			continue
		}
		signals.Warnings = append(signals.Warnings, item.result.Warnings...)
		converted := toSeries(item.name, item.unit, item.result)
		if len(converted.Points) > 0 || item.required {
			series[item.name] = &converted
		}
	}
	signals.CPU, signals.Memory = *series["cpu"], *series["memory"]
	if len(signals.CPU.Points) == 0 && len(signals.Memory.Points) == 0 {
		return signals, ErrNoData
	}
	signals.Throttling, signals.Replicas, signals.Latency = series["throttling"], series["replicas"], series["latency"]

	oomAt := map[time.Time]bool{}
	if oom := series["oom"]; oom != nil {
		for _, point := range oom.Points {
			oomAt[point.Timestamp] = point.Value >= 1
		}
	}
	if restarts := series["restarts"]; restarts != nil {
		for _, point := range restarts.Points {
			count := int64(math.Round(point.Value))
			if count < 1 {
				continue
			}
			kind := EventRestart
			if oomAt[point.Timestamp] {
				kind = EventOOMKill
			}
			signals.Events = append(signals.Events, Event{Timestamp: point.Timestamp, Kind: kind, Count: count})
		}
	}
	if subject.Kind == "Deployment" {
		rollouts, err := source.Rollouts(ctx, subject, start, end)
		if err != nil {
			signals.Warnings = append(signals.Warnings, "rollout history could not be read")
		}
		signals.Events = append(signals.Events, rollouts...)
	}
	sort.SliceStable(signals.Events, func(left, right int) bool {
		return signals.Events[left].Timestamp.Before(signals.Events[right].Timestamp)
	})
	return signals, nil
}

// Rollouts returns Deployment rollouts in [start, end] from the creation
// times of its ReplicaSets (kube-state-metrics). Only Deployments create
// ReplicaSets, so other kinds return none.
func (source *Source) Rollouts(ctx context.Context, subject Subject, start, end time.Time) ([]Event, error) {
	if subject.Kind != "Deployment" {
		return nil, nil
	}
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	expression := "max by (replicaset) (kube_replicaset_created{namespace=" + strconv.Quote(subject.Namespace) + ",replicaset=~" + strconv.Quote(regexp.QuoteMeta(subject.Workload)+"-[a-z0-9]{1,10}") + "})"
	values, err := source.instantVector(ctx, expression, end, "replicaset")
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(values))
	for replicaSet, created := range values {
		at := time.Unix(int64(created), 0).UTC()
		if at.Before(start) || at.After(end) {
			continue
		}
		events = append(events, Event{Timestamp: at, Kind: EventRollout, Detail: replicaSet})
	}
	sort.Slice(events, func(left, right int) bool { return events[left].Timestamp.Before(events[right].Timestamp) })
	return events, nil
}

// Pearson returns the correlation of two series over their shared
// timestamps, or nil with fewer than ten shared points or no variance.
func Pearson(left, right *Series) *float64 {
	if left == nil || right == nil {
		return nil
	}
	values := make(map[time.Time]float64, len(left.Points))
	for _, point := range left.Points {
		values[point.Timestamp] = point.Value
	}
	var xs, ys []float64
	for _, point := range right.Points {
		if value, ok := values[point.Timestamp]; ok {
			xs = append(xs, value)
			ys = append(ys, point.Value)
		}
	}
	if len(xs) < 10 {
		return nil
	}
	var sumX, sumY, sumXY, sumXX, sumYY float64
	for index := range xs {
		sumX += xs[index]
		sumY += ys[index]
		sumXY += xs[index] * ys[index]
		sumXX += xs[index] * xs[index]
		sumYY += ys[index] * ys[index]
	}
	count := float64(len(xs))
	denominator := math.Sqrt((count*sumXX - sumX*sumX) * (count*sumYY - sumY*sumY))
	if denominator == 0 || math.IsNaN(denominator) {
		return nil
	}
	correlation := math.Round((count*sumXY-sumX*sumY)/denominator*100) / 100
	return &correlation
}

// Since returns the series restricted to points at or after t.
func (series Series) Since(t time.Time) Series {
	trimmed := series
	trimmed.Points = make([]Point, 0, len(series.Points))
	for _, point := range series.Points {
		if !point.Timestamp.Before(t) {
			trimmed.Points = append(trimmed.Points, point)
		}
	}
	return trimmed
}

// MeanBetween averages the points in [from, to).
func (series Series) MeanBetween(from, to time.Time) (float64, int) {
	sum, count := 0.0, 0
	for _, point := range series.Points {
		if !point.Timestamp.Before(from) && point.Timestamp.Before(to) {
			sum += point.Value
			count++
		}
	}
	if count == 0 {
		return 0, 0
	}
	return sum / float64(count), count
}

// ThrottlingPeakExpression is the P95 of the per-interval throttling ratio
// over the lookback, so throttling concentrated in busy hours is visible even
// when the window average is low.
func ThrottlingPeakExpression(subject Subject, lookback, step time.Duration) string {
	return "quantile_over_time(0.95, (" + ThrottlingRatioExpression(subject, step) + ")[" + promDuration(lookback) + ":" + promDuration(step) + "])"
}
