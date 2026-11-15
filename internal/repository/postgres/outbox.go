package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberight/kuberight/internal/repository"
)

var _ repository.NotificationOutbox = (*Store)(nil)

const outboxColumns = "id, event_type, payload, status, attempts, next_attempt_at, created_at, delivered_at, last_error"

// qualifiedOutboxColumns names the columns of an UPDATE ... FROM that joins
// another relation with an id column.
const qualifiedOutboxColumns = "notification_outbox.id, event_type, payload, status, attempts, next_attempt_at, created_at, delivered_at, last_error"

func scanOutbox(row pgx.Row, extra ...any) (repository.OutboxEntry, error) {
	var entry repository.OutboxEntry
	var payload []byte
	targets := append([]any{&entry.ID, &entry.EventType, &payload, &entry.Status, &entry.Attempts, &entry.NextAttemptAt, &entry.CreatedAt, &entry.DeliveredAt, &entry.LastError}, extra...)
	if err := row.Scan(targets...); err != nil {
		return repository.OutboxEntry{}, err
	}
	entry.Payload = payload
	entry.NextAttemptAt, entry.CreatedAt = entry.NextAttemptAt.UTC(), entry.CreatedAt.UTC()
	if entry.DeliveredAt != nil {
		at := entry.DeliveredAt.UTC()
		entry.DeliveredAt = &at
	}
	return entry, nil
}

// EnqueueNotification implements repository.NotificationOutbox.
func (store *Store) EnqueueNotification(ctx context.Context, eventType string, payload []byte, now time.Time) (repository.OutboxEntry, error) {
	if err := repository.ValidateNotification(eventType, payload); err != nil {
		return repository.OutboxEntry{}, err
	}
	id, err := repository.NewNotificationID()
	if err != nil {
		return repository.OutboxEntry{}, err
	}
	entry, err := scanOutbox(store.pool.QueryRow(ctx,
		`INSERT INTO notification_outbox (id, event_type, payload, status, next_attempt_at, created_at)
VALUES ($1, $2, $3, 'pending', $4, $4) RETURNING `+outboxColumns, id, eventType, payload, now.UTC()))
	if err != nil {
		return repository.OutboxEntry{}, fmt.Errorf("enqueue notification: %w", err)
	}
	return entry, nil
}

// ClaimNotifications implements repository.NotificationOutbox. SKIP LOCKED
// lets replicas claim disjoint entries without waiting on each other.
func (store *Store) ClaimNotifications(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]repository.OutboxEntry, error) {
	rows, err := store.pool.Query(ctx, `WITH due AS (
    SELECT id FROM notification_outbox
    WHERE status = 'pending' AND next_attempt_at <= $1
    ORDER BY next_attempt_at, id LIMIT $3
    FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE notification_outbox SET next_attempt_at = $2 FROM due
    WHERE notification_outbox.id = due.id
    RETURNING `+qualifiedOutboxColumns+`
)
SELECT `+outboxColumns+` FROM claimed ORDER BY created_at, id`, now.UTC(), now.Add(lease).UTC(), max(limit, 0))
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	defer rows.Close()
	claimed := []repository.OutboxEntry{}
	for rows.Next() {
		entry, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		claimed = append(claimed, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	return claimed, nil
}

// pendingUpdate runs an update that applies only to a pending entry and
// tells an unknown ID apart from one that is no longer pending.
func (store *Store) pendingUpdate(ctx context.Context, id, operation, query string, arguments ...any) error {
	tag, err := store.pool.Exec(ctx, query, append([]any{id}, arguments...)...)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM notification_outbox WHERE id = $1)", id).Scan(&exists); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if !exists {
		return repository.ErrNotFound
	}
	return nil
}

// CompleteNotification implements repository.NotificationOutbox.
func (store *Store) CompleteNotification(ctx context.Context, id string, at time.Time) error {
	return store.pendingUpdate(ctx, id, "complete notification",
		"UPDATE notification_outbox SET status = 'delivered', delivered_at = $2, last_error = '' WHERE id = $1 AND status = 'pending'", at.UTC())
}

// FailNotification implements repository.NotificationOutbox.
func (store *Store) FailNotification(ctx context.Context, id, lastError string, next time.Time, dead bool) error {
	return store.pendingUpdate(ctx, id, "fail notification",
		`UPDATE notification_outbox SET attempts = attempts + 1, last_error = $2, next_attempt_at = $3,
    status = CASE WHEN $4 THEN 'dead' ELSE 'pending' END
WHERE id = $1 AND status = 'pending'`, repository.TruncateDeliveryError(lastError), next.UTC(), dead)
}

// ListNotifications implements repository.NotificationOutbox.
func (store *Store) ListNotifications(ctx context.Context, status string, page, pageSize int) (repository.OutboxListResult, error) {
	if err := repository.ValidateNotificationList(status, page, pageSize); err != nil {
		return repository.OutboxListResult{}, err
	}
	rows, err := store.pool.Query(ctx, "SELECT "+outboxColumns+`, count(*) OVER () FROM notification_outbox
WHERE $1 = '' OR status = $1 ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, status, pageSize, (page-1)*pageSize)
	if err != nil {
		return repository.OutboxListResult{}, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	result := repository.OutboxListResult{Items: []repository.OutboxEntry{}, Page: page, PageSize: pageSize}
	for rows.Next() {
		var total int
		entry, err := scanOutbox(rows, &total)
		if err != nil {
			return repository.OutboxListResult{}, fmt.Errorf("scan notification: %w", err)
		}
		result.Items, result.Total = append(result.Items, entry), total
	}
	if err := rows.Err(); err != nil {
		return repository.OutboxListResult{}, fmt.Errorf("list notifications: %w", err)
	}
	if len(result.Items) == 0 && page > 1 {
		if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM notification_outbox WHERE $1 = '' OR status = $1", status).Scan(&result.Total); err != nil {
			return repository.OutboxListResult{}, fmt.Errorf("count notifications: %w", err)
		}
	}
	return result, nil
}

// ReplayNotification implements repository.NotificationOutbox.
func (store *Store) ReplayNotification(ctx context.Context, id string, now time.Time) (repository.OutboxEntry, error) {
	entry, err := scanOutbox(store.pool.QueryRow(ctx, `UPDATE notification_outbox SET status = 'pending', attempts = 0, last_error = '', next_attempt_at = $2
WHERE id = $1 AND status = 'dead' RETURNING `+outboxColumns, id, now.UTC()))
	if err == nil {
		return entry, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return repository.OutboxEntry{}, fmt.Errorf("replay notification: %w", err)
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM notification_outbox WHERE id = $1)", id).Scan(&exists); err != nil {
		return repository.OutboxEntry{}, fmt.Errorf("replay notification: %w", err)
	}
	if !exists {
		return repository.OutboxEntry{}, repository.ErrNotFound
	}
	return repository.OutboxEntry{}, repository.ErrNotReplayable
}

// PruneNotifications implements repository.NotificationOutbox.
func (store *Store) PruneNotifications(ctx context.Context, deliveredBefore time.Time) (int, error) {
	tag, err := store.pool.Exec(ctx, "DELETE FROM notification_outbox WHERE status = 'delivered' AND delivered_at < $1", deliveredBefore.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune notifications: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
