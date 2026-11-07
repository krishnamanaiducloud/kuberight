package repository

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

func cloneFleetScope(scope FleetScope) FleetScope {
	scope.Namespaces = append([]string(nil), scope.Namespaces...)
	scope.Kinds = append([]string(nil), scope.Kinds...)
	return scope
}

func cloneFleetSchedule(schedule FleetSchedule) FleetSchedule {
	schedule.Scope = cloneFleetScope(schedule.Scope)
	if schedule.LastRunAt != nil {
		value := *schedule.LastRunAt
		schedule.LastRunAt = &value
	}
	return schedule
}

func cloneFleetScan(scan FleetScan) FleetScan {
	scan.Scope = cloneFleetScope(scan.Scope)
	scan.FailureDetails = append([]FleetTargetFailure(nil), scan.FailureDetails...)
	if scan.LeaseExpiresAt != nil {
		value := *scan.LeaseExpiresAt
		scan.LeaseExpiresAt = &value
	}
	if scan.StartedAt != nil {
		value := *scan.StartedAt
		scan.StartedAt = &value
	}
	if scan.CompletedAt != nil {
		value := *scan.CompletedAt
		scan.CompletedAt = &value
	}
	if scan.CancellationAt != nil {
		value := *scan.CancellationAt
		scan.CancellationAt = &value
	}
	return scan
}

func NewPolicyID() (string, error)        { return newID("pol_") }
func NewFleetScheduleID() (string, error) { return newID("fsc_") }
func NewFleetScanID() (string, error)     { return newID("fsn_") }

func (memory *Memory) CreatePolicy(ctx context.Context, mutation PolicyMutation) (PolicyRecord, error) {
	if err := ctx.Err(); err != nil {
		return PolicyRecord{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.createPolicy(mutation)
}

func (memory *Memory) createPolicy(mutation PolicyMutation) (PolicyRecord, error) {
	if err := mutation.Validate(); err != nil {
		return PolicyRecord{}, err
	}
	id, err := NewPolicyID()
	if err != nil {
		return PolicyRecord{}, errors.New("could not generate policy identifier")
	}
	now := memory.now().UTC()
	record := PolicyRecord{ID: id, Name: strings.TrimSpace(mutation.Name), Description: mutation.Description, Scope: mutation.Scope, Priority: mutation.Priority, Enabled: mutation.Enabled, Policy: mutation.Policy, Version: 1, CreatedAt: now, CreatedBy: mutation.Actor, UpdatedAt: now, UpdatedBy: mutation.Actor}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.state.policies[id] = record
	memory.state.policyOrder = append(memory.state.policyOrder, id)
	return record, nil
}

func (memory *Memory) GetPolicy(ctx context.Context, id string) (PolicyRecord, error) {
	if err := ctx.Err(); err != nil {
		return PolicyRecord{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	record, ok := memory.state.policies[id]
	if !ok {
		return PolicyRecord{}, ErrNotFound
	}
	return record, nil
}

func (memory *Memory) ListPolicies(ctx context.Context, options PolicyListOptions) (PolicyListResult, error) {
	if err := ctx.Err(); err != nil {
		return PolicyListResult{}, err
	}
	if err := ValidatePolicyListOptions(options); err != nil {
		return PolicyListResult{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	items := make([]PolicyRecord, 0, len(memory.state.policies))
	for index := len(memory.state.policyOrder) - 1; index >= 0; index-- {
		record := memory.state.policies[memory.state.policyOrder[index]]
		if options.Cluster != "" && record.Scope.Cluster != "" && record.Scope.Cluster != options.Cluster {
			continue
		}
		if options.Namespace != "" && record.Scope.Namespace != "" && record.Scope.Namespace != options.Namespace {
			continue
		}
		if options.Level != "" && record.Scope.Level != options.Level {
			continue
		}
		if options.Enabled != nil && record.Enabled != *options.Enabled {
			continue
		}
		items = append(items, record)
	}
	total := len(items)
	start := min((options.Page-1)*options.PageSize, total)
	end := min(start+options.PageSize, total)
	return PolicyListResult{Items: append([]PolicyRecord(nil), items[start:end]...), Page: options.Page, PageSize: options.PageSize, Total: total}, nil
}

func (memory *Memory) UpdatePolicy(ctx context.Context, id string, expectedVersion int64, mutation PolicyMutation) (PolicyRecord, error) {
	if err := ctx.Err(); err != nil {
		return PolicyRecord{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.updatePolicy(id, expectedVersion, mutation)
}

func (memory *Memory) updatePolicy(id string, expectedVersion int64, mutation PolicyMutation) (PolicyRecord, error) {
	if err := mutation.Validate(); err != nil {
		return PolicyRecord{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.policies[id]
	if !ok {
		return PolicyRecord{}, ErrNotFound
	}
	if expectedVersion < 1 || record.Version != expectedVersion {
		return PolicyRecord{}, ErrPolicyConflict
	}
	record.Name, record.Description, record.Scope, record.Priority, record.Enabled, record.Policy = strings.TrimSpace(mutation.Name), mutation.Description, mutation.Scope, mutation.Priority, mutation.Enabled, mutation.Policy
	record.Version++
	record.UpdatedAt = memory.now().UTC()
	record.UpdatedBy = mutation.Actor
	memory.state.policies[id] = record
	return record, nil
}

func (memory *Memory) DeletePolicy(ctx context.Context, id string, expectedVersion int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.deletePolicy(id, expectedVersion)
}

func (memory *Memory) deletePolicy(id string, expectedVersion int64) error {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.policies[id]
	if !ok {
		return ErrNotFound
	}
	if expectedVersion < 1 || record.Version != expectedVersion {
		return ErrPolicyConflict
	}
	delete(memory.state.policies, id)
	for index, candidate := range memory.state.policyOrder {
		if candidate == id {
			memory.state.policyOrder = append(memory.state.policyOrder[:index], memory.state.policyOrder[index+1:]...)
			break
		}
	}
	return nil
}

func (memory *Memory) ResolvePolicy(ctx context.Context, subject PolicyScope) (PolicyRecord, error) {
	if err := ctx.Err(); err != nil {
		return PolicyRecord{}, err
	}
	if subject.Namespace == "" || subject.Kind == "" || subject.Workload == "" || subject.Container == "" {
		return PolicyRecord{}, ErrInvalidPolicy
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	var matches []PolicyRecord
	for _, record := range memory.state.policies {
		if record.Enabled && record.Scope.Matches(subject) {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return PolicyRecord{}, ErrNotFound
	}
	sort.SliceStable(matches, func(i, j int) bool {
		left, right := matches[i], matches[j]
		if left.Scope.Specificity() != right.Scope.Specificity() {
			return left.Scope.Specificity() > right.Scope.Specificity()
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
	return matches[0], nil
}

func validateFleetList(options FleetListOptions) error {
	if options.Page < 1 || options.PageSize < 1 || options.PageSize > 200 {
		return errors.New("page must be positive and pageSize must be between 1 and 200")
	}
	if options.Status != "" && options.Status != FleetScanQueued && options.Status != FleetScanRunning && options.Status != FleetScanCompleted && options.Status != FleetScanFailed && options.Status != FleetScanCanceled {
		return errors.New("unsupported fleet scan status")
	}
	return nil
}

func (memory *Memory) CreateFleetSchedule(ctx context.Context, mutation FleetScheduleMutation) (FleetSchedule, error) {
	if err := ctx.Err(); err != nil {
		return FleetSchedule{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.createFleetSchedule(mutation)
}
func (memory *Memory) createFleetSchedule(mutation FleetScheduleMutation) (FleetSchedule, error) {
	if err := mutation.Validate(); err != nil {
		return FleetSchedule{}, err
	}
	id, err := NewFleetScheduleID()
	if err != nil {
		return FleetSchedule{}, err
	}
	now := memory.now().UTC()
	record := FleetSchedule{ID: id, Name: strings.TrimSpace(mutation.Name), Enabled: mutation.Enabled, IntervalSeconds: mutation.IntervalSeconds, Scope: cloneFleetScope(mutation.Scope), PolicyID: mutation.PolicyID, WindowSeconds: mutation.WindowSeconds, NextRunAt: mutation.NextRunAt.UTC(), Version: 1, CreatedAt: now, CreatedBy: mutation.Actor, UpdatedAt: now, UpdatedBy: mutation.Actor}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.state.schedules[id] = record
	memory.state.scheduleOrder = append(memory.state.scheduleOrder, id)
	return cloneFleetSchedule(record), nil
}
func (memory *Memory) GetFleetSchedule(ctx context.Context, id string) (FleetSchedule, error) {
	if err := ctx.Err(); err != nil {
		return FleetSchedule{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	record, ok := memory.state.schedules[id]
	if !ok {
		return FleetSchedule{}, ErrNotFound
	}
	return cloneFleetSchedule(record), nil
}
func (memory *Memory) ListFleetSchedules(ctx context.Context, options FleetListOptions) (FleetScheduleListResult, error) {
	if err := ctx.Err(); err != nil {
		return FleetScheduleListResult{}, err
	}
	if err := validateFleetList(options); err != nil {
		return FleetScheduleListResult{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	items := make([]FleetSchedule, 0, len(memory.state.schedules))
	for i := len(memory.state.scheduleOrder) - 1; i >= 0; i-- {
		items = append(items, cloneFleetSchedule(memory.state.schedules[memory.state.scheduleOrder[i]]))
	}
	total := len(items)
	start := min((options.Page-1)*options.PageSize, total)
	end := min(start+options.PageSize, total)
	return FleetScheduleListResult{Items: items[start:end], Page: options.Page, PageSize: options.PageSize, Total: total}, nil
}
func (memory *Memory) UpdateFleetSchedule(ctx context.Context, id string, version int64, mutation FleetScheduleMutation) (FleetSchedule, error) {
	if err := ctx.Err(); err != nil {
		return FleetSchedule{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.updateFleetSchedule(id, version, mutation)
}
func (memory *Memory) updateFleetSchedule(id string, version int64, mutation FleetScheduleMutation) (FleetSchedule, error) {
	if err := mutation.Validate(); err != nil {
		return FleetSchedule{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.schedules[id]
	if !ok {
		return FleetSchedule{}, ErrNotFound
	}
	if version < 1 || record.Version != version {
		return FleetSchedule{}, ErrFleetConflict
	}
	record.Name, record.Enabled, record.IntervalSeconds, record.Scope, record.PolicyID, record.WindowSeconds, record.NextRunAt = strings.TrimSpace(mutation.Name), mutation.Enabled, mutation.IntervalSeconds, cloneFleetScope(mutation.Scope), mutation.PolicyID, mutation.WindowSeconds, mutation.NextRunAt.UTC()
	record.Version++
	record.UpdatedAt = memory.now().UTC()
	record.UpdatedBy = mutation.Actor
	memory.state.schedules[id] = record
	return cloneFleetSchedule(record), nil
}
func (memory *Memory) DeleteFleetSchedule(ctx context.Context, id string, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.deleteFleetSchedule(id, version)
}
func (memory *Memory) deleteFleetSchedule(id string, version int64) error {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.schedules[id]
	if !ok {
		return ErrNotFound
	}
	if version < 1 || record.Version != version {
		return ErrFleetConflict
	}
	delete(memory.state.schedules, id)
	for i, candidate := range memory.state.scheduleOrder {
		if candidate == id {
			memory.state.scheduleOrder = append(memory.state.scheduleOrder[:i], memory.state.scheduleOrder[i+1:]...)
			break
		}
	}
	return nil
}

func (memory *Memory) CreateFleetScan(ctx context.Context, request FleetScanRequest) (FleetScan, bool, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, false, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.createFleetScan(request, "")
}
func (memory *Memory) createFleetScan(request FleetScanRequest, scheduleID string) (FleetScan, bool, error) {
	if err := request.Validate(); err != nil {
		return FleetScan{}, false, err
	}
	hash := FleetIdempotencyHash(request.CreatedBy, request.IdempotencyKey)
	fingerprint := FleetRequestFingerprint(request)
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if hash != "" {
		if id, ok := memory.state.fleetIdempotency[hash]; ok {
			existing := memory.state.scans[id]
			if existing.RequestFingerprint != fingerprint {
				return FleetScan{}, false, ErrIdempotencyConflict
			}
			return cloneFleetScan(existing), false, nil
		}
	}
	id, err := NewFleetScanID()
	if err != nil {
		return FleetScan{}, false, err
	}
	now := memory.now().UTC()
	record := FleetScan{ID: id, ScheduleID: scheduleID, ScheduledFor: request.ScheduledFor.UTC(), AvailableAt: request.ScheduledFor.UTC(), Scope: cloneFleetScope(request.Scope), PolicyID: request.PolicyID, WindowSeconds: request.WindowSeconds, Status: FleetScanQueued, MaxAttempts: request.MaxAttempts, CreatedAt: now, CreatedBy: request.CreatedBy, IdempotencyHash: hash, RequestFingerprint: fingerprint}
	memory.state.scans[id] = record
	memory.state.scanOrder = append(memory.state.scanOrder, id)
	if hash != "" {
		memory.state.fleetIdempotency[hash] = id
	}
	return cloneFleetScan(record), true, nil
}
func (memory *Memory) GetFleetScan(ctx context.Context, id string) (FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	record, ok := memory.state.scans[id]
	if !ok {
		return FleetScan{}, ErrNotFound
	}
	return cloneFleetScan(record), nil
}
func (memory *Memory) ListFleetScans(ctx context.Context, options FleetListOptions) (FleetScanListResult, error) {
	if err := ctx.Err(); err != nil {
		return FleetScanListResult{}, err
	}
	if err := validateFleetList(options); err != nil {
		return FleetScanListResult{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	items := []FleetScan{}
	for i := len(memory.state.scanOrder) - 1; i >= 0; i-- {
		record := memory.state.scans[memory.state.scanOrder[i]]
		if options.Status != "" && record.Status != options.Status {
			continue
		}
		if options.ScheduleID != "" && record.ScheduleID != options.ScheduleID {
			continue
		}
		items = append(items, cloneFleetScan(record))
	}
	total := len(items)
	start := min((options.Page-1)*options.PageSize, total)
	end := min(start+options.PageSize, total)
	return FleetScanListResult{Items: items[start:end], Page: options.Page, PageSize: options.PageSize, Total: total}, nil
}
func (memory *Memory) CancelFleetScan(ctx context.Context, id string, now time.Time) (FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.cancelFleetScan(id, now)
}
func (memory *Memory) cancelFleetScan(id string, now time.Time) (FleetScan, error) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.scans[id]
	if !ok {
		return FleetScan{}, ErrNotFound
	}
	if record.Status != FleetScanQueued && record.Status != FleetScanRunning {
		return FleetScan{}, ErrFleetScanConflict
	}
	value := now.UTC()
	record.Status = FleetScanCanceled
	record.CancellationAt = &value
	record.CompletedAt = &value
	record.LeaseOwner = ""
	record.LeaseExpiresAt = nil
	memory.state.scans[id] = record
	return cloneFleetScan(record), nil
}
func (memory *Memory) ResumeFleetScan(ctx context.Context, id string, now time.Time) (FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	return memory.resumeFleetScan(id, now)
}
func (memory *Memory) resumeFleetScan(id string, now time.Time) (FleetScan, error) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.scans[id]
	if !ok {
		return FleetScan{}, ErrNotFound
	}
	if record.Status != FleetScanCanceled && record.Status != FleetScanFailed {
		return FleetScan{}, ErrFleetScanConflict
	}
	record.Status = FleetScanQueued
	record.AvailableAt = now.UTC()
	record.Attempt = 0
	record.Expected = 0
	record.Processed = 0
	record.Succeeded = 0
	record.Failed = 0
	record.CancellationAt = nil
	record.CompletedAt = nil
	record.StartedAt = nil
	record.LastError = ""
	record.FailureDetails = nil
	memory.state.scans[id] = record
	return cloneFleetScan(record), nil
}

func (memory *Memory) EnqueueDueFleetScans(ctx context.Context, now time.Time, limit int) ([]FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	ids := append([]string(nil), memory.state.scheduleOrder...)
	sort.Slice(ids, func(i, j int) bool {
		return memory.state.schedules[ids[i]].NextRunAt.Before(memory.state.schedules[ids[j]].NextRunAt)
	})
	result := []FleetScan{}
	for _, id := range ids {
		if len(result) >= limit {
			break
		}
		schedule := memory.state.schedules[id]
		if !schedule.Enabled || schedule.NextRunAt.After(now) {
			continue
		}
		scheduledFor := schedule.NextRunAt
		request := FleetScanRequest{Scope: schedule.Scope, PolicyID: schedule.PolicyID, WindowSeconds: schedule.WindowSeconds, ScheduledFor: scheduledFor, CreatedBy: "fleet-scheduler", MaxAttempts: 3}
		scanID, err := NewFleetScanID()
		if err != nil {
			return nil, err
		}
		created := memory.now().UTC()
		scan := FleetScan{ID: scanID, ScheduleID: id, ScheduledFor: scheduledFor, AvailableAt: scheduledFor, Scope: cloneFleetScope(schedule.Scope), PolicyID: schedule.PolicyID, WindowSeconds: schedule.WindowSeconds, Status: FleetScanQueued, MaxAttempts: request.MaxAttempts, CreatedAt: created, CreatedBy: request.CreatedBy, RequestFingerprint: FleetRequestFingerprint(request)}
		memory.state.scans[scanID] = scan
		memory.state.scanOrder = append(memory.state.scanOrder, scanID)
		last := scheduledFor
		schedule.LastRunAt = &last
		for !schedule.NextRunAt.After(now) {
			schedule.NextRunAt = schedule.NextRunAt.Add(time.Duration(schedule.IntervalSeconds) * time.Second)
		}
		schedule.Version++
		schedule.UpdatedAt = created
		schedule.UpdatedBy = "fleet-scheduler"
		memory.state.schedules[id] = schedule
		result = append(result, cloneFleetScan(scan))
	}
	return result, nil
}
func (memory *Memory) ClaimFleetScan(ctx context.Context, worker string, now time.Time, lease time.Duration) (FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, err
	}
	if strings.TrimSpace(worker) == "" || len(worker) > 128 || lease < time.Minute || lease > time.Hour {
		return FleetScan{}, ErrFleetScanConflict
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for _, id := range memory.state.scanOrder {
		record := memory.state.scans[id]
		eligible := record.Status == FleetScanQueued || (record.Status == FleetScanRunning && record.LeaseExpiresAt != nil && !record.LeaseExpiresAt.After(now))
		if !eligible || record.AvailableAt.After(now) || record.Attempt >= record.MaxAttempts {
			continue
		}
		expires := now.UTC().Add(lease)
		started := now.UTC()
		record.Status = FleetScanRunning
		record.LeaseOwner = worker
		record.LeaseExpiresAt = &expires
		record.Attempt++
		if record.StartedAt == nil {
			record.StartedAt = &started
		}
		memory.state.scans[id] = record
		return cloneFleetScan(record), nil
	}
	return FleetScan{}, ErrNotFound
}
func (memory *Memory) UpdateFleetScan(ctx context.Context, id, worker string, now time.Time, progress FleetScanProgress) (FleetScan, error) {
	if err := ctx.Err(); err != nil {
		return FleetScan{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	record, ok := memory.state.scans[id]
	if !ok {
		return FleetScan{}, ErrNotFound
	}
	if record.Status != FleetScanRunning || record.LeaseOwner != worker || record.LeaseExpiresAt == nil || record.LeaseExpiresAt.Before(now) {
		return FleetScan{}, ErrFleetScanConflict
	}
	if err := ValidateFleetScanProgress(progress); err != nil {
		return FleetScan{}, ErrFleetScanConflict
	}
	record.Expected, record.Processed, record.Succeeded, record.Failed = progress.Expected, progress.Processed, progress.Succeeded, progress.Failed
	record.LastError = progress.Error
	record.FailureDetails = append([]FleetTargetFailure(nil), progress.FailureDetails...)
	if progress.Status == "" {
		expires := now.UTC().Add(FleetScanLeaseDuration)
		if record.LeaseExpiresAt.After(expires) {
			expires = *record.LeaseExpiresAt
		}
		record.LeaseExpiresAt = &expires
	} else if progress.Status == FleetScanCompleted {
		completed := now.UTC()
		record.Status = FleetScanCompleted
		record.CompletedAt = &completed
		record.LeaseOwner = ""
		record.LeaseExpiresAt = nil
	} else if progress.Status == FleetScanFailed {
		record.LeaseOwner = ""
		record.LeaseExpiresAt = nil
		if record.Attempt < record.MaxAttempts {
			record.Status = FleetScanQueued
			record.AvailableAt = now.UTC().Add(FleetScanRetryDelay)
			record.CompletedAt = nil
		} else {
			completed := now.UTC()
			record.Status = FleetScanFailed
			record.CompletedAt = &completed
		}
	} else {
		return FleetScan{}, ErrFleetScanConflict
	}
	memory.state.scans[id] = record
	return cloneFleetScan(record), nil
}

func (tx *memoryTx) CreatePolicy(ctx context.Context, m PolicyMutation) (PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return PolicyRecord{}, err
	}
	return tx.memory.createPolicy(m)
}
func (tx *memoryTx) GetPolicy(ctx context.Context, id string) (PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return PolicyRecord{}, err
	}
	return tx.memory.GetPolicy(ctx, id)
}
func (tx *memoryTx) ListPolicies(ctx context.Context, o PolicyListOptions) (PolicyListResult, error) {
	if err := tx.check(ctx); err != nil {
		return PolicyListResult{}, err
	}
	return tx.memory.ListPolicies(ctx, o)
}
func (tx *memoryTx) UpdatePolicy(ctx context.Context, id string, v int64, m PolicyMutation) (PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return PolicyRecord{}, err
	}
	return tx.memory.updatePolicy(id, v, m)
}
func (tx *memoryTx) DeletePolicy(ctx context.Context, id string, v int64) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return tx.memory.deletePolicy(id, v)
}
func (tx *memoryTx) ResolvePolicy(ctx context.Context, s PolicyScope) (PolicyRecord, error) {
	if err := tx.check(ctx); err != nil {
		return PolicyRecord{}, err
	}
	return tx.memory.ResolvePolicy(ctx, s)
}
func (tx *memoryTx) CreateFleetSchedule(ctx context.Context, m FleetScheduleMutation) (FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return FleetSchedule{}, err
	}
	return tx.memory.createFleetSchedule(m)
}
func (tx *memoryTx) GetFleetSchedule(ctx context.Context, id string) (FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return FleetSchedule{}, err
	}
	return tx.memory.GetFleetSchedule(ctx, id)
}
func (tx *memoryTx) ListFleetSchedules(ctx context.Context, o FleetListOptions) (FleetScheduleListResult, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScheduleListResult{}, err
	}
	return tx.memory.ListFleetSchedules(ctx, o)
}
func (tx *memoryTx) UpdateFleetSchedule(ctx context.Context, id string, v int64, m FleetScheduleMutation) (FleetSchedule, error) {
	if err := tx.check(ctx); err != nil {
		return FleetSchedule{}, err
	}
	return tx.memory.updateFleetSchedule(id, v, m)
}
func (tx *memoryTx) DeleteFleetSchedule(ctx context.Context, id string, v int64) error {
	if err := tx.check(ctx); err != nil {
		return err
	}
	return tx.memory.deleteFleetSchedule(id, v)
}
func (tx *memoryTx) CreateFleetScan(ctx context.Context, r FleetScanRequest) (FleetScan, bool, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScan{}, false, err
	}
	return tx.memory.createFleetScan(r, "")
}
func (tx *memoryTx) GetFleetScan(ctx context.Context, id string) (FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScan{}, err
	}
	return tx.memory.GetFleetScan(ctx, id)
}
func (tx *memoryTx) ListFleetScans(ctx context.Context, o FleetListOptions) (FleetScanListResult, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScanListResult{}, err
	}
	return tx.memory.ListFleetScans(ctx, o)
}
func (tx *memoryTx) CancelFleetScan(ctx context.Context, id string, t time.Time) (FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScan{}, err
	}
	return tx.memory.cancelFleetScan(id, t)
}
func (tx *memoryTx) ResumeFleetScan(ctx context.Context, id string, t time.Time) (FleetScan, error) {
	if err := tx.check(ctx); err != nil {
		return FleetScan{}, err
	}
	return tx.memory.resumeFleetScan(id, t)
}
func (tx *memoryTx) EnqueueDueFleetScans(context.Context, time.Time, int) ([]FleetScan, error) {
	return nil, errors.New("enqueueing due scans is not supported inside a transaction")
}
func (tx *memoryTx) ClaimFleetScan(context.Context, string, time.Time, time.Duration) (FleetScan, error) {
	return FleetScan{}, errors.New("claiming scans is not supported inside a transaction")
}
func (tx *memoryTx) UpdateFleetScan(context.Context, string, string, time.Time, FleetScanProgress) (FleetScan, error) {
	return FleetScan{}, errors.New("updating scan workers is not supported inside a transaction")
}
