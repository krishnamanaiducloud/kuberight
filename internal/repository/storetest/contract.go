// Package storetest contains the behavioral contract every repository.Store
// implementation must satisfy. Implementations call Run from their own tests.
package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

var recommendationID = regexp.MustCompile(`^rec_[0-9a-f]{32}$`)

// Fingerprint returns a valid request fingerprint for test payload text.
func Fingerprint(payload string) string {
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

// Recommendation returns a minimal recommendation that satisfies every
// durable-schema constraint.
func Recommendation(namespace, workload, container, risk string) optimizer.Recommendation {
	return optimizer.Recommendation{
		Cluster: "local", Namespace: namespace, Kind: "Deployment", Workload: workload, Container: container,
		CPURequestMilli: 100, MemoryRequestBytes: 64 << 20, Confidence: "medium", ConfidenceScore: 0.6, Risk: risk,
		Warnings: []string{"test warning"}, DataQuality: optimizer.DataQuality{Issues: []string{}},
	}
}

// Run executes the contract. newStore must return an empty store.
func Run(t *testing.T, newStore func(*testing.T) repository.Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("create assigns identity and pending review", func(t *testing.T) {
		store := newStore(t)
		record, created, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{CreatedBy: "alice", Source: repository.SourcePrometheus})
		if err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		if !recommendationID.MatchString(record.ID) || record.ReviewStatus != repository.ReviewPending || record.CreatedBy != "alice" || record.Source != repository.SourcePrometheus || record.CreatedAt.IsZero() {
			t.Fatalf("unexpected record: %#v", record)
		}
		stored, err := store.GetRecommendation(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ID != record.ID || stored.Workload != "api" || len(stored.Warnings) != 1 || stored.Source != repository.SourcePrometheus {
			t.Fatalf("stored record mismatch: %#v", stored)
		}
		count, err := store.CountRecommendations(ctx)
		if err != nil || count != 1 {
			t.Fatalf("count=%d err=%v", count, err)
		}
	})

	t.Run("empty lists serialize as arrays", func(t *testing.T) {
		store := newStore(t)
		recommendation := Recommendation("shop", "api", "app", "low")
		recommendation.Warnings = []string{}
		record, _, err := store.CreateRecommendation(ctx, recommendation, repository.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, read := range []func() (repository.RecommendationRecord, error){
			func() (repository.RecommendationRecord, error) { return store.GetRecommendation(ctx, record.ID) },
			func() (repository.RecommendationRecord, error) {
				result, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10})
				if err != nil || len(result.Items) == 0 {
					return repository.RecommendationRecord{}, err
				}
				return result.Items[0], nil
			},
		} {
			stored, err := read()
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(stored)
			if !strings.Contains(string(payload), `"warnings":[]`) || !strings.Contains(string(payload), `"issues":[]`) {
				t.Fatalf("empty lists must serialize as [] for API clients: %s", payload)
			}
		}
	})

	t.Run("source defaults to request", func(t *testing.T) {
		store := newStore(t)
		record, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{})
		if err != nil || record.Source != repository.SourceRequest {
			t.Fatalf("source=%q err=%v", record.Source, err)
		}
	})

	t.Run("unknown recommendation is not found", func(t *testing.T) {
		store := newStore(t)
		if _, err := store.GetRecommendation(ctx, "rec_00000000000000000000000000000000"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("idempotency is scoped and payload bound", func(t *testing.T) {
		store := newStore(t)
		recommendation := Recommendation("shop", "api", "app", "low")
		one := repository.CreateOptions{IdempotencyKey: "shared", Scope: "alice\x00default", Fingerprint: Fingerprint("one")}
		first, created, err := store.CreateRecommendation(ctx, recommendation, one)
		if err != nil || !created {
			t.Fatalf("first created=%v err=%v", created, err)
		}
		replay, created, err := store.CreateRecommendation(ctx, recommendation, one)
		if err != nil || created || replay.ID != first.ID {
			t.Fatalf("replay id=%q created=%v err=%v, want %q", replay.ID, created, err, first.ID)
		}
		other := one
		other.Scope = "bob\x00default"
		second, created, err := store.CreateRecommendation(ctx, recommendation, other)
		if err != nil || !created || second.ID == first.ID {
			t.Fatalf("other scope id=%q created=%v err=%v", second.ID, created, err)
		}
		changed := one
		changed.Fingerprint = Fingerprint("two")
		if _, _, err := store.CreateRecommendation(ctx, recommendation, changed); !errors.Is(err, repository.ErrIdempotencyConflict) {
			t.Fatalf("got %v, want ErrIdempotencyConflict", err)
		}
		if count, _ := store.CountRecommendations(ctx); count != 2 {
			t.Fatalf("conflict wrote a record: count=%d", count)
		}
	})

	t.Run("invalid idempotency claim is rejected", func(t *testing.T) {
		store := newStore(t)
		_, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{IdempotencyKey: "bad key", Scope: "s", Fingerprint: Fingerprint("x")})
		if !errors.Is(err, repository.ErrInvalidIdempotency) {
			t.Fatalf("got %v, want ErrInvalidIdempotency", err)
		}
	})

	t.Run("concurrent replays create one record", func(t *testing.T) {
		store := newStore(t)
		options := repository.CreateOptions{IdempotencyKey: "race", Scope: "alice", Fingerprint: Fingerprint("race")}
		var wait sync.WaitGroup
		ids := make(chan string, 16)
		createdCount := make(chan bool, 16)
		for range 16 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				record, created, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), options)
				if err != nil {
					t.Errorf("create failed: %v", err)
					return
				}
				ids <- record.ID
				createdCount <- created
			}()
		}
		wait.Wait()
		close(ids)
		close(createdCount)
		unique := map[string]struct{}{}
		for id := range ids {
			unique[id] = struct{}{}
		}
		creations := 0
		for created := range createdCount {
			if created {
				creations++
			}
		}
		if len(unique) != 1 || creations != 1 {
			t.Fatalf("got %d distinct IDs and %d creations, want 1 and 1", len(unique), creations)
		}
	})

	t.Run("list filters sorts and pages", func(t *testing.T) {
		store := newStore(t)
		for _, risk := range []string{"high", "low", "medium"} {
			if _, _, err := store.CreateRecommendation(ctx, Recommendation("shop", risk, "app", risk), repository.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := store.CreateRecommendation(ctx, Recommendation("billing", "ledger", "app", "low"), repository.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		result, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, Namespace: "shop", Sort: "risk", Order: "asc"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Total != 3 || len(result.Items) != 3 || result.Items[0].Risk != "low" || result.Items[1].Risk != "medium" || result.Items[2].Risk != "high" {
			t.Fatalf("unexpected risk order: %#v", result.Items)
		}
		paged, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 2, PageSize: 3, Sort: "createdAt", Order: "desc"})
		if err != nil || paged.Total != 4 || len(paged.Items) != 1 {
			t.Fatalf("page 2: total=%d items=%d err=%v", paged.Total, len(paged.Items), err)
		}
		filtered, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, Filter: "LEDG"})
		if err != nil || filtered.Total != 1 || filtered.Items[0].Workload != "ledger" {
			t.Fatalf("filter: %#v err=%v", filtered, err)
		}
		risky, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, Risk: "low"})
		if err != nil || risky.Total != 2 {
			t.Fatalf("risk filter total=%d err=%v", risky.Total, err)
		}
	})

	t.Run("latest only keeps newest per container", func(t *testing.T) {
		store := newStore(t)
		older, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "high"), repository.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		newer, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "sidecar", "low"), repository.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		result, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, LatestOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.Total != 2 {
			t.Fatalf("latest total=%d, want 2", result.Total)
		}
		for _, item := range result.Items {
			if item.ID == older.ID {
				t.Fatalf("latest list returned superseded record %s instead of %s", older.ID, newer.ID)
			}
		}
		// Filters apply after the latest record is chosen, so a superseded
		// high-risk record must not reappear.
		highRisk, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, LatestOnly: true, Risk: "high"})
		if err != nil || highRisk.Total != 0 {
			t.Fatalf("latest high-risk total=%d err=%v, want 0", highRisk.Total, err)
		}
	})

	t.Run("review is single assignment", func(t *testing.T) {
		store := newStore(t)
		record, _, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{CreatedBy: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Microsecond)
		reviewed, err := store.ReviewRecommendation(ctx, record.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", Comment: "looks safe", At: at})
		if err != nil {
			t.Fatal(err)
		}
		if reviewed.ReviewStatus != repository.ReviewApproved || reviewed.ReviewedBy != "bob" || reviewed.ReviewComment != "looks safe" || reviewed.ReviewedAt == nil || !reviewed.ReviewedAt.Equal(at) {
			t.Fatalf("unexpected review: %#v", reviewed)
		}
		if _, err := store.ReviewRecommendation(ctx, record.ID, repository.ReviewDecision{Status: repository.ReviewRejected, Reviewer: "carol", At: at}); !errors.Is(err, repository.ErrReviewConflict) {
			t.Fatalf("second review: got %v, want ErrReviewConflict", err)
		}
		stored, err := store.GetRecommendation(ctx, record.ID)
		if err != nil || stored.ReviewStatus != repository.ReviewApproved || stored.ReviewedBy != "bob" {
			t.Fatalf("stored review: %#v err=%v", stored, err)
		}
		pending, err := store.ListRecommendations(ctx, repository.ListOptions{Page: 1, PageSize: 10, ReviewStatus: repository.ReviewPending})
		if err != nil || pending.Total != 0 {
			t.Fatalf("pending total=%d err=%v", pending.Total, err)
		}
		if _, err := store.ReviewRecommendation(ctx, "rec_00000000000000000000000000000000", repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", At: at}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("unknown review: got %v, want ErrNotFound", err)
		}
		if _, err := store.ReviewRecommendation(ctx, record.ID, repository.ReviewDecision{Status: "maybe", Reviewer: "bob", At: at}); !errors.Is(err, repository.ErrInvalidReview) {
			t.Fatalf("invalid review: got %v, want ErrInvalidReview", err)
		}
	})

	t.Run("audit lists newest first", func(t *testing.T) {
		store := newStore(t)
		base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		for index, action := range []string{"create", "replay", "approve"} {
			if err := store.AppendAuditEvent(ctx, repository.AuditEvent{Timestamp: base.Add(time.Duration(index) * time.Minute), Actor: "alice", Action: action, Resource: "recommendations/x", Namespace: "shop", Outcome: "success", RequestID: "req-" + action}); err != nil {
				t.Fatal(err)
			}
		}
		result, err := store.ListAuditEvents(ctx, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if result.Total != 3 || len(result.Items) != 2 || result.Items[0].Action != "approve" || result.Items[1].Action != "replay" {
			t.Fatalf("unexpected audit page: %#v", result)
		}
		if result.Items[0].ID == "" {
			t.Fatal("audit event ID was not assigned")
		}
		last, err := store.ListAuditEvents(ctx, 2, 2)
		if err != nil || len(last.Items) != 1 || last.Items[0].Action != "create" {
			t.Fatalf("unexpected last page: %#v err=%v", last, err)
		}
	})

	t.Run("transaction commits", func(t *testing.T) {
		store := newStore(t)
		err := store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
			record, _, err := tx.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{})
			if err != nil {
				return err
			}
			return tx.AppendAuditEvent(ctx, repository.AuditEvent{Actor: "alice", Action: "create", Resource: "recommendations/" + record.ID, Outcome: "success", RequestID: "req-1"})
		})
		if err != nil {
			t.Fatal(err)
		}
		count, _ := store.CountRecommendations(ctx)
		audit, _ := store.ListAuditEvents(ctx, 1, 10)
		if count != 1 || audit.Total != 1 {
			t.Fatalf("count=%d audit=%d, want 1 and 1", count, audit.Total)
		}
	})

	t.Run("transaction rolls back on error", func(t *testing.T) {
		store := newStore(t)
		sentinel := errors.New("abort")
		err := store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
			if _, _, err := tx.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{IdempotencyKey: "k", Scope: "s", Fingerprint: Fingerprint("p")}); err != nil {
				return err
			}
			if err := tx.AppendAuditEvent(ctx, repository.AuditEvent{Actor: "alice", Action: "create", Resource: "recommendations/x", Outcome: "success", RequestID: "req-1"}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want sentinel", err)
		}
		count, _ := store.CountRecommendations(ctx)
		audit, _ := store.ListAuditEvents(ctx, 1, 10)
		if count != 0 || audit.Total != 0 {
			t.Fatalf("rollback left count=%d audit=%d", count, audit.Total)
		}
		// The rolled-back idempotency claim must not block a later request.
		if _, created, err := store.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{IdempotencyKey: "k", Scope: "s", Fingerprint: Fingerprint("other")}); err != nil || !created {
			t.Fatalf("after rollback created=%v err=%v", created, err)
		}
	})

	t.Run("transaction rolls back and re-panics", func(t *testing.T) {
		store := newStore(t)
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered %v, want boom", recovered)
			}
			if count, _ := store.CountRecommendations(ctx); count != 0 {
				t.Fatalf("panic rollback left %d records", count)
			}
		}()
		_ = store.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
			if _, _, err := tx.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{}); err != nil {
				return err
			}
			panic("boom")
		})
	})

	t.Run("transaction rejects use after return", func(t *testing.T) {
		store := newStore(t)
		var leaked repository.Transaction
		if err := store.InTransaction(ctx, func(_ context.Context, tx repository.Transaction) error {
			leaked = tx
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := leaked.CreateRecommendation(ctx, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{}); err == nil {
			t.Fatal("leaked transaction accepted a write")
		}
	})

	t.Run("canceled context is rejected", func(t *testing.T) {
		store := newStore(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := store.CreateRecommendation(canceled, Recommendation("shop", "api", "app", "low"), repository.CreateOptions{}); err == nil {
			t.Fatal("canceled create succeeded")
		}
		if err := store.InTransaction(canceled, func(context.Context, repository.Transaction) error { return nil }); err == nil {
			t.Fatal("canceled transaction succeeded")
		}
	})
}
