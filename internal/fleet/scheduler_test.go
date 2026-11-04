package fleet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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

func TestWorkerCompletesAndRecordsFailure(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		executor Executor
		status   string
	}{
		{"success", executorFunc(func(_ context.Context, _ repository.FleetScan, report func(repository.FleetScanProgress) error) error {
			return report(repository.FleetScanProgress{Expected: 2, Processed: 2, Succeeded: 2})
		}), repository.FleetScanCompleted},
		{"failure", executorFunc(func(_ context.Context, _ repository.FleetScan, report func(repository.FleetScanProgress) error) error {
			if err := report(repository.FleetScanProgress{Expected: 2, Processed: 1, Succeeded: 1}); err != nil {
				return err
			}
			return errors.New("prometheus unavailable")
		}), repository.FleetScanFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := repository.NewMemory(100)
			scan, _, err := store.CreateFleetScan(t.Context(), repository.FleetScanRequest{Scope: repository.FleetScope{}, WindowSeconds: 24 * 3600, ScheduledFor: base, CreatedBy: "alice", MaxAttempts: 3})
			if err != nil {
				t.Fatal(err)
			}
			worker := NewWorker(store, test.executor, "worker-1", quietLogger())
			worker.now = func() time.Time { return base }
			worker.claimAndExecute(t.Context())
			stored, err := store.GetFleetScan(t.Context(), scan.ID)
			if err != nil || stored.Status != test.status || stored.CompletedAt == nil {
				t.Fatalf("stored=%#v err=%v", stored, err)
			}
		})
	}
}
