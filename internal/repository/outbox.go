package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Notification outbox statuses.
const (
	OutboxPending   = "pending"
	OutboxDelivered = "delivered"
	OutboxDead      = "dead"
)

// ErrNotReplayable means only a dead notification can be replayed.
var ErrNotReplayable = errors.New("only a dead notification can be replayed")

// OutboxEntry is one durable webhook delivery. Its ID is also the delivery
// ID receivers see, so a retried delivery can be deduplicated.
type OutboxEntry struct {
	ID            string          `json:"id"`
	EventType     string          `json:"eventType"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"nextAttemptAt"`
	CreatedAt     time.Time       `json:"createdAt"`
	DeliveredAt   *time.Time      `json:"deliveredAt,omitempty"`
	LastError     string          `json:"lastError,omitempty"`
}

// OutboxListResult is a page of outbox entries, newest first.
type OutboxListResult struct {
	Items    []OutboxEntry
	Page     int
	PageSize int
	Total    int
}

// NotificationOutbox stores webhook deliveries so a full queue, a failed
// receiver, or a restart cannot lose them.
//
// ClaimNotifications returns up to limit pending entries whose next attempt
// is due and leases them until now+lease, so replicas sharing the store never
// deliver one entry concurrently. CompleteNotification and
// FailNotification apply only to pending entries; FailNotification counts
// the attempt and either schedules the next one or, with dead set, moves
// the entry to the dead-letter state. ReplayNotification returns a dead
// entry to pending with its attempts reset. PruneNotifications deletes
// delivered entries older than the cutoff.
type NotificationOutbox interface {
	EnqueueNotification(ctx context.Context, eventType string, payload []byte, now time.Time) (OutboxEntry, error)
	ClaimNotifications(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxEntry, error)
	CompleteNotification(ctx context.Context, id string, at time.Time) error
	FailNotification(ctx context.Context, id, lastError string, next time.Time, dead bool) error
	ListNotifications(ctx context.Context, status string, page, pageSize int) (OutboxListResult, error)
	ReplayNotification(ctx context.Context, id string, now time.Time) (OutboxEntry, error)
	PruneNotifications(ctx context.Context, deliveredBefore time.Time) (int, error)
}

// NewNotificationID returns an outbox entry ID.
func NewNotificationID() (string, error) { return newID("ntf_") }

// ValidOutboxStatus reports whether a status filter is supported; empty
// selects every status.
func ValidOutboxStatus(status string) bool {
	return status == "" || status == OutboxPending || status == OutboxDelivered || status == OutboxDead
}

// ValidateNotification checks what every implementation stores.
func ValidateNotification(eventType string, payload []byte) error {
	if eventType == "" || len(eventType) > 128 {
		return errors.New("notification event type must be 1 to 128 characters")
	}
	if !json.Valid(payload) || len(payload) > 1<<20 {
		return errors.New("notification payload must be valid JSON of at most 1 MiB")
	}
	return nil
}

// TruncateDeliveryError bounds a stored delivery error.
func TruncateDeliveryError(message string) string {
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}

// ValidateNotificationList checks a list request.
func ValidateNotificationList(status string, page, pageSize int) error {
	if !ValidOutboxStatus(status) {
		return errors.New("notification status filter must be pending, delivered, or dead")
	}
	if page < 1 || pageSize < 1 || pageSize > 500 {
		return errors.New("page must be positive and pageSize between 1 and 500")
	}
	return nil
}
