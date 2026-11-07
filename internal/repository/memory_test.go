package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/repository/storetest"
)

func TestMemorySatisfiesStoreContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) repository.Store { return repository.NewMemory(100) })
}

func TestMemoryEvictsOldestBoundedRecord(t *testing.T) {
	ctx := context.Background()
	memory := repository.NewMemory(2)
	create := func(namespace, key string) repository.RecommendationRecord {
		record, _, err := memory.CreateRecommendation(ctx, storetest.Recommendation(namespace, "app", "app", "low"), repository.CreateOptions{IdempotencyKey: key, Scope: "test", Fingerprint: storetest.Fingerprint(key)})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := create("one", "one")
	create("two", "two")
	create("three", "three")
	if _, err := memory.GetRecommendation(ctx, first.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("oldest record was not evicted: %v", err)
	}
	if count, _ := memory.CountRecommendations(ctx); count != 2 {
		t.Fatalf("got %d records, want 2", count)
	}
	// The evicted record's idempotency claim is released with it.
	if _, created, err := memory.CreateRecommendation(ctx, storetest.Recommendation("one", "app", "app", "low"), repository.CreateOptions{IdempotencyKey: "one", Scope: "test", Fingerprint: storetest.Fingerprint("changed")}); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}
