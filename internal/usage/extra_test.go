package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

func TestExtraReadsOptionalCorrelationSeries(t *testing.T) {
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		switch {
		case strings.Contains(query.Expression, "kube_node_status_condition"):
			if !strings.Contains(query.Expression, `kube_pod_info{namespace="shop",pod=~`) {
				t.Errorf("node pressure is not scoped to the workload's pods: %s", query.Expression)
			}
			return metrics.RangeResult{NoData: true}, nil
		case strings.Contains(query.Expression, "http_requests"):
			if !strings.Contains(query.Expression, `service="web.api"`) {
				t.Errorf("request-rate template not rendered: %s", query.Expression)
			}
			return metrics.RangeResult{}, metrics.ErrUpstream
		default:
			if !strings.HasPrefix(query.Expression, "sum(rate(container_cpu_usage_seconds_total{") {
				t.Errorf("unexpected query %s", query.Expression)
			}
			return seriesAt(start, 10*time.Minute, 900, 950), nil
		}
	}}
	extra, err := NewSource(fake).Extra(context.Background(), subject, Window{Duration: time.Hour, Step: 10 * time.Minute, End: end}, LatencyTemplate(`sum(rate(http_requests_total{service="$workload"}[5m]))`))
	if err != nil {
		t.Fatal(err)
	}
	if extra.CPUTotal == nil || len(extra.CPUTotal.Points) != 2 || extra.NodePressure != nil || extra.RequestRate != nil {
		t.Fatalf("extra %+v", extra)
	}
	if len(extra.Warnings) != 1 || !strings.Contains(extra.Warnings[0], "requestRate") {
		t.Fatalf("warnings %v", extra.Warnings)
	}
	// Without a template only the built-in series are queried.
	fake.expressions = nil
	if _, err := NewSource(fake).Extra(context.Background(), subject, Window{Duration: time.Hour, Step: 10 * time.Minute}, ""); err != nil || len(fake.expressions) != 2 {
		t.Fatalf("err=%v queries=%v", err, fake.expressions)
	}
	if _, err := NewSource(fake).Extra(context.Background(), Subject{}, Window{}, ""); err == nil {
		t.Fatal("invalid subjects are rejected")
	}
}
