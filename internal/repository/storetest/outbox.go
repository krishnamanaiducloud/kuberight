package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

// runOutbox checks the notification outbox contract.
func runOutbox(t *testing.T, newStore func(*testing.T) repository.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	t.Run("outbox delivers, retries, dead-letters, and replays", func(t *testing.T) {
		outbox, ok := newStore(t).(repository.NotificationOutbox)
		if !ok {
			t.Fatal("the store does not implement the notification outbox")
		}
		if _, err := outbox.EnqueueNotification(ctx, "recommendation.approved", []byte("{not json"), now); err == nil {
			t.Fatal("invalid JSON was enqueued")
		}
		first, err := outbox.EnqueueNotification(ctx, "recommendation.approved", []byte(`{"recommendationId":"rec_1"}`), now)
		if err != nil || !strings.HasPrefix(first.ID, "ntf_") || first.Status != repository.OutboxPending || first.Attempts != 0 {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		second, err := outbox.EnqueueNotification(ctx, "recommendation.regressed", []byte(`{"recommendationId":"rec_2"}`), now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		// A claim leases entries, so a second claim inside the lease sees none.
		claimed, err := outbox.ClaimNotifications(ctx, now.Add(2*time.Second), time.Minute, 10)
		if err != nil || len(claimed) != 2 || claimed[0].ID != first.ID || string(claimed[0].Payload) != `{"recommendationId": "rec_1"}` && string(claimed[0].Payload) != `{"recommendationId":"rec_1"}` {
			t.Fatalf("claimed=%+v err=%v", claimed, err)
		}
		if again, _ := outbox.ClaimNotifications(ctx, now.Add(30*time.Second), time.Minute, 10); len(again) != 0 {
			t.Fatalf("a leased entry was claimed twice: %+v", again)
		}
		if err := outbox.CompleteNotification(ctx, first.ID, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := outbox.FailNotification(ctx, second.ID, "receiver returned 503", now.Add(10*time.Second), false); err != nil {
			t.Fatal(err)
		}
		retry, err := outbox.ClaimNotifications(ctx, now.Add(10*time.Second), time.Minute, 10)
		if err != nil || len(retry) != 1 || retry[0].ID != second.ID || retry[0].Attempts != 1 || retry[0].LastError != "receiver returned 503" {
			t.Fatalf("retry=%+v err=%v", retry, err)
		}
		if err := outbox.FailNotification(ctx, second.ID, "receiver returned 410", now.Add(11*time.Second), true); err != nil {
			t.Fatal(err)
		}
		// Completing or failing an entry that is no longer pending is a no-op.
		if err := outbox.CompleteNotification(ctx, second.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := outbox.CompleteNotification(ctx, "ntf_00000000000000000000000000000000", now); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown complete: %v", err)
		}
		dead, err := outbox.ListNotifications(ctx, repository.OutboxDead, 1, 10)
		if err != nil || dead.Total != 1 || dead.Items[0].ID != second.ID || dead.Items[0].Attempts != 2 || dead.Items[0].Status != repository.OutboxDead {
			t.Fatalf("dead=%+v err=%v", dead, err)
		}
		all, err := outbox.ListNotifications(ctx, "", 1, 1)
		if err != nil || all.Total != 2 || len(all.Items) != 1 || all.Items[0].ID != second.ID {
			t.Fatalf("all=%+v err=%v", all, err)
		}
		if _, err := outbox.ListNotifications(ctx, "lost", 1, 10); err == nil {
			t.Fatal("an unknown status filter was accepted")
		}
		if _, err := outbox.ReplayNotification(ctx, first.ID, now); !errors.Is(err, repository.ErrNotReplayable) {
			t.Fatalf("replaying a delivered entry: %v", err)
		}
		if _, err := outbox.ReplayNotification(ctx, "ntf_00000000000000000000000000000000", now); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("replaying an unknown entry: %v", err)
		}
		replayed, err := outbox.ReplayNotification(ctx, second.ID, now.Add(time.Minute))
		if err != nil || replayed.Status != repository.OutboxPending || replayed.Attempts != 0 || replayed.LastError != "" {
			t.Fatalf("replayed=%+v err=%v", replayed, err)
		}
		if due, _ := outbox.ClaimNotifications(ctx, now.Add(time.Minute), time.Minute, 10); len(due) != 1 || due[0].ID != second.ID {
			t.Fatalf("a replayed entry must be due: %+v", due)
		}
		pruned, err := outbox.PruneNotifications(ctx, now.Add(time.Hour))
		if err != nil || pruned != 1 {
			t.Fatalf("pruned=%d err=%v", pruned, err)
		}
		if remaining, _ := outbox.ListNotifications(ctx, "", 1, 10); remaining.Total != 1 || remaining.Items[0].ID != second.ID {
			t.Fatalf("remaining=%+v", remaining)
		}
	})
}
