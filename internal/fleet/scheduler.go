// Package fleet runs the durable fleet-scan queue. PostgreSQL row locking and
// unique schedule slots make enqueue and claim safe across API replicas.
package fleet

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kuberight/kuberight/internal/repository"
)

type Queue interface {
	EnqueueDueFleetScans(context.Context, time.Time, int) ([]repository.FleetScan, error)
	ClaimFleetScan(context.Context, string, time.Time, time.Duration) (repository.FleetScan, error)
	UpdateFleetScan(context.Context, string, string, time.Time, repository.FleetScanProgress) (repository.FleetScan, error)
}

type Scheduler struct {
	queue    Queue
	logger   *slog.Logger
	interval time.Duration
	batch    int
	now      func() time.Time
}

func NewScheduler(queue Queue, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{queue: queue, logger: logger, interval: 30 * time.Second, batch: 100, now: time.Now}
}

// Run enqueues immediately and then on a bounded cadence. A transient store
// failure is logged and retried; it never terminates the serving process.
func (scheduler *Scheduler) Run(ctx context.Context) {
	if scheduler == nil || scheduler.queue == nil {
		return
	}
	ticker := time.NewTicker(scheduler.interval)
	defer ticker.Stop()
	for {
		scheduler.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (scheduler *Scheduler) tick(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	scans, err := scheduler.queue.EnqueueDueFleetScans(ctx, scheduler.now().UTC(), scheduler.batch)
	if err != nil {
		if ctx.Err() == nil {
			scheduler.logger.Error("could not enqueue due fleet scans", "error", err)
		}
		return
	}
	if len(scans) > 0 {
		scheduler.logger.Info("enqueued scheduled fleet scans", "count", len(scans))
	}
}

// Executor analyzes one claimed scan. It should call progress after discovery
// and periodically while processing so the lease stays live and operators see
// accurate progress. Returning an error records a failed attempt.
type Executor interface {
	Execute(context.Context, repository.FleetScan, func(repository.FleetScanProgress) error) error
}

type Worker struct {
	queue       Queue
	executor    Executor
	logger      *slog.Logger
	owner       string
	lease       time.Duration
	poll        time.Duration
	concurrency int
	now         func() time.Time
}

func NewWorker(queue Queue, executor Executor, owner string, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{queue: queue, executor: executor, owner: owner, logger: logger, lease: repository.FleetScanLeaseDuration, poll: 5 * time.Second, concurrency: 1, now: time.Now}
}

func (worker *Worker) Run(ctx context.Context) {
	if worker == nil || worker.queue == nil || worker.executor == nil || worker.owner == "" {
		return
	}
	var workers sync.WaitGroup
	for index := 0; index < worker.concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			worker.runOne(ctx)
		}()
	}
	<-ctx.Done()
	workers.Wait()
}

func (worker *Worker) runOne(ctx context.Context) {
	ticker := time.NewTicker(worker.poll)
	defer ticker.Stop()
	for {
		worker.claimAndExecute(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *Worker) claimAndExecute(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	scan, err := worker.queue.ClaimFleetScan(ctx, worker.owner, worker.now().UTC(), worker.lease)
	if errors.Is(err, repository.ErrNotFound) {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			worker.logger.Error("could not claim fleet scan", "error", err)
		}
		return
	}
	executionContext, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()
	heartbeatContext, stopHeartbeat := context.WithCancel(executionContext)

	// Progress is per attempt. Carrying a failed attempt's partial counters into
	// a retry can make an otherwise successful retry impossible to finalize and
	// can retain stale per-target failure details.
	progress := repository.FleetScanProgress{}
	var progressMu, updateMu, leaseMu sync.Mutex
	var leaseErr error
	var loseLeaseOnce sync.Once
	loseLease := func(err error) {
		if err == nil {
			return
		}
		loseLeaseOnce.Do(func() {
			leaseMu.Lock()
			leaseErr = err
			leaseMu.Unlock()
			cancelExecution()
		})
	}
	persistProgress := func() error {
		updateMu.Lock()
		defer updateMu.Unlock()
		progressMu.Lock()
		snapshot := cloneProgress(progress)
		progressMu.Unlock()
		updateContext, cancel := context.WithTimeout(executionContext, 10*time.Second)
		defer cancel()
		_, updateErr := worker.queue.UpdateFleetScan(updateContext, scan.ID, worker.owner, worker.now().UTC(), snapshot)
		if updateErr != nil {
			loseLease(updateErr)
		}
		return updateErr
	}

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := worker.lease / 3
		if interval > 15*time.Second {
			interval = 15 * time.Second
		}
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatContext.Done():
				return
			case <-ticker.C:
				if err := persistProgress(); err != nil {
					return
				}
			}
		}
	}()

	report := func(next repository.FleetScanProgress) error {
		// The worker owns lifecycle transitions. Executors may only report
		// counters and bounded target diagnostics while the lease is running.
		if next.Status != "" || next.Error != "" {
			return repository.ErrFleetScanConflict
		}
		if err := repository.ValidateFleetScanProgress(next); err != nil {
			return err
		}
		progressMu.Lock()
		progress = cloneProgress(next)
		progressMu.Unlock()
		return persistProgress()
	}
	executeErr := worker.executor.Execute(executionContext, scan, report)
	stopHeartbeat()
	<-heartbeatDone

	leaseMu.Lock()
	lostLease := leaseErr
	leaseMu.Unlock()
	if lostLease != nil {
		if ctx.Err() == nil && !errors.Is(lostLease, repository.ErrFleetScanConflict) {
			worker.logger.Warn("fleet scan lease was lost", "scan_id", scan.ID, "error", lostLease)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}

	progressMu.Lock()
	finalProgress := cloneProgress(progress)
	progressMu.Unlock()
	if executeErr != nil {
		finalProgress.Status = repository.FleetScanFailed
		finalProgress.Error = truncateError(executeErr.Error(), 4096)
	} else {
		finalProgress.Status = repository.FleetScanCompleted
		finalProgress.Error = ""
	}
	finalContext, cancelFinal := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelFinal()
	updated, updateErr := worker.queue.UpdateFleetScan(finalContext, scan.ID, worker.owner, worker.now().UTC(), finalProgress)
	if updateErr != nil {
		worker.logger.Error("could not finalize fleet scan", "scan_id", scan.ID, "error", updateErr)
		return
	}
	if executeErr != nil {
		if updated.Status == repository.FleetScanQueued {
			worker.logger.Warn("fleet scan attempt failed and was queued for retry", "scan_id", scan.ID, "attempt", updated.Attempt, "max_attempts", updated.MaxAttempts, "error", finalProgress.Error)
		} else {
			worker.logger.Error("fleet scan exhausted its retry budget", "scan_id", scan.ID, "attempt", updated.Attempt, "error", finalProgress.Error)
		}
	}
}

func cloneProgress(progress repository.FleetScanProgress) repository.FleetScanProgress {
	progress.FailureDetails = append([]repository.FleetTargetFailure(nil), progress.FailureDetails...)
	return progress
}

func truncateError(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
