// Package usage reads container resource usage history from Prometheus for a
// single workload container. It builds every PromQL expression from validated
// Kubernetes names, so request input can never alter query structure.
package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

var (
	// ErrInvalidSubject indicates a namespace, workload, kind, or container
	// that is not a valid Kubernetes name.
	ErrInvalidSubject = errors.New("invalid usage subject")
	// ErrNoData indicates that Prometheus returned no series for the subject.
	ErrNoData = errors.New("no usage data was found for the workload container")

	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// podSuffixes match the names controllers give to pods. Prometheus anchors
// regular-expression matchers, so each pattern matches a whole pod name.
var podSuffixes = map[string]string{
	"Deployment":  `-[a-z0-9]{1,10}-[a-z0-9]{5}`,
	"StatefulSet": `-[0-9]+`,
	"DaemonSet":   `-[a-z0-9]{5}`,
	"ReplicaSet":  `-[a-z0-9]{5}`,
	"Job":         `-[a-z0-9]{5}`,
	"CronJob":     `-[0-9]+-[a-z0-9]{5}`,
}

// SupportedKinds lists the workload kinds whose pods can be matched.
func SupportedKinds() []string {
	return []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob"}
}

// Subject identifies one container of one workload.
type Subject struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
}

// Validate rejects names that are not valid Kubernetes object names.
func (subject Subject) Validate() error {
	if len(subject.Namespace) > 63 || !dnsLabel.MatchString(subject.Namespace) {
		return fmt.Errorf("%w: namespace must be a DNS-1123 label", ErrInvalidSubject)
	}
	if len(subject.Workload) > 253 || !dnsSubdomain.MatchString(subject.Workload) {
		return fmt.Errorf("%w: workload must be a DNS-1123 subdomain", ErrInvalidSubject)
	}
	if len(subject.Container) > 63 || !dnsLabel.MatchString(subject.Container) {
		return fmt.Errorf("%w: container must be a DNS-1123 label", ErrInvalidSubject)
	}
	if _, ok := podSuffixes[subject.Kind]; !ok {
		return fmt.Errorf("%w: kind must be one of Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, or CronJob", ErrInvalidSubject)
	}
	return nil
}

// selector renders the label matchers for the subject's containers. The
// values are validated names rendered through strconv.Quote, which matches
// PromQL's string-literal escaping.
func (subject Subject) selector() string {
	pods := regexp.QuoteMeta(subject.Workload) + podSuffixes[subject.Kind]
	return "namespace=" + strconv.Quote(subject.Namespace) + ",pod=~" + strconv.Quote(pods) + ",container=" + strconv.Quote(subject.Container)
}

// Window is a query range ending at End.
type Window struct {
	Duration time.Duration
	Step     time.Duration
	End      time.Time
}

// Series is a normalized usage series. CPU values are millicores and memory
// values are bytes. Missing points are omitted, never zero-filled.
type Series struct {
	Resource string              `json:"resource"`
	Unit     string              `json:"unit"`
	Points   []Point             `json:"points"`
	Quality  metrics.DataQuality `json:"quality"`
}

// Point is one observation.
type Point struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// Values returns the series values in time order.
func (series Series) Values() []float64 {
	values := make([]float64, len(series.Points))
	for index, point := range series.Points {
		values[index] = point.Value
	}
	return values
}

// QualityIssues renders blocking quality problems as human-readable text.
// Duplicate and anomalous samples were already dropped by normalization and
// are therefore reported but not blocking.
func (series Series) QualityIssues() (blocking []string, informational []string) {
	for _, issue := range series.Quality.Issues {
		switch issue {
		case metrics.IssueSparse:
			blocking = append(blocking, fmt.Sprintf("%s history covers %.0f%% of the expected samples", series.Resource, series.Quality.Coverage*100))
		case metrics.IssueStale:
			blocking = append(blocking, series.Resource+" history is stale; the workload may no longer be running")
		case metrics.IssueMissing:
			blocking = append(blocking, series.Resource+" history is missing")
		case metrics.IssueDuplicate:
			informational = append(informational, fmt.Sprintf("%d duplicate %s samples were discarded", series.Quality.DuplicateSamples, series.Resource))
		case metrics.IssueAnomalous:
			informational = append(informational, fmt.Sprintf("%d malformed or out-of-range %s samples were discarded", series.Quality.AnomalousSamples, series.Resource))
		}
	}
	return blocking, informational
}

// History holds CPU and memory usage for one container.
type History struct {
	Subject  Subject   `json:"subject"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Step     string    `json:"step"`
	CPU      Series    `json:"cpu"`
	Memory   Series    `json:"memory"`
	Warnings []string  `json:"warnings"`
}

// Events counts restarts and OOM kills over a window. Available is false
// when kube-state-metrics series are absent; counts are then unknown, not
// zero.
type Events struct {
	Available bool  `json:"available"`
	Restarts  int64 `json:"restarts"`
	OOMKills  int64 `json:"oomKills"`
	// ThrottlingAvailable is false when the container has no CPU limit (no
	// CFS quota) or cAdvisor throttling series are absent.
	ThrottlingAvailable bool    `json:"throttlingAvailable"`
	ThrottlingRatio     float64 `json:"throttlingRatio"`
}

// RangeQuerier is the subset of the Prometheus client used by Source.
type RangeQuerier interface {
	QueryRange(context.Context, metrics.RangeQuery) (metrics.RangeResult, error)
}

// Source reads usage from Prometheus.
type Source struct {
	client RangeQuerier
	now    func() time.Time
}

// NewSource wraps a Prometheus range client.
func NewSource(client RangeQuerier) *Source {
	return &Source{client: client, now: time.Now}
}

// rateWindow covers at least two typical 30-second scrapes so rate() has
// enough samples, while matching coarse steps to avoid skipping data.
func rateWindow(step time.Duration) string {
	return promDuration(max(step, 2*time.Minute))
}

func promDuration(duration time.Duration) string {
	return strconv.FormatInt(int64(math.Ceil(duration.Seconds())), 10) + "s"
}

// CPUExpression returns the per-step maximum CPU usage across the workload's
// pods, in millicores. Using the busiest replica keeps sizing conservative.
func CPUExpression(subject Subject, step time.Duration) string {
	return "max(rate(container_cpu_usage_seconds_total{" + subject.selector() + "}[" + rateWindow(step) + "])) * 1000"
}

// MemoryExpression returns the per-step peak working set across pods. The
// inner max_over_time captures peaks that fall between evaluation steps.
func MemoryExpression(subject Subject, step time.Duration) string {
	return "max(max_over_time(container_memory_working_set_bytes{" + subject.selector() + "}[" + promDuration(step) + "]))"
}

// ThrottlingExpression returns the share of CFS scheduling periods in which
// the workload's containers were throttled over the lookback.
func ThrottlingExpression(subject Subject, lookback time.Duration) string {
	selector := subject.selector()
	window := promDuration(lookback)
	return "sum(increase(container_cpu_cfs_throttled_periods_total{" + selector + "}[" + window + "])) / sum(increase(container_cpu_cfs_periods_total{" + selector + "}[" + window + "]))"
}

// History reads CPU and memory usage concurrently.
func (source *Source) History(ctx context.Context, subject Subject, window Window) (History, error) {
	if err := subject.Validate(); err != nil {
		return History{}, err
	}
	if window.End.IsZero() {
		window.End = source.now()
	}
	window.End = window.End.UTC().Truncate(time.Second)
	start := window.End.Add(-window.Duration)
	queries := []struct {
		resource, unit, expression string
		destination                *Series
	}{
		{"cpu", "millicores", CPUExpression(subject, window.Step), nil},
		{"memory", "bytes", MemoryExpression(subject, window.Step), nil},
	}
	history := History{Subject: subject, Start: start, End: window.End, Step: window.Step.String(), Warnings: []string{}}
	queries[0].destination = &history.CPU
	queries[1].destination = &history.Memory

	var wait sync.WaitGroup
	errs := make([]error, len(queries))
	warnings := make([][]string, len(queries))
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			query := queries[index]
			result, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: query.expression, Start: start, End: window.End, Step: window.Step})
			if err != nil {
				errs[index] = fmt.Errorf("%s usage query: %w", query.resource, err)
				return
			}
			warnings[index] = result.Warnings
			*query.destination = toSeries(query.resource, query.unit, result)
		}(index)
	}
	wait.Wait()
	if err := errors.Join(errs...); err != nil {
		return History{}, err
	}
	for _, items := range warnings {
		history.Warnings = append(history.Warnings, items...)
	}
	if len(history.CPU.Points) == 0 && len(history.Memory.Points) == 0 {
		return history, ErrNoData
	}
	return history, nil
}

func toSeries(resource, unit string, result metrics.RangeResult) Series {
	series := Series{Resource: resource, Unit: unit, Points: []Point{}}
	if result.NoData || len(result.Series) == 0 {
		series.Quality = metrics.DataQuality{Issues: []metrics.QualityIssue{metrics.IssueMissing}}
		return series
	}
	// The expressions aggregate with max(), so Prometheus returns one series.
	normalized := result.Series[0]
	series.Quality = normalized.Quality
	for _, sample := range normalized.Samples {
		series.Points = append(series.Points, Point{Timestamp: sample.Timestamp, Value: sample.Value})
	}
	return series
}

// Events counts container restarts and OOM kills over the window using
// kube-state-metrics series. A single-point range query acts as an instant
// query evaluated at the window end.
func (source *Source) Events(ctx context.Context, subject Subject, window Window) (Events, error) {
	if err := subject.Validate(); err != nil {
		return Events{}, err
	}
	if window.End.IsZero() {
		window.End = source.now()
	}
	end := window.End.UTC().Truncate(time.Second)
	lookback := promDuration(window.Duration)
	restarts, restartsFound, err := source.instant(ctx, "sum(increase(kube_pod_container_status_restarts_total{"+subject.selector()+"}["+lookback+"]))", end)
	if err != nil {
		return Events{}, fmt.Errorf("restart query: %w", err)
	}
	oom, oomFound, err := source.instant(ctx, "sum(max_over_time(kube_pod_container_status_last_terminated_reason{"+subject.selector()+`,reason="OOMKilled"}[`+lookback+"]))", end)
	if err != nil {
		return Events{}, fmt.Errorf("OOM query: %w", err)
	}
	events := Events{Available: restartsFound}
	if restartsFound {
		events.Restarts = int64(math.Round(math.Max(restarts, 0)))
	}
	if oomFound {
		events.OOMKills = int64(math.Round(math.Max(oom, 0)))
	}
	// A zero denominator yields NaN, which normalization drops, so a container
	// without CFS periods reports throttling as unavailable rather than zero.
	ratio, ratioFound, err := source.instant(ctx, ThrottlingExpression(subject, window.Duration), end)
	if err != nil {
		return Events{}, fmt.Errorf("throttling query: %w", err)
	}
	if ratioFound {
		events.ThrottlingAvailable = true
		events.ThrottlingRatio = math.Min(math.Max(ratio, 0), 1)
	}
	return events, nil
}

func (source *Source) instant(ctx context.Context, expression string, at time.Time) (float64, bool, error) {
	result, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: expression, Start: at, End: at, Step: time.Minute})
	if err != nil {
		return 0, false, err
	}
	if result.NoData || len(result.Series) == 0 || len(result.Series[0].Samples) == 0 {
		return 0, false, nil
	}
	samples := result.Series[0].Samples
	return samples[len(samples)-1].Value, true, nil
}

// Ready evaluates a constant expression to confirm Prometheus answers.
func (source *Source) Ready(ctx context.Context) error {
	now := source.now().UTC().Truncate(time.Second)
	_, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: "vector(1)", Start: now, End: now, Step: time.Minute})
	return err
}
