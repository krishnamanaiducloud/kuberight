package httpapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
)

// defaultScanName names the schedule KubeRight creates for itself.
const defaultScanName = "Default scan"

// EnsureDefaultScanSchedule creates a schedule that scans every namespace
// the inventory can see, so recommendations appear without anyone queueing
// a scan. It does nothing when the interval is zero or when any schedule
// already exists: once one exists, schedules are the operator's. With a
// shared store only the replica holding the lock creates it.
func EnsureDefaultScanSchedule(ctx context.Context, cfg config.Config, store repository.Store, logger *slog.Logger, now time.Time) error {
	if cfg.DefaultScanInterval <= 0 || cfg.OperationMode == config.ModeObserve {
		return nil
	}
	if locker, ok := store.(repository.Locker); ok {
		release, acquired, err := locker.TryLock(ctx, "default-scan-schedule")
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		defer release()
	}
	existing, err := store.ListFleetSchedules(ctx, repository.FleetListOptions{Page: 1, PageSize: 1})
	if err != nil {
		return err
	}
	if existing.Total > 0 {
		return nil
	}
	window := min(max(cfg.Normalized().RecommendationWindow, time.Hour), 30*24*time.Hour)
	mutation := repository.FleetScheduleMutation{
		Name: defaultScanName, Enabled: true, IntervalSeconds: int64(cfg.DefaultScanInterval / time.Second),
		WindowSeconds: int64(window / time.Second), NextRunAt: now.UTC(), Actor: "kuberight",
	}
	var created repository.FleetSchedule
	err = store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
		var createErr error
		if created, createErr = tx.CreateFleetSchedule(ctx, mutation); createErr != nil {
			return createErr
		}
		return tx.AppendAuditEvent(ctx, repository.AuditEvent{
			Timestamp: now.UTC(), Actor: "kuberight", Action: "create", Resource: "fleet-schedules/" + created.ID,
			Outcome: "success", RequestID: "startup",
		})
	})
	if err != nil {
		return err
	}
	logger.Info("created the default scan schedule; recommendations follow its first scan",
		"schedule_id", created.ID, "interval", cfg.DefaultScanInterval.String(), "window", window.String())
	return nil
}
