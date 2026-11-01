package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/repository/storetest"
)

// testDatabase returns a connection string bound to a fresh, empty schema.
// Integration tests are skipped unless KUBERIGHT_TEST_DATABASE_URL is set.
func testDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("KUBERIGHT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("KUBERIGHT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "kuberight_test_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupContext, base)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(cleanupContext)
		if _, err := conn.Exec(cleanupContext, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("KUBERIGHT_TEST_DATABASE_URL must be a URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func openMigrated(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testDatabase(t), Options{MaxConns: 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := Migrate(ctx, store.Pool()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPostgresSatisfiesStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) repository.Store { return openMigrated(t) })
}

func TestMigrateIsIdempotentAndReady(t *testing.T) {
	store := openMigrated(t)
	ctx := context.Background()
	if err := Migrate(ctx, store.Pool()); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready() = %v", err)
	}
	var version int64
	if err := store.Pool().QueryRow(ctx, "SELECT max(version) FROM kuberight_schema_migrations").Scan(&version); err != nil || uint64(version) != LatestSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestReadyFailsBeforeMigration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDatabase(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Ready(ctx); err == nil {
		t.Fatal("Ready() succeeded on an unmigrated schema")
	}
}

func TestMigrateRejectsAlteredMigration(t *testing.T) {
	store := openMigrated(t)
	ctx := context.Background()
	if _, err := store.Pool().Exec(ctx, "UPDATE kuberight_schema_migrations SET checksum = '\\x00' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, store.Pool()); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("got %v, want ErrSchemaMismatch", err)
	}
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Ready() = %v, want ErrSchemaMismatch", err)
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	store := openMigrated(t)
	ctx := context.Background()
	if _, err := store.Pool().Exec(ctx, "INSERT INTO kuberight_schema_migrations (version, name, checksum) VALUES ($1, 'future', '\\x00')", int64(LatestSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, store.Pool()); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("got %v, want ErrSchemaMismatch", err)
	}
}

func TestSchemaRejectsIncompleteReview(t *testing.T) {
	store := openMigrated(t)
	ctx := context.Background()
	record, _, err := store.CreateRecommendation(ctx, storetest.Recommendation("shop", "api", "app", "low"), repository.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE recommendations SET review_status = 'approved' WHERE id = $1", record.ID); err == nil {
		t.Fatal("schema accepted an approval without a reviewer")
	}
}

func TestOpenRejectsInvalidConnectionString(t *testing.T) {
	if _, err := Open(context.Background(), "postgres://%zz", Options{}); err == nil {
		t.Fatal("Open() accepted an invalid connection string")
	}
}
