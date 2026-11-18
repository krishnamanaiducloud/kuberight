package notify

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

// Durable delivery settings. Retries back off from durableBackoff, doubling
// up to durableMaxBackoff, so ten attempts span roughly four hours before an
// event is dead-lettered for replay.
const (
	durableAttempts   = 10
	durableBackoff    = 30 * time.Second
	durableMaxBackoff = time.Hour
	durableBatch      = 20
	durablePoll       = 5 * time.Second
	durableRetention  = 7 * 24 * time.Hour
)

// Durable delivers events through a database outbox. Notify stores each
// event, and a dispatcher claims due entries, posts them, and retries
// transient failures with exponential backoff. A rejected event, or one that
// still fails after durableAttempts, is dead-lettered and can be replayed.
// Replicas sharing the database claim disjoint entries. The entry ID is the
// delivery ID, so receivers can drop duplicates.
type Durable struct {
	webhook *Webhook
	outbox  repository.NotificationOutbox
	now     func() time.Time
	wake    chan struct{}
	stop    context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

// NewDurable starts a dispatcher that delivers through webhook from outbox.
// The webhook's own queue remains the fallback when the outbox cannot store
// an event.
func NewDurable(webhook *Webhook, outbox repository.NotificationOutbox, now func() time.Time) *Durable {
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	durable := &Durable{webhook: webhook, outbox: outbox, now: now, wake: make(chan struct{}, 1), stop: cancel, done: make(chan struct{})}
	go durable.run(ctx)
	return durable
}

// Notify stores the event without blocking the caller for longer than a
// short database write.
func (durable *Durable) Notify(event Event) {
	payload, err := json.Marshal(event)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err = durable.outbox.EnqueueNotification(ctx, event.Type, payload, durable.now())
		cancel()
	}
	if err != nil {
		durable.webhook.logger.Warn("webhook outbox is unavailable; delivering from memory", "type", event.Type, "recommendation_id", event.RecommendationID, "error", err)
		durable.webhook.Notify(event)
		return
	}
	select {
	case durable.wake <- struct{}{}:
	default:
	}
}

// Close stops the dispatcher. Undelivered entries stay in the outbox for
// the next start; the in-memory fallback queue is drained until ctx ends.
func (durable *Durable) Close(ctx context.Context) error {
	durable.once.Do(durable.stop)
	select {
	case <-durable.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return durable.webhook.Close(ctx)
}

// Stats reports delivery counters, including in-memory fallback deliveries.
func (durable *Durable) Stats() (delivered, failed, dropped uint64) { return durable.webhook.Stats() }

func (durable *Durable) run(ctx context.Context) {
	defer close(durable.done)
	ticker := time.NewTicker(durablePoll)
	defer ticker.Stop()
	pruned := time.Time{}
	for {
		durable.Pass(ctx)
		if now := durable.now(); now.Sub(pruned) >= time.Hour {
			if _, err := durable.outbox.PruneNotifications(ctx, now.Add(-durableRetention)); err != nil && ctx.Err() == nil {
				durable.webhook.logger.Warn("delivered webhook events could not be pruned", "error", err)
			}
			pruned = now
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-durable.wake:
		}
	}
}

// Pass delivers the entries that are due and returns how many it attempted.
func (durable *Durable) Pass(ctx context.Context) int {
	now := durable.now()
	// The lease outlasts one delivery attempt, so another replica cannot
	// claim an entry that is still being posted.
	entries, err := durable.outbox.ClaimNotifications(ctx, now, 2*durable.webhook.client.Timeout+30*time.Second, durableBatch)
	if err != nil {
		if ctx.Err() == nil {
			durable.webhook.logger.Warn("webhook outbox could not be read", "error", err)
		}
		return 0
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return len(entries)
		}
		durable.deliver(ctx, entry)
	}
	return len(entries)
}

func (durable *Durable) deliver(ctx context.Context, entry repository.OutboxEntry) {
	var event Event
	err := json.Unmarshal(entry.Payload, &event)
	var body []byte
	if err == nil {
		body, err = durable.webhook.body(event)
	}
	if err == nil {
		err = durable.webhook.post(entry.EventType, entry.ID, body)
	}
	now := durable.now()
	if err == nil {
		durable.webhook.delivered.Add(1)
		if completeErr := durable.outbox.CompleteNotification(ctx, entry.ID, now); completeErr != nil {
			durable.webhook.logger.Warn("delivered webhook event could not be marked delivered; it may be sent again", "notification_id", entry.ID, "error", completeErr)
		}
		return
	}
	var permanent permanentError
	dead := errors.As(err, &permanent) || entry.Attempts+1 >= durableAttempts
	backoff := min(durableMaxBackoff, durableBackoff<<min(entry.Attempts, 16))
	if failErr := durable.outbox.FailNotification(ctx, entry.ID, err.Error(), now.Add(backoff), dead); failErr != nil {
		durable.webhook.logger.Warn("webhook delivery failure could not be recorded", "notification_id", entry.ID, "error", failErr)
	}
	if dead {
		durable.webhook.failed.Add(1)
		durable.webhook.logger.Error("webhook event dead-lettered; replay it with POST /api/v1/notifications/{id}:replay", "notification_id", entry.ID, "type", entry.EventType, "attempts", entry.Attempts+1, "error", err)
		return
	}
	durable.webhook.logger.Warn("webhook delivery will be retried", "notification_id", entry.ID, "attempt", entry.Attempts+1, "next_attempt_in", backoff.String(), "error", err)
}
