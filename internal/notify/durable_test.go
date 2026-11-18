package notify

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

// clock is a settable test time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// durableHarness runs the dispatcher by hand: its background loop is
// stopped, and Pass is called with a controlled clock.
func durableHarness(t *testing.T, status func(attempt int) int) (*Durable, *repository.Memory, *clock, *[]string) {
	t.Helper()
	var mu sync.Mutex
	deliveries := &[]string{}
	attempts := 0
	webhook := newTestWebhook(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		*deliveries = append(*deliveries, r.Header.Get("X-KubeRight-Delivery"))
		w.WriteHeader(status(attempts))
	}, "json")
	store := repository.NewMemory(100)
	at := &clock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	durable := NewDurable(webhook, store, at.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	durable.once.Do(durable.stop)
	<-durable.done
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeWebhook(t, webhook) })
	return durable, store, at, deliveries
}

func TestDurableDeliveryRetriesWithTheSameDeliveryID(t *testing.T) {
	durable, store, at, deliveries := durableHarness(t, func(attempt int) int {
		if attempt < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	durable.Notify(sample)
	ctx := context.Background()
	if attempted := durable.Pass(ctx); attempted != 1 {
		t.Fatalf("attempted %d", attempted)
	}
	// The retry waits for its backoff: 30s, then 60s.
	if attempted := durable.Pass(ctx); attempted != 0 {
		t.Fatalf("a retry ran before its backoff: %d", attempted)
	}
	at.Advance(30 * time.Second)
	durable.Pass(ctx)
	at.Advance(time.Minute)
	durable.Pass(ctx)
	entries, _ := store.ListNotifications(ctx, "", 1, 10)
	entry := entries.Items[0]
	if entry.Status != repository.OutboxDelivered || entry.Attempts != 2 || len(*deliveries) != 3 {
		t.Fatalf("entry=%+v deliveries=%v", entry, *deliveries)
	}
	for _, delivery := range *deliveries {
		if delivery != entry.ID {
			t.Fatalf("delivery IDs must be the entry ID so receivers can deduplicate: %v", *deliveries)
		}
	}
	if delivered, failed, dropped := durable.Stats(); delivered != 1 || failed != 0 || dropped != 0 {
		t.Fatalf("stats delivered=%d failed=%d dropped=%d", delivered, failed, dropped)
	}
}

func TestDurableDeliveryDeadLettersAndReplays(t *testing.T) {
	rejecting := true
	durable, store, at, _ := durableHarness(t, func(int) int {
		if rejecting {
			return http.StatusGone
		}
		return http.StatusNoContent
	})
	ctx := context.Background()
	durable.Notify(sample)
	durable.Pass(ctx)
	dead, _ := store.ListNotifications(ctx, repository.OutboxDead, 1, 10)
	if dead.Total != 1 || dead.Items[0].Attempts != 1 || dead.Items[0].LastError == "" {
		t.Fatalf("a rejected event is dead-lettered at once: %+v", dead)
	}
	rejecting = false
	if _, err := store.ReplayNotification(ctx, dead.Items[0].ID, at.Now()); err != nil {
		t.Fatal(err)
	}
	durable.Pass(ctx)
	if delivered, _ := store.ListNotifications(ctx, repository.OutboxDelivered, 1, 10); delivered.Total != 1 {
		t.Fatalf("the replayed event was not delivered: %+v", delivered)
	}
}

func TestDurableDeliveryGivesUpAfterTheAttemptLimit(t *testing.T) {
	durable, store, at, deliveries := durableHarness(t, func(int) int { return http.StatusBadGateway })
	ctx := context.Background()
	durable.Notify(sample)
	for range durableAttempts {
		durable.Pass(ctx)
		at.Advance(durableMaxBackoff)
	}
	dead, _ := store.ListNotifications(ctx, repository.OutboxDead, 1, 10)
	if dead.Total != 1 || dead.Items[0].Attempts != durableAttempts || len(*deliveries) != durableAttempts {
		t.Fatalf("dead=%+v deliveries=%d", dead, len(*deliveries))
	}
	if _, failed, _ := durable.Stats(); failed != 1 {
		t.Fatalf("failed=%d", failed)
	}
}

type brokenOutbox struct{ repository.NotificationOutbox }

func (brokenOutbox) EnqueueNotification(context.Context, string, []byte, time.Time) (repository.OutboxEntry, error) {
	return repository.OutboxEntry{}, errors.New("database is down")
}

func TestDurableFallsBackToMemoryWhenTheOutboxIsDown(t *testing.T) {
	received := make(chan string, 1)
	webhook := newTestWebhook(t, func(w http.ResponseWriter, r *http.Request) { received <- r.Header.Get("X-KubeRight-Event") }, "json")
	durable := NewDurable(webhook, brokenOutbox{repository.NewMemory(10)}, nil)
	durable.Notify(sample)
	select {
	case event := <-received:
		if event != RecommendationApproved {
			t.Fatalf("event=%s", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fallback queue did not deliver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := durable.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
