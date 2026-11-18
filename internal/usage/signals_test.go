package usage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

func seriesAt(start time.Time, step time.Duration, values ...float64) metrics.RangeResult {
	samples := make([]metrics.Sample, len(values))
	for index, value := range values {
		samples[index] = metrics.Sample{Timestamp: start.Add(time.Duration(index) * step), Value: value}
	}
	return metrics.RangeResult{Series: []metrics.Series{{Samples: samples, Quality: metrics.DataQuality{Issues: []metrics.QualityIssue{}, Coverage: 1}}}}
}

func TestSignalsCombineSeriesAndClassifyEvents(t *testing.T) {
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)
	rolloutAt := start.Add(30 * time.Minute)
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		switch {
		case strings.Contains(query.Expression, "kube_replicaset_created"):
			return metrics.RangeResult{Series: []metrics.Series{
				{Labels: map[string]string{"replicaset": "web.api-7d9f8"}, Samples: []metrics.Sample{{Timestamp: end, Value: float64(rolloutAt.Unix())}}},
				{Labels: map[string]string{"replicaset": "web.api-old11"}, Samples: []metrics.Sample{{Timestamp: end, Value: float64(start.Add(-48 * time.Hour).Unix())}}},
			}}, nil
		case strings.Contains(query.Expression, "OOMKilled"):
			return seriesAt(start, 10*time.Minute, 0, 0, 1, 1, 1, 1), nil
		case strings.Contains(query.Expression, "restarts_total"):
			return seriesAt(start, 10*time.Minute, 1, 0, 2, 0, 0, 0), nil
		case strings.Contains(query.Expression, "cfs_throttled"):
			return metrics.RangeResult{}, metrics.ErrUpstream
		case strings.Contains(query.Expression, "count(sum by (pod)"):
			return metrics.RangeResult{NoData: true}, nil
		case strings.Contains(query.Expression, "http_latency"):
			if !strings.Contains(query.Expression, `service="web.api"`) || !strings.Contains(query.Expression, `namespace="shop"`) {
				t.Errorf("latency template not rendered: %s", query.Expression)
			}
			return seriesAt(start, 10*time.Minute, 0.1, 0.2), nil
		default:
			return seriesAt(start, 10*time.Minute, 100, 110, 120), nil
		}
	}}
	signals, err := NewSource(fake).Signals(context.Background(), subject, Window{Duration: time.Hour, Step: 10 * time.Minute, End: end}, LatencyTemplate(`http_latency{namespace="$namespace",service="$workload"}`))
	if err != nil {
		t.Fatal(err)
	}
	if signals.Throttling != nil || signals.Replicas != nil || signals.Latency == nil || len(signals.CPU.Points) != 3 {
		t.Fatalf("series: throttling=%v replicas=%v latency=%v", signals.Throttling, signals.Replicas, signals.Latency)
	}
	if len(signals.Warnings) != 1 || !strings.Contains(signals.Warnings[0], "throttling") {
		t.Fatalf("warnings=%v", signals.Warnings)
	}
	kinds := []string{}
	for _, event := range signals.Events {
		kinds = append(kinds, event.Kind)
	}
	if strings.Join(kinds, ",") != "restart,oom_kill,rollout" || signals.Events[1].Count != 2 || !signals.Events[2].Timestamp.Equal(rolloutAt) {
		t.Fatalf("events=%#v", signals.Events)
	}
}

func TestSignalsRequireUsage(t *testing.T) {
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) { return metrics.RangeResult{NoData: true}, nil }}
	if _, err := NewSource(fake).Signals(context.Background(), subject, Window{Duration: time.Hour, Step: time.Minute}, ""); !errors.Is(err, ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
	failing := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) {
		return metrics.RangeResult{}, metrics.ErrUpstream
	}}
	if _, err := NewSource(failing).Signals(context.Background(), subject, Window{Duration: time.Hour, Step: time.Minute}, ""); !errors.Is(err, metrics.ErrUpstream) {
		t.Fatalf("got %v, want ErrUpstream", err)
	}
}

func TestLatencyTemplateValidation(t *testing.T) {
	cases := map[string]bool{
		"": true,
		`histogram_quantile(0.95, sum by (le) (rate(x{namespace="$namespace"}[5m])))`: true,
		"up":                                    false,
		"x{namespace=\"$namespace\"}\nor up":    false,
		strings.Repeat("x", 2040) + "$workload": false,
	}
	for template, valid := range cases {
		if err := LatencyTemplate(template).Validate(); (err == nil) != valid {
			t.Errorf("Validate(%.40q) = %v, want valid=%v", template, err, valid)
		}
	}
}

func TestPearsonAlignsTimestamps(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	left := Series{}
	right := Series{}
	inverse := Series{}
	for index := range 20 {
		at := start.Add(time.Duration(index) * time.Minute)
		left.Points = append(left.Points, Point{Timestamp: at, Value: float64(index)})
		right.Points = append(right.Points, Point{Timestamp: at, Value: float64(index * 2)})
		inverse.Points = append(inverse.Points, Point{Timestamp: at, Value: float64(-index)})
	}
	if value := Pearson(&left, &right); value == nil || *value != 1 {
		t.Fatalf("r=%v, want 1", value)
	}
	if value := Pearson(&left, &inverse); value == nil || *value != -1 {
		t.Fatalf("r=%v, want -1", value)
	}
	short := Series{Points: left.Points[:5]}
	if Pearson(&short, &right) != nil || Pearson(nil, &right) != nil {
		t.Fatal("too few shared points must be undefined")
	}
	constant := Series{}
	for _, point := range left.Points {
		constant.Points = append(constant.Points, Point{Timestamp: point.Timestamp, Value: 5})
	}
	if Pearson(&left, &constant) != nil {
		t.Fatal("a constant series has no correlation")
	}
}

func TestPodMatcherFollowsControllerNaming(t *testing.T) {
	deployment, _ := PodMatcher("Deployment", "api")
	if !deployment.MatchString("api-7d9f8c6b5-x2k4p") || deployment.MatchString("api-v2-7d9f8c6b5-x2k4p") || deployment.MatchString("api-0") {
		t.Fatal("deployment matcher is wrong")
	}
	statefulSet, _ := PodMatcher("StatefulSet", "db")
	if !statefulSet.MatchString("db-0") || statefulSet.MatchString("db-replica-0") {
		t.Fatal("statefulset matcher is wrong")
	}
	if _, err := PodMatcher("Pod", "x"); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("got %v", err)
	}
	if _, err := NewSource(&fakeQuerier{}).PodUsage(context.Background(), `bad"ns`, time.Hour, time.Time{}); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("got %v", err)
	}
}

func TestSeriesWindowHelpers(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	series := Series{Points: []Point{{start, 1}, {start.Add(time.Hour), 3}, {start.Add(2 * time.Hour), 5}}}
	if trimmed := series.Since(start.Add(time.Hour)); len(trimmed.Points) != 2 || trimmed.Points[0].Value != 3 {
		t.Fatalf("Since=%v", trimmed.Points)
	}
	if mean, count := series.MeanBetween(start, start.Add(2*time.Hour)); mean != 2 || count != 2 {
		t.Fatalf("mean=%v count=%d", mean, count)
	}
}

func TestHotspotExpressionsAreBoundedAndScoped(t *testing.T) {
	expressions := HotspotExpressions([]string{"shop"}, 24*time.Hour)
	for index, expression := range expressions {
		if !strings.Contains(expression, `namespace=~"shop"`) || !strings.Contains(expression, "topk(200,") {
			t.Fatalf("expression %d is not scoped and bounded: %s", index, expression)
		}
	}
	if !strings.Contains(expressions[1], "quantile_over_time(0.95,") || !strings.Contains(expressions[2], "clamp_min(") || !strings.Contains(expressions[3], "and on (namespace, pod, container)") {
		t.Fatalf("unexpected expressions: %v", expressions)
	}
}

func TestHotspotsMergeMeasurementsPerContainer(t *testing.T) {
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	call := 0
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		call++
		labels := map[string]string{"namespace": "shop", "pod": "api-7d9f8c6b5-x2k4p", "container": "app"}
		other := map[string]string{"namespace": "other", "pod": "x-1", "container": "c"}
		values := map[int]float64{1: 0.12, 2: 0.4, 3: 4.2, 4: 210, 5: 880}
		return metrics.RangeResult{Series: []metrics.Series{
			{Labels: labels, Samples: []metrics.Sample{{Timestamp: end, Value: values[call]}}},
			{Labels: other, Samples: []metrics.Sample{{Timestamp: end, Value: 1}}},
		}}, nil
	}}
	spots, err := NewSource(fake).Hotspots(context.Background(), []string{"shop"}, 24*time.Hour, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(spots) != 1 {
		t.Fatalf("spots=%#v, want only the requested namespace", spots)
	}
	spot := spots[0]
	if *spot.ThrottlingRatio != 0.12 || *spot.ThrottlingPeakRatio != 0.4 || *spot.SpikeRatio != 4.2 || *spot.CPUMedianMilli != 210 || *spot.CPUPeakMilli != 880 {
		t.Fatalf("spot=%#v", spot)
	}
}

func TestThrottlingPeakIsReportedWithEvents(t *testing.T) {
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		switch {
		case strings.Contains(query.Expression, "quantile_over_time(0.95"):
			return series(0.31), nil
		case strings.Contains(query.Expression, "cfs_throttled"):
			return series(0.03), nil
		case strings.Contains(query.Expression, "OOMKilled"):
			return series(0), nil
		default:
			return series(1), nil
		}
	}}
	events, err := NewSource(fake).Events(context.Background(), subject, Window{Duration: 24 * time.Hour, Step: 5 * time.Minute})
	if err != nil || events.ThrottlingRatio != 0.03 || events.ThrottlingPeakRatio != 0.31 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}
