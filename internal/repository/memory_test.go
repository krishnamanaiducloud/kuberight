package repository

import (
	"errors"
	"sync"
	"testing"

	"github.com/kuberight/kuberight/internal/optimizer"
)

func TestCreateIsIdempotent(t *testing.T) {
	memory := NewMemory(10)
	recommendation := optimizer.Recommendation{Namespace: "default", Workload: "app", Container: "app"}
	options := CreateOptions{IdempotencyKey: "request-1", Scope: "alice/default", Fingerprint: "payload-one"}
	first, created, err := memory.Create(recommendation, options)
	if err != nil || !created {
		t.Fatalf("first create failed: created=%v err=%v", created, err)
	}
	second, created, err := memory.Create(recommendation, options)
	if err != nil || created || first.ID != second.ID {
		t.Fatalf("idempotent create mismatch: first=%q second=%q created=%v err=%v", first.ID, second.ID, created, err)
	}
}

func TestCreateScopesIdempotencyKeysAndRejectsPayloadChanges(t *testing.T) {
	memory := NewMemory(10)
	recommendation := optimizer.Recommendation{Namespace: "default", Workload: "app", Container: "app"}
	first, created, err := memory.Create(recommendation, CreateOptions{IdempotencyKey: "shared", Scope: "alice/default", Fingerprint: "payload-one"})
	if err != nil || !created {
		t.Fatalf("first create failed: created=%v err=%v", created, err)
	}
	second, created, err := memory.Create(recommendation, CreateOptions{IdempotencyKey: "shared", Scope: "bob/default", Fingerprint: "payload-one"})
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("scoped create mismatch: first=%q second=%q created=%v err=%v", first.ID, second.ID, created, err)
	}
	_, _, err = memory.Create(recommendation, CreateOptions{IdempotencyKey: "shared", Scope: "alice/default", Fingerprint: "payload-two"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
}

func TestMemoryIsSafeForConcurrentWriters(t *testing.T) {
	memory := NewMemory(200)
	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, _, err := memory.Create(optimizer.Recommendation{Namespace: "default", Workload: "app", Container: "app"}, CreateOptions{}); err != nil {
				t.Errorf("create failed: %v", err)
			}
		}()
	}
	wait.Wait()
	if memory.Count() != 100 {
		t.Fatalf("got %d records, want 100", memory.Count())
	}
}

func TestMemoryEvictsOldestBoundedRecord(t *testing.T) {
	memory := NewMemory(2)
	first, _, _ := memory.Create(optimizer.Recommendation{Namespace: "one"}, CreateOptions{IdempotencyKey: "one", Scope: "test", Fingerprint: "one"})
	_, _, _ = memory.Create(optimizer.Recommendation{Namespace: "two"}, CreateOptions{IdempotencyKey: "two", Scope: "test", Fingerprint: "two"})
	_, _, _ = memory.Create(optimizer.Recommendation{Namespace: "three"}, CreateOptions{IdempotencyKey: "three", Scope: "test", Fingerprint: "three"})
	if _, ok := memory.Get(first.ID); ok {
		t.Fatal("oldest record was not evicted")
	}
	if memory.Count() != 2 {
		t.Fatalf("got %d records, want 2", memory.Count())
	}
}

func TestListSortsRiskBySeverity(t *testing.T) {
	memory := NewMemory(10)
	for _, risk := range []string{"high", "low", "medium"} {
		_, _, err := memory.Create(optimizer.Recommendation{Namespace: "default", Workload: risk, Risk: risk}, CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	result := memory.List(ListOptions{Page: 1, PageSize: 10, Sort: "risk", Order: "asc"})
	if len(result.Items) != 3 || result.Items[0].Risk != "low" || result.Items[1].Risk != "medium" || result.Items[2].Risk != "high" {
		t.Fatalf("unexpected risk order: %#v", result.Items)
	}
}
