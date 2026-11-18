package fleet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kuberight/kuberight/internal/repository"
)

type executorFunc func(context.Context, repository.FleetScan, func(repository.FleetScanProgress) error) error

func (function executorFunc) Execute(ctx context.Context, scan repository.FleetScan, report func(repository.FleetScanProgress) error) error {
	return function(ctx, scan, report)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSchedulerEnqueuesDueSchedule(t *testing.T) {
	store := repository.NewMemory(100)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	_, err := store.CreateFleetSchedule(t.Context(), repository.FleetScheduleMutation{Name: "hourly", Enabled: true, IntervalSeconds: 3600, WindowSeconds: 24 * 3600, Scope: repository.FleetScope{}, NextRunAt: base, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewScheduler(store, quietLogger())
	scheduler.now = func() time.Time { return base }
	scheduler.tick(t.Context())
	result, err := store.ListFleetScans(t.Context(), repository.FleetListOptions{Page: 1, PageSize: 10})
	if err != nil || result.Total != 1 || result.Items[0].Status != repository.FleetScanQueued {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	scheduler.tick(t.Context())
	result, _ = store.ListFleetScans(t.Context(), repository.FleetListOptions{Page: 1, PageSize: 10})
	if result.Total != 1 {
		t.Fatalf("duplicate schedule slot was enqueued: %#v", result)
	}
}

func TestWorkerCompletesSuccessfulScan(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory(100)
	scan, _, err := store.CreateFleetScan(t.Context(), repository.FleetScanRequest{Scope: repository.FleetScope{}, WindowSeconds: 24 * 3600, ScheduledFor: base, CreatedBy: "alice", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, executorFunc(func(_ context.Context, _ repository.FleetScan, report func(repository.FleetScanProgress) error) error {
		return report(repository.FleetScanProgress{Expected: 2, Processed: 2, Succeeded: 2})
	}), "worker-1", quietLogger())
	worker.now = func() time.Time { return base }
	worker.claimAndExecute(t.Context())
	stored, err := store.GetFleetScan(t.Context(), scan.ID)
	if err != nil || stored.Status != repository.FleetScanCompleted || stored.CompletedAt == nil {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestWorkerRetriesFailureWithFreshAttemptProgress(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory(100)
	scan, _, err := store.CreateFleetScan(t.Context(), repository.FleetScanRequest{Scope: repository.FleetScope{}, WindowSeconds: 24 * 3600, ScheduledFor: base, CreatedBy: "alice", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	executor := executorFunc(func(_ context.Context, claimed repository.FleetScan, report func(repository.FleetScanProgress) error) error {
		attempts++
		if claimed.ScheduledFor != scan.ScheduledFor || claimed.CreatedAt != scan.CreatedAt || claimed.WindowSeconds != scan.WindowSeconds {
			t.Fatalf("retry changed frozen scan inputs: claimed=%#v original=%#v", claimed, scan)
		}
		if attempts == 1 {
			if err := report(repository.FleetScanProgress{Expected: 2, Processed: 1, Succeeded: 1}); err != nil {
				return err
			}
			return errors.New("prometheus unavailable")
		}
		// A retry starts with fresh progress. A successful empty result must not
		// inherit the first attempt's partial counters or diagnostics.
		return nil
	})
	now := base
	worker := NewWorker(store, executor, "worker-1", quietLogger())
	worker.now = func() time.Time { return now }
	worker.claimAndExecute(t.Context())
	queued, err := store.GetFleetScan(t.Context(), scan.ID)
	if err != nil || queued.Status != repository.FleetScanQueued || queued.CompletedAt != nil || queued.Attempt != 1 || queued.LastError != "prometheus unavailable" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if want := base.Add(repository.FleetScanRetryDelay); !queued.AvailableAt.Equal(want) {
		t.Fatalf("availableAt=%s want=%s", queued.AvailableAt, want)
	}

	now = queued.AvailableAt
	worker.claimAndExecute(t.Context())
	completed, err := store.GetFleetScan(t.Context(), scan.ID)
	if err != nil || completed.Status != repository.FleetScanCompleted || completed.CompletedAt == nil || completed.Attempt != 2 {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
	if completed.Expected != 0 || completed.Processed != 0 || completed.Succeeded != 0 || completed.Failed != 0 || completed.LastError != "" || len(completed.FailureDetails) != 0 {
		t.Fatalf("retry retained stale progress: %#v", completed)
	}
}

func TestWorkerExhaustsRetryBudget(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory(100)
	scan, _, err := store.CreateFleetScan(t.Context(), repository.FleetScanRequest{Scope: repository.FleetScope{}, WindowSeconds: 24 * 3600, ScheduledFor: base, CreatedBy: "alice", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, executorFunc(func(context.Context, repository.FleetScan, func(repository.FleetScanProgress) error) error {
		return errors.New("metrics backend unavailable")
	}), "worker-1", quietLogger())
	now := base
	worker.now = func() time.Time { return now }
	worker.claimAndExecute(t.Context())
	now = base.Add(repository.FleetScanRetryDelay)
	worker.claimAndExecute(t.Context())
	failed, err := store.GetFleetScan(t.Context(), scan.ID)
	if err != nil || failed.Status != repository.FleetScanFailed || failed.CompletedAt == nil || failed.Attempt != failed.MaxAttempts {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
}

func TestWorkerRunWaitsForInFlightExecution(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store := repository.NewMemory(100)
	if _, _, err := store.CreateFleetScan(t.Context(), repository.FleetScanRequest{Scope: repository.FleetScope{}, WindowSeconds: 24 * 3600, ScheduledFor: base, CreatedBy: "alice", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	executor := executorFunc(func(ctx context.Context, _ repository.FleetScan, _ func(repository.FleetScanProgress) error) error {
		close(started)
		<-ctx.Done()
		<-release
		return ctx.Err()
	})
	worker := NewWorker(store, executor, "worker-1", quietLogger())
	worker.now = func() time.Time { return base }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start the queued scan")
	}
	cancel()
	select {
	case <-done:
		close(release)
		t.Fatal("Run returned before the in-flight executor stopped")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the executor stopped")
	}
}

func TestTruncateErrorPreservesUTF8(t *testing.T) {
	got := truncateError("1234€5678", 6)
	if got != "1234" || !utf8.ValidString(got) {
		t.Fatalf("truncateError()=%q", got)
	}
}
