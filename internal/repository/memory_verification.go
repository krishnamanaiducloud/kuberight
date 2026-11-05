package repository

import (
	"context"
	"errors"
	"sort"
	"time"
)

func (memory *Memory) RecordVerification(ctx context.Context, run VerificationRun) (VerificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return VerificationOutcome{}, err
	}
	if err := ValidateVerificationRun(run); err != nil {
		return VerificationOutcome{}, err
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.state.byID[run.RecommendationID]; !exists {
		return VerificationOutcome{}, ErrNotFound
	}
	if run.ID == "" {
		id, err := NewVerificationRunID()
		if err != nil {
			return VerificationOutcome{}, errors.New("could not generate verification run identifier")
		}
		run.ID = id
	}
	run = cloneVerificationRun(run)
	existing, found := memory.state.verificationOutcomes[run.RecommendationID]
	mergeFirstObservation(&run, existing, found)
	memory.state.verificationRuns = append(memory.state.verificationRuns, cloneVerificationRun(run))
	outcome := VerificationOutcome{VerificationRun: run}
	if found {
		outcome.RegressionNotifiedAt = existing.RegressionNotifiedAt
	}
	memory.state.verificationOutcomes[run.RecommendationID] = cloneVerificationOutcome(outcome)
	return cloneVerificationOutcome(outcome), nil
}

func mergeFirstObservation(run *VerificationRun, existing VerificationOutcome, found bool) {
	if !found || existing.FirstObservedAt == nil {
		return
	}
	if run.FirstObservedAt == nil || existing.FirstObservedAt.Before(*run.FirstObservedAt) {
		value := existing.FirstObservedAt.UTC()
		run.FirstObservedAt = &value
		run.ObservedGeneration = existing.ObservedGeneration
		run.ObservedSpecFingerprint = existing.ObservedSpecFingerprint
	}
}

func (memory *Memory) GetVerificationOutcome(ctx context.Context, recommendationID string) (VerificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return VerificationOutcome{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	value, found := memory.state.verificationOutcomes[recommendationID]
	if !found {
		return VerificationOutcome{}, ErrNotFound
	}
	return cloneVerificationOutcome(value), nil
}

func validateVerificationPagination(page, pageSize int) error {
	if page < 1 || pageSize < 1 || pageSize > 10_000 {
		return errors.New("page must be positive and pageSize must be between 1 and 10000")
	}
	return nil
}

func (memory *Memory) ListVerificationOutcomes(ctx context.Context, page, pageSize int) (VerificationOutcomeListResult, error) {
	if err := ctx.Err(); err != nil {
		return VerificationOutcomeListResult{}, err
	}
	if err := validateVerificationPagination(page, pageSize); err != nil {
		return VerificationOutcomeListResult{}, err
	}
	memory.mu.RLock()
	items := make([]VerificationOutcome, 0, len(memory.state.verificationOutcomes))
	for _, value := range memory.state.verificationOutcomes {
		items = append(items, cloneVerificationOutcome(value))
	}
	memory.mu.RUnlock()
	sort.Slice(items, func(left, right int) bool {
		if items[left].CheckedAt.Equal(items[right].CheckedAt) {
			return items[left].RecommendationID > items[right].RecommendationID
		}
		return items[left].CheckedAt.After(items[right].CheckedAt)
	})
	total := len(items)
	start := min((page-1)*pageSize, total)
	end := min(start+pageSize, total)
	return VerificationOutcomeListResult{Items: items[start:end], Page: page, PageSize: pageSize, Total: total}, nil
}

func (memory *Memory) ListVerificationRuns(ctx context.Context, recommendationID string, page, pageSize int) (VerificationRunListResult, error) {
	if err := ctx.Err(); err != nil {
		return VerificationRunListResult{}, err
	}
	if err := validateVerificationPagination(page, pageSize); err != nil {
		return VerificationRunListResult{}, err
	}
	memory.mu.RLock()
	items := make([]VerificationRun, 0)
	for _, value := range memory.state.verificationRuns {
		if value.RecommendationID == recommendationID {
			items = append(items, cloneVerificationRun(value))
		}
	}
	memory.mu.RUnlock()
	sort.Slice(items, func(left, right int) bool {
		if items[left].CheckedAt.Equal(items[right].CheckedAt) {
			return items[left].ID > items[right].ID
		}
		return items[left].CheckedAt.After(items[right].CheckedAt)
	})
	total := len(items)
	start := min((page-1)*pageSize, total)
	end := min(start+pageSize, total)
	return VerificationRunListResult{Items: items[start:end], Page: page, PageSize: pageSize, Total: total}, nil
}

func (memory *Memory) ClaimRegressionNotification(ctx context.Context, recommendationID string, checkedAt, notifiedAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if notifiedAt.IsZero() {
		return false, errors.New("regression notification time is required")
	}
	memory.writeMu.Lock()
	defer memory.writeMu.Unlock()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	outcome, found := memory.state.verificationOutcomes[recommendationID]
	if !found {
		return false, ErrNotFound
	}
	if outcome.Status != VerificationRegressed || !outcome.CheckedAt.Equal(checkedAt) || outcome.RegressionNotifiedAt != nil {
		return false, nil
	}
	value := notifiedAt.UTC()
	outcome.RegressionNotifiedAt = &value
	memory.state.verificationOutcomes[recommendationID] = cloneVerificationOutcome(outcome)
	return true, nil
}
