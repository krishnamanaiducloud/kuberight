package usage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/metrics"
)

func TestMetricCoverageSeparatesMissingFromUnreachable(t *testing.T) {
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		switch {
		case strings.Contains(query.Expression, "container_cpu_usage_seconds_total"):
			return series(412), nil
		case strings.Contains(query.Expression, "container_memory_working_set_bytes"):
			return series(398), nil
		case strings.Contains(query.Expression, "container_cpu_cfs_throttled_periods_total"):
			// count() over no series returns an empty vector.
			return metrics.RangeResult{NoData: true}, nil
		case strings.Contains(query.Expression, "kube_pod_container_resource_requests"):
			return metrics.RangeResult{}, errors.New("upstream timeout")
		default:
			return series(0), nil
		}
	}}
	coverage := NewSource(fake).MetricCoverage(context.Background())
	if len(coverage) != 8 {
		t.Fatalf("coverage = %+v", coverage)
	}
	want := map[string]struct {
		state  string
		series int64
	}{
		"CPU usage":               {CoverageAvailable, 412},
		"Memory usage":            {CoverageAvailable, 398},
		"CPU throttling":          {CoverageUnavailable, 0},
		"kube-state-metrics":      {CoverageUnknown, 0},
		"Container limits":        {CoverageUnavailable, 0},
		"Restart counts":          {CoverageUnavailable, 0},
		"Last termination reason": {CoverageUnavailable, 0},
		"Node conditions":         {CoverageUnavailable, 0},
	}
	for _, item := range coverage {
		expected, ok := want[item.Signal]
		if !ok || item.State != expected.state || item.Series != expected.series {
			t.Fatalf("%s = %+v, want %+v", item.Signal, item, expected)
		}
		if item.Metric == "" || item.UsedFor == "" || item.Source == "" {
			t.Fatalf("incomplete row: %+v", item)
		}
	}
	for _, expression := range fake.expressions {
		if !strings.HasPrefix(expression, "count(last_over_time(") {
			t.Fatalf("a coverage probe is not a single-number count: %s", expression)
		}
	}
}
