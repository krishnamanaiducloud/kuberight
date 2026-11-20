package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// Coverage states. Unknown means Prometheus could not answer, which is not
// the same as the metric being absent.
const (
	CoverageAvailable   = "available"
	CoverageUnavailable = "unavailable"
	CoverageUnknown     = "unknown"
)

// coverageLookback is how recently a series must have been scraped to count.
const coverageLookback = 10 * time.Minute

// MetricCoverage reports whether one family of metrics that KubeRight reads
// is present in Prometheus, so missing data is shown as missing, never as zero.
type MetricCoverage struct {
	Signal  string `json:"signal"`
	Metric  string `json:"metric"`
	Source  string `json:"source"`
	State   string `json:"state"`
	Series  int64  `json:"series"`
	UsedFor string `json:"usedFor"`
	Note    string `json:"note,omitempty"`
	// Failure says why the family could not be checked, such as timeout or
	// authentication_failed; empty when Prometheus answered.
	Failure string `json:"failure,omitempty"`
}

type coverageProbe struct {
	signal, metric, selector, source, usedFor string
}

var coverageProbes = []coverageProbe{
	{"CPU usage", "container_cpu_usage_seconds_total", `{container!="",container!="POD"}`, "cAdvisor (kubelet)", "CPU recommendations, hotspots, forecasts"},
	{"Memory usage", "container_memory_working_set_bytes", `{container!="",container!="POD"}`, "cAdvisor (kubelet)", "memory recommendations, OOM safeguards, forecasts"},
	{"CPU throttling", "container_cpu_cfs_throttled_periods_total", `{container!="",container!="POD"}`, "cAdvisor (kubelet)", "throttling evidence and CPU hotspots"},
	{"kube-state-metrics", "kube_pod_container_resource_requests", "", "kube-state-metrics", "attributing usage to workloads, requests and limits, restarts and OOM history"},
	{"Container limits", "kube_pod_container_resource_limits", "", "kube-state-metrics", "limit headroom in recommendations"},
	{"Restart counts", "kube_pod_container_status_restarts_total", "", "kube-state-metrics", "restart anomalies and post-change verification"},
	{"Last termination reason", "kube_pod_container_status_last_terminated_reason", "", "kube-state-metrics", "OOM kill history and post-change verification"},
	{"Node conditions", "kube_node_status_condition", "", "kube-state-metrics", "node pressure in correlation analysis"},
}

// CoverageExpression counts the series of one metric scraped recently. It
// returns one number, however many series exist.
func CoverageExpression(metric, selector string) string {
	return "count(last_over_time(" + metric + selector + "[" + promDuration(coverageLookback) + "]))"
}

// MetricCoverage probes each metric family concurrently. A failed query is
// reported as unknown rather than unavailable.
func (source *Source) MetricCoverage(ctx context.Context) []MetricCoverage {
	at := source.now().UTC().Truncate(time.Second)
	results := make([]MetricCoverage, len(coverageProbes))
	var wait sync.WaitGroup
	for index, probe := range coverageProbes {
		results[index] = MetricCoverage{Signal: probe.signal, Metric: probe.metric, Source: probe.source, UsedFor: probe.usedFor, State: CoverageUnknown}
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := source.client.QueryRange(ctx, metrics.RangeQuery{Name: "coverage." + probe.metric, Expression: CoverageExpression(probe.metric, probe.selector), Start: at, End: at, Step: time.Minute})
			item := &results[index]
			if err != nil {
				item.Failure = string(metrics.FailureOf(err))
				item.Note = "could not be checked: Prometheus " + strings.ReplaceAll(item.Failure, "_", " ")
				return
			}
			item.State = CoverageUnavailable
			if response.NoData || len(response.Series) == 0 || len(response.Series[0].Samples) == 0 {
				return
			}
			samples := response.Series[0].Samples
			if count := int64(samples[len(samples)-1].Value); count > 0 {
				item.State, item.Series = CoverageAvailable, count
			}
		}()
	}
	wait.Wait()
	return results
}
