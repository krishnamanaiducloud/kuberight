package repository

import (
	"context"
	"sort"
	"time"
)

var _ NotificationOutbox = (*Memory)(nil)

// memoryOutbox holds outbox entries for the in-process store. Like the rest
// of the in-memory store, it is lost at restart.
type memoryOutbox struct {
	entries map[string]*OutboxEntry
}

func (memory *Memory) outboxEntries() map[string]*OutboxEntry {
	if memory.outbox.entries == nil {
		memory.outbox.entries = map[string]*OutboxEntry{}
	}
	return memory.outbox.entries
}

func cloneOutbox(entry OutboxEntry) OutboxEntry {
	entry.Payload = append([]byte(nil), entry.Payload...)
	if entry.DeliveredAt != nil {
		at := *entry.DeliveredAt
		entry.DeliveredAt = &at
	}
	return entry
}

// EnqueueNotification implements NotificationOutbox.
func (memory *Memory) EnqueueNotification(ctx context.Context, eventType string, payload []byte, now time.Time) (OutboxEntry, error) {
	if err := ctx.Err(); err != nil {
		return OutboxEntry{}, err
	}
	if err := ValidateNotification(eventType, payload); err != nil {
		return OutboxEntry{}, err
	}
	id, err := NewNotificationID()
	if err != nil {
		return OutboxEntry{}, err
	}
	now = now.UTC()
	entry := OutboxEntry{ID: id, EventType: eventType, Payload: append([]byte(nil), payload...), Status: OutboxPending, NextAttemptAt: now, CreatedAt: now}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.outboxEntries()[id] = &entry
	return cloneOutbox(entry), nil
}

// ClaimNotifications implements NotificationOutbox.
func (memory *Memory) ClaimNotifications(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	due := []*OutboxEntry{}
	for _, entry := range memory.outboxEntries() {
		if entry.Status == OutboxPending && !entry.NextAttemptAt.After(now) {
			due = append(due, entry)
		}
	}
	sort.Slice(due, func(left, right int) bool {
		if !due[left].NextAttemptAt.Equal(due[right].NextAttemptAt) {
			return due[left].NextAttemptAt.Before(due[right].NextAttemptAt)
		}
		return due[left].ID < due[right].ID
	})
	claimed := []OutboxEntry{}
	for _, entry := range due[:min(len(due), max(limit, 0))] {
		entry.NextAttemptAt = now.Add(lease).UTC()
		claimed = append(claimed, cloneOutbox(*entry))
	}
	// Claimed entries are returned oldest first.
	sort.Slice(claimed, func(left, right int) bool {
		if !claimed[left].CreatedAt.Equal(claimed[right].CreatedAt) {
			return claimed[left].CreatedAt.Before(claimed[right].CreatedAt)
		}
		return claimed[left].ID < claimed[right].ID
	})
	return claimed, nil
}

func (memory *Memory) pendingOutbox(id string) (*OutboxEntry, error) {
	entry, found := memory.outboxEntries()[id]
	if !found {
		return nil, ErrNotFound
	}
	if entry.Status != OutboxPending {
		return nil, nil
	}
	return entry, nil
}

// CompleteNotification implements NotificationOutbox.
func (memory *Memory) CompleteNotification(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry, err := memory.pendingOutbox(id)
	if entry == nil {
		return err
	}
	at = at.UTC()
	entry.Status, entry.DeliveredAt, entry.LastError = OutboxDelivered, &at, ""
	return nil
}

// FailNotification implements NotificationOutbox.
func (memory *Memory) FailNotification(ctx context.Context, id, lastError string, next time.Time, dead bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry, err := memory.pendingOutbox(id)
	if entry == nil {
		return err
	}
	entry.Attempts++
	entry.LastError, entry.NextAttemptAt = TruncateDeliveryError(lastError), next.UTC()
	if dead {
		entry.Status = OutboxDead
	}
	return nil
}

// ListNotifications implements NotificationOutbox.
func (memory *Memory) ListNotifications(ctx context.Context, status string, page, pageSize int) (OutboxListResult, error) {
	if err := ctx.Err(); err != nil {
		return OutboxListResult{}, err
	}
	if err := ValidateNotificationList(status, page, pageSize); err != nil {
		return OutboxListResult{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	matched := []OutboxEntry{}
	for _, entry := range memory.outbox.entries {
		if status == "" || entry.Status == status {
			matched = append(matched, cloneOutbox(*entry))
		}
	}
	sort.Slice(matched, func(left, right int) bool {
		if !matched[left].CreatedAt.Equal(matched[right].CreatedAt) {
			return matched[left].CreatedAt.After(matched[right].CreatedAt)
		}
		return matched[left].ID < matched[right].ID
	})
	start := min(len(matched), (page-1)*pageSize)
	return OutboxListResult{Items: matched[start:min(len(matched), start+pageSize)], Page: page, PageSize: pageSize, Total: len(matched)}, nil
}

// ReplayNotification implements NotificationOutbox.
func (memory *Memory) ReplayNotification(ctx context.Context, id string, now time.Time) (OutboxEntry, error) {
	if err := ctx.Err(); err != nil {
		return OutboxEntry{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry, found := memory.outboxEntries()[id]
	if !found {
		return OutboxEntry{}, ErrNotFound
	}
	if entry.Status != OutboxDead {
		return OutboxEntry{}, ErrNotReplayable
	}
	entry.Status, entry.Attempts, entry.LastError, entry.NextAttemptAt = OutboxPending, 0, "", now.UTC()
	return cloneOutbox(*entry), nil
}

// PruneNotifications implements NotificationOutbox.
func (memory *Memory) PruneNotifications(ctx context.Context, deliveredBefore time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	pruned := 0
	for id, entry := range memory.outboxEntries() {
		if entry.Status == OutboxDelivered && entry.DeliveredAt != nil && entry.DeliveredAt.Before(deliveredBefore) {
			delete(memory.outbox.entries, id)
			pruned++
		}
	}
	return pruned, nil
}
