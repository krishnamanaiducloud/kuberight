package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuberight/kuberight/internal/repository"
)

var _ repository.ResizeStateStore = (*Store)(nil)

// GetResizeState implements repository.ResizeStateStore.
func (store *Store) GetResizeState(ctx context.Context, recommendationID string) (repository.ResizeState, bool, error) {
	state := repository.ResizeState{RecommendationID: recommendationID}
	var next, halted *time.Time
	err := store.pool.QueryRow(ctx,
		"SELECT failures, next_attempt_at, halted_since FROM automation_resize_state WHERE recommendation_id = $1", recommendationID).
		Scan(&state.Failures, &next, &halted)
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.ResizeState{}, false, nil
	}
	if err != nil {
		return repository.ResizeState{}, false, fmt.Errorf("read resize state: %w", err)
	}
	if next != nil {
		state.NextAttemptAt = next.UTC()
	}
	if halted != nil {
		at := halted.UTC()
		state.HaltedSince = &at
	}
	return state, true, nil
}

// SaveResizeState implements repository.ResizeStateStore.
func (store *Store) SaveResizeState(ctx context.Context, state repository.ResizeState) error {
	if err := repository.ValidateResizeState(state); err != nil {
		return err
	}
	var next, halted *time.Time
	if !state.NextAttemptAt.IsZero() {
		at := state.NextAttemptAt.UTC()
		next = &at
	}
	if state.HaltedSince != nil {
		at := state.HaltedSince.UTC()
		halted = &at
	}
	tag, err := store.pool.Exec(ctx, `
INSERT INTO automation_resize_state (recommendation_id, failures, next_attempt_at, halted_since, updated_at)
SELECT $1, $2, $3, $4, now() WHERE EXISTS (SELECT 1 FROM recommendations WHERE id = $1)
ON CONFLICT (recommendation_id) DO UPDATE
SET failures = EXCLUDED.failures, next_attempt_at = EXCLUDED.next_attempt_at, halted_since = EXCLUDED.halted_since, updated_at = EXCLUDED.updated_at`,
		state.RecommendationID, state.Failures, next, halted)
	if err != nil {
		return fmt.Errorf("save resize state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// DeleteResizeState implements repository.ResizeStateStore.
func (store *Store) DeleteResizeState(ctx context.Context, recommendationID string) error {
	if _, err := store.pool.Exec(ctx, "DELETE FROM automation_resize_state WHERE recommendation_id = $1", recommendationID); err != nil {
		return fmt.Errorf("delete resize state: %w", err)
	}
	return nil
}

// ListResizeStates implements repository.ResizeStateStore. The state that
// was written longest ago comes first, so a limit never starves one.
func (store *Store) ListResizeStates(ctx context.Context, limit int) ([]repository.ResizeState, error) {
	if limit < 0 {
		limit = 0
	}
	rows, err := store.pool.Query(ctx,
		"SELECT recommendation_id, failures, next_attempt_at, halted_since FROM automation_resize_state ORDER BY updated_at, recommendation_id LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("list resize states: %w", err)
	}
	defer rows.Close()
	states := []repository.ResizeState{}
	for rows.Next() {
		var state repository.ResizeState
		var next, halted *time.Time
		if err := rows.Scan(&state.RecommendationID, &state.Failures, &next, &halted); err != nil {
			return nil, fmt.Errorf("read resize state: %w", err)
		}
		if next != nil {
			state.NextAttemptAt = next.UTC()
		}
		if halted != nil {
			at := halted.UTC()
			state.HaltedSince = &at
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list resize states: %w", err)
	}
	return states, nil
}
