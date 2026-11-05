package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

func fleetExecutorForTest(t *testing.T, spec WorkloadSpec, source *exactFakeUsage) (*FleetScanExecutor, *repository.Memory) {
	t.Helper()
	store := repository.NewMemory(100)
	executor, err := NewFleetScanExecutor(fallbackConfig(), Dependencies{
		Provider: fakeInventory{spec: spec}, Usage: source, Repository: store, Access: &recordingAccess{},
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor, store
}

func completeExactCoverage(replicas int32) *usage.ReplicaCoverage {
	return &usage.ReplicaCoverage{
		Attribution: usage.AttributionHistorical, ExpectedReplicas: replicas, DiscoveredReplicas: int(replicas), ObservedReplicas: int(replicas),
		ActiveReplicas: int(replicas), CPUObservedReplicas: int(replicas), MemoryObservedReplicas: int(replicas),
		CPUReplicaCoverage: 1, MemoryReplicaCoverage: 1, CPUTemporalCoverage: 1, MemoryTemporalCoverage: 1,
		ExpectedReplicaHistoryCoverage: 1, IdentityCoverage: 1, Complete: true, Issues: []string{},
	}
}

func onlyRecommendation(t *testing.T, store *repository.Memory) repository.RecommendationRecord {
	t.Helper()
	listed, err := store.ListRecommendations(context.Background(), repository.ListOptions{Page: 1, PageSize: 10})
	if err != nil || len(listed.Items) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	return listed.Items[0]
}

func TestFleetObservationWindowEndsAtExecutionAndRecordsDelay(t *testing.T) {
	spec := checkoutSpec()
	spec.Replicas = 2
	spec.MetricIdentity = recommendationIdentity(2)
	base := healthyUsage(2016)
	base.history.ReplicaCoverage = completeExactCoverage(2)
	source := &exactFakeUsage{fakeUsage: base}
	executor, store := fleetExecutorForTest(t, spec, source)
	scan := repository.FleetScan{ID: "scan-late", ScheduledFor: fixedNow.Add(-3 * time.Hour), CreatedAt: fixedNow.Add(-3 * time.Hour), WindowSeconds: 7 * 24 * 3600}
	if err := executor.analyze(context.Background(), scan, spec, spec.Containers[0]); err != nil {
		t.Fatal(err)
	}
	if len(source.windows) != 1 || !source.windows[0].End.Equal(fixedNow.Truncate(time.Second)) {
		t.Fatalf("observation windows = %+v, want the execution time", source.windows)
	}
	record := onlyRecommendation(t, store)
	if record.Provenance.ObservationEnd == nil || !record.Provenance.ObservationEnd.Equal(fixedNow.Truncate(time.Second)) {
		t.Fatalf("provenance = %+v", record.Provenance)
	}
	if !strings.Contains(strings.Join(record.Warnings, "; "), "ran 3h0m0s late") {
		t.Fatalf("the delay was not recorded: %v", record.Warnings)
	}
}

func TestFleetBlocksReductionsWithoutPerReplicaEventEvidence(t *testing.T) {
	spec := checkoutSpec()
	spec.Replicas = 2
	spec.MetricIdentity = recommendationIdentity(2)
	base := healthyUsage(2016)
	base.history.ReplicaCoverage = completeExactCoverage(2)
	// kube-state-metrics restart and OOM series are missing.
	base.events = usage.Events{Available: false}
	source := &exactFakeUsage{fakeUsage: base}
	executor, store := fleetExecutorForTest(t, spec, source)
	scan := repository.FleetScan{ID: "scan-events", ScheduledFor: fixedNow, CreatedAt: fixedNow, WindowSeconds: 7 * 24 * 3600}
	if err := executor.analyze(context.Background(), scan, spec, spec.Containers[0]); err != nil {
		t.Fatal(err)
	}
	record := onlyRecommendation(t, store)
	if record.DataQuality.Sufficient || !strings.Contains(strings.Join(record.DataQuality.Issues, "; "), "per-replica restart and OOM history is unavailable") {
		t.Fatalf("data quality = %+v", record.DataQuality)
	}
	if record.MemoryRequestBytes < record.CurrentMemoryRequestBytes || record.CPURequestMilli < record.CurrentCPURequestMilli {
		t.Fatalf("a fleet scan reduced requests without OOM evidence: cpu %d->%d memory %d->%d", record.CurrentCPURequestMilli, record.CPURequestMilli, record.CurrentMemoryRequestBytes, record.MemoryRequestBytes)
	}
}

func TestBoundedIssuesKeepsOptimizerInputValid(t *testing.T) {
	issues := make([]string, 0, 40)
	for index := range 40 {
		issues = append(issues, fmt.Sprintf("issue %02d ", index)+strings.Repeat("x", 300))
	}
	bounded := boundedIssues(append(issues, issues[0], " "))
	if len(bounded) != maximumQualityIssues || bounded[len(bounded)-1] != "additional data-quality issues were omitted" {
		t.Fatalf("bounded = %d items, last %q", len(bounded), bounded[len(bounded)-1])
	}
	for _, issue := range bounded {
		if len(issue) > maximumQualityIssueLength || issue == "" {
			t.Fatalf("issue length %d", len(issue))
		}
	}
}
