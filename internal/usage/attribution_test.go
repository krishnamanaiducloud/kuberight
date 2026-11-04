package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

func attributedIdentity() WorkloadIdentity {
	created := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	return WorkloadIdentity{
		UID: "deployment-current", Generation: 7, CreatedAt: created, ExpectedReplicas: 2,
		Pods: []PodIdentity{
			{Name: "web-api-new-aaaaa", UID: "pod-uid-a", CreatedAt: created.Add(time.Hour), Phase: "Running", Containers: []string{"app"}},
			{Name: "web-api-new-bbbbb", UID: "pod-uid-b", CreatedAt: created.Add(2 * time.Hour), Phase: "Running", Containers: []string{"app"}},
		},
	}
}

func TestAttributedExpressionsBindPodNamesAndUIDs(t *testing.T) {
	identity := attributedIdentity()
	cpu, err := AttributedCPUExpression(subject, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`kube_pod_info{namespace="shop"`, `uid=~"pod-uid-a|pod-uid-b"`, `pod=~"web-api-new-aaaaa|web-api-new-bbbbb"`, `group_left (uid)`, `max by (pod, uid)`} {
		if !strings.Contains(cpu, required) {
			t.Fatalf("exact CPU query does not contain %q: %s", required, cpu)
		}
	}
	if strings.Contains(cpu, `web\\.api-[a-z0-9]`) {
		t.Fatalf("exact query fell back to workload-name inference: %s", cpu)
	}
	memory, err := AttributedMemoryExpression(subject, identity, time.Minute)
	if err != nil || !strings.Contains(memory, "container_memory_working_set_bytes") || !strings.Contains(memory, "kube_pod_info") {
		t.Fatalf("memory expression=%q err=%v", memory, err)
	}
}

func TestAttributedIdentityFailsClosed(t *testing.T) {
	valid := attributedIdentity()
	cases := map[string]WorkloadIdentity{
		"missing workload uid": func() WorkloadIdentity { value := valid; value.UID = ""; return value }(),
		"missing creation":     func() WorkloadIdentity { value := valid; value.CreatedAt = time.Time{}; return value }(),
		"scaled to zero":       func() WorkloadIdentity { value := valid; value.ExpectedReplicas = 0; return value }(),
		"no exact pods":        func() WorkloadIdentity { value := valid; value.Pods = nil; return value }(),
		"resolution issue": func() WorkloadIdentity {
			value := valid
			value.ResolutionIssues = []string{"owner UID unavailable"}
			return value
		}(),
		"pod predates workload": func() WorkloadIdentity {
			value := valid
			value.Pods = append([]PodIdentity(nil), valid.Pods...)
			value.Pods[0].CreatedAt = valid.CreatedAt.Add(-time.Second)
			return value
		}(),
		"duplicate uid": func() WorkloadIdentity {
			value := valid
			value.Pods = append([]PodIdentity(nil), valid.Pods...)
			value.Pods[1].UID = value.Pods[0].UID
			return value
		}(),
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := AttributedCPUExpression(subject, identity, time.Minute)
			if !errors.Is(err, ErrAttributionIncomplete) {
				t.Fatalf("got %v, want ErrAttributionIncomplete", err)
			}
		})
	}
}

func resultForReplicas(at []time.Time, values map[replicaKey][]float64) metrics.RangeResult {
	result := metrics.RangeResult{Series: []metrics.Series{}}
	for key, samples := range values {
		raw := metrics.Series{Labels: map[string]string{"pod": key.pod, "uid": key.uid}, Quality: metrics.DataQuality{}}
		for index, value := range samples {
			raw.Samples = append(raw.Samples, metrics.Sample{Timestamp: at[index], Value: value})
		}
		result.Series = append(result.Series, raw)
	}
	return result
}

func TestAttributedHistoryRetainsReplicasAndAggregatesWorstReplica(t *testing.T) {
	identity := attributedIdentity()
	end := time.Date(2026, 10, 1, 0, 2, 0, 0, time.UTC)
	timestamps := []time.Time{end.Add(-2 * time.Minute), end.Add(-time.Minute), end}
	values := map[replicaKey][]float64{
		{pod: identity.Pods[0].Name, uid: identity.Pods[0].UID}: {100, 300, 100},
		{pod: identity.Pods[1].Name, uid: identity.Pods[1].UID}: {200, 100, 400},
		// A same-name series with an old UID must be discarded even if an
		// upstream unexpectedly returns it.
		{pod: identity.Pods[0].Name, uid: "old-recreated-uid"}: {9999, 9999, 9999},
	}
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) {
		return resultForReplicas(timestamps, values), nil
	}}
	history, err := NewSource(fake).HistoryForIdentity(context.Background(), subject, identity, Window{Duration: 2 * time.Minute, Step: time.Minute, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if got := history.CPU.Values(); len(got) != 3 || got[0] != 200 || got[1] != 300 || got[2] != 400 {
		t.Fatalf("busiest-replica CPU values = %v", got)
	}
	if history.ReplicaCoverage == nil || !history.ReplicaCoverage.Complete || history.ReplicaCoverage.ObservedReplicas != 2 || len(history.ReplicaCoverage.Observations) != 2 {
		t.Fatalf("coverage = %#v", history.ReplicaCoverage)
	}
	if history.ReplicaCoverage.CPUImbalanceRatio != 1.33 {
		t.Fatalf("CPU imbalance = %v, want 1.33", history.ReplicaCoverage.CPUImbalanceRatio)
	}
}

func TestAttributedHistoryReportsMissingAndShortLivedReplicas(t *testing.T) {
	identity := attributedIdentity()
	end := time.Date(2026, 10, 1, 0, 9, 0, 0, time.UTC)
	timestamps := []time.Time{end.Add(-time.Minute), end}
	values := map[replicaKey][]float64{{pod: identity.Pods[0].Name, uid: identity.Pods[0].UID}: {100, 100}}
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) {
		return resultForReplicas(timestamps, values), nil
	}}
	history, err := NewSource(fake).HistoryForIdentity(context.Background(), subject, identity, Window{Duration: 9 * time.Minute, Step: time.Minute, End: end})
	if err != nil {
		t.Fatal(err)
	}
	coverage := history.ReplicaCoverage
	if coverage.Complete || coverage.MissingReplicas != 1 || coverage.CPUReplicaCoverage != 0.5 || coverage.CPUTemporalCoverage != 0.2 {
		t.Fatalf("unexpected incomplete coverage: %#v", coverage)
	}
	joined := strings.Join(coverage.Issues, "; ")
	for _, required := range []string{"CPU metrics cover 1 of 2", "memory metrics cover 1 of 2", "exact CPU history covers 20%", "incomplete per-replica history"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("coverage issues do not contain %q: %s", required, joined)
		}
	}
}

func TestAttributedEventsRemainPerReplica(t *testing.T) {
	identity := attributedIdentity()
	end := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeQuerier{respond: func(query metrics.RangeQuery) (metrics.RangeResult, error) {
		valueA, valueB := 0.1, 0.2
		switch {
		case strings.Contains(query.Expression, "restarts_total"):
			valueA, valueB = 1.4, 2.6
		case strings.Contains(query.Expression, "OOMKilled"):
			valueA, valueB = 0, 1
		case strings.Contains(query.Expression, "quantile_over_time"):
			valueA, valueB = 0.3, 0.6
		}
		return resultForReplicas([]time.Time{query.End}, map[replicaKey][]float64{
			{pod: identity.Pods[0].Name, uid: identity.Pods[0].UID}: {valueA},
			{pod: identity.Pods[1].Name, uid: identity.Pods[1].UID}: {valueB},
		}), nil
	}}
	events, err := NewSource(fake).EventsForIdentity(context.Background(), subject, identity, Window{Duration: time.Hour, Step: time.Minute, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if !events.Available || events.Restarts != 4 || events.OOMKills != 1 || !events.ThrottlingAvailable || events.ThrottlingRatio != 0.2 || events.ThrottlingPeakRatio != 0.6 || len(events.Replicas) != 2 {
		t.Fatalf("events = %#v", events)
	}
}

func TestAttributedFingerprintChangesWithPodUID(t *testing.T) {
	identity := attributedIdentity()
	window := Window{Duration: time.Hour, Step: time.Minute, End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	first, err := AttributedRecommendationQueryFingerprint(subject, identity, window)
	if err != nil {
		t.Fatal(err)
	}
	identity.Pods = append([]PodIdentity(nil), identity.Pods...)
	identity.Pods[0].UID = "replacement-pod-uid"
	second, err := AttributedRecommendationQueryFingerprint(subject, identity, window)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) != 64 || len(second) != 64 {
		t.Fatalf("fingerprints must be stable SHA-256 values and UID-sensitive: %q %q", first, second)
	}
}

func TestForContainerFailsClosedWhenActivePodLacksContainer(t *testing.T) {
	identity := attributedIdentity()
	identity.Pods[1].Containers = []string{"sidecar"}
	filtered := identity.ForContainer("app")
	if len(filtered.Pods) != 1 || len(filtered.ResolutionIssues) != 1 || !strings.Contains(filtered.ResolutionIssues[0], "1 active") {
		t.Fatalf("filtered identity = %#v", filtered)
	}
}

func TestAttributedQueriesChunkLargeReplicaSetsBelowPrometheusLimit(t *testing.T) {
	created := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	identity := WorkloadIdentity{UID: "large-deployment", CreatedAt: created, ExpectedReplicas: 256, Pods: make([]PodIdentity, 0, 256)}
	for index := range 256 {
		identity.Pods = append(identity.Pods, PodIdentity{
			Name: fmt.Sprintf("large-deployment-very-long-revision-name-%03d-aaaaa", index), UID: fmt.Sprintf("00000000-0000-0000-0000-%012d", index),
			CreatedAt: created.Add(time.Minute), Phase: "Running", Containers: []string{"app"},
		})
	}
	fake := &fakeQuerier{respond: func(metrics.RangeQuery) (metrics.RangeResult, error) { return metrics.RangeResult{NoData: true}, nil }}
	source := NewSource(fake)
	window := Window{Duration: time.Hour, Step: time.Minute, End: created.Add(24 * time.Hour)}
	if _, err := source.HistoryForIdentity(context.Background(), subject, identity, window); !errors.Is(err, ErrNoData) {
		t.Fatalf("large exact history error = %v, want ErrNoData", err)
	}
	if _, err := source.EventsForIdentity(context.Background(), subject, identity, window); err != nil {
		t.Fatal(err)
	}
	if fingerprint, err := AttributedRecommendationQueryFingerprint(subject, identity, window); err != nil || len(fingerprint) != 64 {
		t.Fatalf("large fingerprint=%q err=%v", fingerprint, err)
	}
	if len(fake.expressions) <= 6 {
		t.Fatalf("large workload was not split into bounded queries: %d calls", len(fake.expressions))
	}
	for _, expression := range fake.expressions {
		if len(expression) > 8<<10 {
			t.Fatalf("chunked expression has %d bytes, over Prometheus bound: %s", len(expression), expression)
		}
	}
}
