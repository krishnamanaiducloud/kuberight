package usage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

type fakeQuerier struct {
	mu          sync.Mutex
	expressions []string
	respond     func(metrics.RangeQuery) (metrics.RangeResult, error)
}

func (fake *fakeQuerier) QueryRange(_ context.Context, query metrics.RangeQuery) (metrics.RangeResult, error) {
	fake.mu.Lock()
	fake.expressions = append(fake.expressions, query.Expression)
	fake.mu.Unlock()
	return fake.respond(query)
}

func series(values ...float64) metrics.RangeResult {
	samples := make([]metrics.Sample, len(values))
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for index, value := range values {
		samples[index] = metrics.Sample{Timestamp: start.Add(time.Duration(index) * time.Minute), Value: value}
	}
	return metrics.RangeResult{Series: []metrics.Series{{Samples: samples, Quality: metrics.DataQuality{Issues: []metrics.QualityIssue{}, Coverage: 1}}}}
}

var subject = Subject{Namespace: "shop", Kind: "Deployment", Workload: "web.api", Container: "app"}

func TestSubjectValidationRejectsInjection(t *testing.T) {
	cases := map[string]Subject{
		"namespace quote":   {Namespace: `shop"}`, Kind: "Deployment", Workload: "api", Container: "app"},
		"workload regex":    {Namespace: "shop", Kind: "Deployment", Workload: "api.*", Container: "app"},
		"container label":   {Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "a,b"},
		"uppercase":         {Namespace: "Shop", Kind: "Deployment", Workload: "api", Container: "app"},
		"unsupported kind":  {Namespace: "shop", Kind: "Pod", Workload: "api", Container: "app"},
		"long namespace":    {Namespace: strings.Repeat("a", 64), Kind: "Deployment", Workload: "api", Container: "app"},
		"empty container":   {Namespace: "shop", Kind: "Deployment", Workload: "api"},
		"newline workload":  {Namespace: "shop", Kind: "Deployment", Workload: "api\n", Container: "app"},
		"trailing hyphen":   {Namespace: "shop-", Kind: "Deployment", Workload: "api", Container: "app"},
		"empty dot segment": {Namespace: "shop", Kind: "Deployment", Workload: "api..x", Container: "app"},
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := candidate.Validate(); !errors.Is(err, ErrInvalidSubject) {
				t.Fatalf("got %v, want ErrInvalidSubject", err)
			}
		})
	}
}

func TestExpressionsEscapeAndAnchorPodNames(t *testing.T) {
	cpu := CPUExpression(subject, 5*time.Minute)
	want := `max(rate(container_cpu_usage_seconds_total{namespace="shop",pod=~"web\\.api-[a-z0-9]{1,10}-[a-z0-9]{5}",container="app"}[300s])) * 1000`
	if cpu != want {
		t.Fatalf("CPU expression:\n got %s\nwant %s", cpu, want)
	}
	memory := MemoryExpression(Subject{Namespace: "db", Kind: "StatefulSet", Workload: "pg", Container: "postgres"}, time.Hour)
	if !strings.Contains(memory, `pod=~"pg-[0-9]+"`) || !strings.Contains(memory, "[3600s]") {
		t.Fatalf("unexpected memory expression %s", memory)
	}
	if short := CPUExpression(subject, 15*time.Second); !strings.Contains(short, "[120s]") {
		t.Fatalf("short steps must use a two-minute rate window: %s", short)
	}
}

func TestHistoryReturnsBothSeries(t *testing.T) {
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		if strings.Contains(query.Expression, "cpu") {
			return series(100, 120, 140), nil
		}
		return series(1e8, 1.1e8), nil
	}}
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	history, err := NewSource(fake).History(context.Background(), subject, Window{Duration: time.Hour, Step: time.Minute, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.CPU.Points) != 3 || history.CPU.Unit != "millicores" || len(history.Memory.Points) != 2 || history.Memory.Unit != "bytes" {
		t.Fatalf("unexpected history %#v", history)
	}
	if !history.Start.Equal(end.Add(-time.Hour)) || history.CPU.Values()[2] != 140 {
		t.Fatalf("unexpected window or values: %#v", history)
	}
}

func TestHistoryReportsNoDataWithoutZeroFilling(t *testing.T) {
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) {
		return metrics.RangeResult{NoData: true}, nil
	}}
	history, err := NewSource(fake).History(context.Background(), subject, Window{Duration: time.Hour, Step: time.Minute})
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
	if len(history.CPU.Points) != 0 || !history.CPU.Quality.Has(metrics.IssueMissing) {
		t.Fatalf("missing data must stay missing: %#v", history.CPU)
	}
	blocking, _ := history.CPU.QualityIssues()
	if len(blocking) != 1 {
		t.Fatalf("missing data must be blocking: %v", blocking)
	}
}

func TestHistoryPropagatesUpstreamErrors(t *testing.T) {
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) {
		return metrics.RangeResult{}, metrics.ErrUpstream
	}}
	if _, err := NewSource(fake).History(context.Background(), subject, Window{Duration: time.Hour, Step: time.Minute}); !errors.Is(err, metrics.ErrUpstream) {
		t.Fatalf("got %v, want ErrUpstream", err)
	}
}

func TestHistoryRejectsInvalidSubjectBeforeQuerying(t *testing.T) {
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) { return series(1), nil }}
	bad := subject
	bad.Container = `app"} or vector(1) #`
	if _, err := NewSource(fake).History(context.Background(), bad, Window{Duration: time.Hour, Step: time.Minute}); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("got %v, want ErrInvalidSubject", err)
	}
	if len(fake.expressions) != 0 {
		t.Fatalf("invalid subject reached Prometheus: %v", fake.expressions)
	}
}

func TestEventsDistinguishUnknownFromZero(t *testing.T) {
	missing := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) { return metrics.RangeResult{NoData: true}, nil }}
	events, err := NewSource(missing).Events(context.Background(), subject, Window{Duration: 24 * time.Hour})
	if err != nil || events.Available || events.ThrottlingAvailable {
		t.Fatalf("events=%#v err=%v, want unavailable", events, err)
	}
	present := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		if strings.Contains(query.Expression, "cfs_throttled") {
			return series(0.08), nil
		}
		if strings.Contains(query.Expression, "OOMKilled") {
			return series(1), nil
		}
		return series(2.6), nil
	}}
	events, err = NewSource(present).Events(context.Background(), subject, Window{Duration: 24 * time.Hour})
	if err != nil || !events.Available || events.Restarts != 3 || events.OOMKills != 1 || !events.ThrottlingAvailable || events.ThrottlingRatio != 0.08 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if !strings.Contains(present.expressions[0], "[86400s]") {
		t.Fatalf("event lookback must match the window: %s", present.expressions[0])
	}
}

func TestQualityIssuesSeparateBlockingProblems(t *testing.T) {
	series := Series{Resource: "cpu", Quality: metrics.DataQuality{
		Issues:   []metrics.QualityIssue{metrics.IssueSparse, metrics.IssueDuplicate, metrics.IssueStale, metrics.IssueAnomalous},
		Coverage: 0.5, DuplicateSamples: 2, AnomalousSamples: 1,
	}}
	blocking, informational := series.QualityIssues()
	if len(blocking) != 2 || len(informational) != 2 || !strings.Contains(blocking[0], "50%") {
		t.Fatalf("blocking=%v informational=%v", blocking, informational)
	}
}

func TestThrottlingExpressionUsesValidatedSelector(t *testing.T) {
	expression := ThrottlingExpression(subject, 24*time.Hour)
	if !strings.Contains(expression, `container_cpu_cfs_throttled_periods_total{namespace="shop"`) || !strings.Contains(expression, "/ sum(increase(container_cpu_cfs_periods_total") || !strings.Contains(expression, "[86400s]") {
		t.Fatalf("unexpected throttling expression %s", expression)
	}
}

func TestNamespaceExpressionsFilterAndBoundSubqueries(t *testing.T) {
	expressions := NamespaceExpressions([]string{"shop", "billing", `bad"}`}, 7*24*time.Hour)
	if !strings.Contains(expressions[0], `namespace=~"shop|billing"`) || strings.Contains(expressions[0], "bad") {
		t.Fatalf("namespace matcher not validated: %s", expressions[0])
	}
	if !strings.Contains(expressions[1], "quantile_over_time(0.95,") || !strings.Contains(expressions[1], "[604800s:1200s]") {
		t.Fatalf("unexpected subquery: %s", expressions[1])
	}
	many := make([]string, 0, 600)
	for index := range 600 {
		many = append(many, "namespace-number-"+strings.Repeat("x", 3)+string(rune('a'+index%26)))
	}
	if strings.Contains(NamespaceExpressions(many, time.Hour)[0], "namespace=~") {
		t.Fatal("oversized namespace matcher must be dropped")
	}
}

func TestNamespaceUsageOnlyReturnsRequestedNamespaces(t *testing.T) {
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		value := 100.0
		if strings.Contains(query.Expression, "memory") {
			value = 1e9
		}
		if strings.Contains(query.Expression, "quantile") {
			value *= 2
		}
		sample := []metrics.Sample{{Timestamp: query.End, Value: value}}
		return metrics.RangeResult{Series: []metrics.Series{
			{Labels: map[string]string{"namespace": "shop"}, Samples: sample},
			{Labels: map[string]string{"namespace": "other-tenant"}, Samples: sample},
		}}, nil
	}}
	usage, err := NewSource(fake).NamespaceUsage(context.Background(), []string{"shop"}, 24*time.Hour, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage["shop"].CPUAverageMilli != 100 || usage["shop"].CPUP95Milli != 200 || usage["shop"].MemoryP95Bytes != 2e9 {
		t.Fatalf("usage = %#v", usage)
	}
	if len(fake.expressions) != 4 {
		t.Fatalf("expected four queries, got %d", len(fake.expressions))
	}
}
