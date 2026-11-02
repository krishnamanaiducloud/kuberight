package usage

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// ExtraSeries are the optional series that correlation analysis adds to a
// container's signals. Each is nil when its source metrics are absent.
type ExtraSeries struct {
	// CPUTotal is the workload's summed CPU across pods, which divided by the
	// replica count gives per-replica demand.
	CPUTotal *Series `json:"cpuTotal,omitempty"`
	// RequestRate comes from the operator's request-rate query template.
	RequestRate *Series `json:"requestRate,omitempty"`
	// NodePressure is 1 while any node running the workload reports memory,
	// disk, or PID pressure (kube-state-metrics), and 0 otherwise.
	NodePressure *Series  `json:"nodePressure,omitempty"`
	Warnings     []string `json:"warnings"`
}

// CPUTotalExpression sums CPU across all of the workload's pods, in
// millicores.
func CPUTotalExpression(subject Subject, step time.Duration) string {
	return "sum(rate(container_cpu_usage_seconds_total{" + subject.selector() + "}[" + rateWindow(step) + "])) * 1000"
}

// NodePressureExpression reports whether any node hosting the workload's
// pods is under memory, disk, or PID pressure.
func NodePressureExpression(subject Subject) string {
	pods := "namespace=" + strconv.Quote(subject.Namespace) + ",pod=~" + strconv.Quote(podPattern(subject))
	return `max(max by (node) (kube_node_status_condition{condition=~"MemoryPressure|DiskPressure|PIDPressure",status="true"}) ` +
		`* on (node) group_left() max by (node) (kube_pod_info{` + pods + `}))`
}

// Extra reads the optional correlation series concurrently. Failures and
// absent metrics become warnings; Extra itself only fails on invalid input.
func (source *Source) Extra(ctx context.Context, subject Subject, window Window, requestRate LatencyTemplate) (ExtraSeries, error) {
	if err := subject.Validate(); err != nil {
		return ExtraSeries{}, err
	}
	if window.End.IsZero() {
		window.End = source.now()
	}
	end := window.End.UTC().Truncate(time.Second)
	start := end.Add(-window.Duration)
	type query struct {
		name, unit, expression string
		target                 **Series
		result                 metrics.RangeResult
		err                    error
	}
	extra := ExtraSeries{Warnings: []string{}}
	queries := []*query{
		{name: "cpuTotal", unit: "millicores", expression: CPUTotalExpression(subject, window.Step), target: &extra.CPUTotal},
		{name: "nodePressure", unit: "flag", expression: NodePressureExpression(subject), target: &extra.NodePressure},
	}
	if requestRate != "" {
		queries = append(queries, &query{name: "requestRate", unit: "requests/s", expression: requestRate.Render(subject), target: &extra.RequestRate})
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
	for _, item := range queries {
		if item.err != nil {
			extra.Warnings = append(extra.Warnings, item.name+" series could not be read")
			continue
		}
		converted := toSeries(item.name, item.unit, item.result)
		if len(converted.Points) > 0 {
			*item.target = &converted
		}
	}
	return extra, nil
}
