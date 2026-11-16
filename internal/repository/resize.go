package repository

import (
	"context"
	"errors"
	"time"
)

// ResizeState is what automation remembers between passes about an in-place
// resize that has not reached every pod of a workload. It is kept in the
// store so that a restart does not reset it and every API replica reads the
// same value.
type ResizeState struct {
	RecommendationID string
	// Failures counts consecutive failed attempts. NextAttemptAt is the
	// earliest time of the next one; the zero time means now.
	Failures      int
	NextAttemptAt time.Time
	// HaltedSince is when a pod that took the new resources was first seen
	// holding up the remaining pods, or nil.
	HaltedSince *time.Time
}

// ResizeStateStore keeps one ResizeState per recommendation.
type ResizeStateStore interface {
	// GetResizeState returns the state and whether one is stored.
	GetResizeState(ctx context.Context, recommendationID string) (ResizeState, bool, error)
	// SaveResizeState creates or replaces the state. It returns ErrNotFound
	// for a recommendation the store does not hold.
	SaveResizeState(ctx context.Context, state ResizeState) error
	// DeleteResizeState removes the state; deleting none is not an error.
	DeleteResizeState(ctx context.Context, recommendationID string) error
}

// ValidateResizeState rejects a state no store should hold.
func ValidateResizeState(state ResizeState) error {
	if state.RecommendationID == "" {
		return errors.New("resize state needs a recommendation")
	}
	if state.Failures < 0 {
		return errors.New("resize state failures cannot be negative")
	}
	return nil
}

var _ ResizeStateStore = (*Memory)(nil)

func cloneResizeState(state ResizeState) ResizeState {
	state.NextAttemptAt = state.NextAttemptAt.UTC()
	if state.HaltedSince != nil {
		at := state.HaltedSince.UTC()
		state.HaltedSince = &at
	}
	return state
}

// GetResizeState implements ResizeStateStore.
func (memory *Memory) GetResizeState(ctx context.Context, recommendationID string) (ResizeState, bool, error) {
	if err := ctx.Err(); err != nil {
		return ResizeState{}, false, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	state, found := memory.resize[recommendationID]
	if _, held := memory.state.byID[recommendationID]; !found || !held {
		return ResizeState{}, false, nil
	}
	return cloneResizeState(state), true, nil
}

// SaveResizeState implements ResizeStateStore.
func (memory *Memory) SaveResizeState(ctx context.Context, state ResizeState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateResizeState(state); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, held := memory.state.byID[state.RecommendationID]; !held {
		return ErrNotFound
	}
	if memory.resize == nil {
		memory.resize = map[string]ResizeState{}
	}
	// The state of an evicted recommendation goes with it.
	for id := range memory.resize {
		if _, held := memory.state.byID[id]; !held {
			delete(memory.resize, id)
		}
	}
	memory.resize[state.RecommendationID] = cloneResizeState(state)
	return nil
}

// DeleteResizeState implements ResizeStateStore.
func (memory *Memory) DeleteResizeState(ctx context.Context, recommendationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	delete(memory.resize, recommendationID)
	return nil
}
