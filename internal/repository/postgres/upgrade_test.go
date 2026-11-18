package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/repository/storetest"
)

// applyThrough brings an empty schema to version, as a release built at that
// version would have left it.
func applyThrough(t *testing.T, ctx context.Context, store *Store, version int) {
	t.Helper()
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, createMigrationTable); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:version] {
		if err := applyMigration(ctx, conn.Conn(), migration); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpgradeFromEveryEarlierSchemaKeepsData(t *testing.T) {
	ctx := context.Background()
	for from := 1; from < int(LatestSchemaVersion); from++ {
		t.Run(fmt.Sprintf("from version %d", from), func(t *testing.T) {
			store, err := Open(ctx, testDatabase(t), Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			applyThrough(t, ctx, store, from)
			if err := VerifySchema(ctx, store.Pool()); err == nil {
				t.Fatal("a partially migrated database must not be served")
			}

			// Rows written by a release at this version, using only the
			// columns that version had.
			recommendation := storetest.Recommendation("shop", "checkout", "api", "low")
			recommendation.ID = "rec_" + fmt.Sprintf("%032x", from)
			payload, err := json.Marshal(recommendation)
			if err != nil {
				t.Fatal(err)
			}
			createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			if _, err := store.Pool().Exec(ctx, `INSERT INTO recommendations
(id, cluster, namespace, workload_kind, workload, container, confidence, risk, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				recommendation.ID, recommendation.Cluster, recommendation.Namespace, recommendation.Kind, recommendation.Workload,
				recommendation.Container, recommendation.Confidence, recommendation.Risk, payload, createdAt); err != nil {
				t.Fatal(err)
			}
			eventID := "evt_" + fmt.Sprintf("%032x", from)
			if _, err := store.Pool().Exec(ctx, `INSERT INTO audit_events (id, event_timestamp, actor, action, resource, namespace, outcome, request_id)
VALUES ($1, $2, 'alice', 'create', $3, 'shop', 'success', 'request-1')`, eventID, createdAt, "recommendations/"+recommendation.ID); err != nil {
				t.Fatal(err)
			}

			if err := Migrate(ctx, store.Pool()); err != nil {
				t.Fatalf("upgrade from version %d: %v", from, err)
			}
			if err := VerifySchema(ctx, store.Pool()); err != nil {
				t.Fatalf("schema after upgrade: %v", err)
			}

			// Earlier rows read back with the defaults the later columns define.
			record, err := store.GetRecommendation(ctx, recommendation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.ReviewStatus != repository.ReviewPending || record.Source != repository.SourceRequest || !record.CreatedAt.Equal(createdAt) ||
				record.CPURequestMilli != 100 || record.Namespace != "shop" {
				t.Fatalf("recommendation after upgrade: %+v", record)
			}
			reviewed, err := store.ReviewRecommendation(ctx, recommendation.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", At: createdAt.Add(time.Hour)})
			if err != nil || reviewed.ReviewStatus != repository.ReviewApproved {
				t.Fatalf("review an upgraded recommendation: %+v err=%v", reviewed, err)
			}
			audit, err := store.ListAuditEvents(ctx, 1, 10)
			if err != nil || audit.Total != 1 || audit.Items[0].ID != eventID {
				t.Fatalf("audit history after upgrade: %+v err=%v", audit, err)
			}
			if _, err := store.Pool().Exec(ctx, "DELETE FROM audit_events"); err == nil {
				t.Fatal("upgraded audit history must be append-only")
			}
			// Tables the later migrations add are usable.
			if _, err := store.CreatePolicy(ctx, repository.PolicyMutation{Name: "upgraded", Scope: repository.PolicyScope{Level: repository.PolicyScopeNamespace, Namespace: "shop"}, Priority: 1, Enabled: true, Policy: optimizer.DefaultPolicy(), Actor: "alice"}); err != nil {
				t.Fatalf("create a policy after upgrade: %v", err)
			}
			if _, err := store.ListVerificationOutcomes(ctx, 1, 10); err != nil {
				t.Fatalf("read verification outcomes after upgrade: %v", err)
			}
			if _, known, err := store.GetResizeState(ctx, "rec_00000000000000000000000000000000"); err != nil || known {
				t.Fatalf("read resize state after upgrade: known=%v err=%v", known, err)
			}
		})
	}
}

func TestDownMigrationsRollBackToEmptyAndReapply(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, testDatabase(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := Migrate(ctx, store.Pool()); err != nil {
		t.Fatal(err)
	}
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for index := len(migrations) - 1; index >= 0; index-- {
		migration := migrations[index]
		err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.DownSQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DELETE FROM kuberight_schema_migrations WHERE version = $1", int64(migration.Version))
			return err
		})
		if err != nil {
			t.Fatalf("roll back %06d_%s: %v", migration.Version, migration.Name, err)
		}
	}
	var tables int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
WHERE table_schema = current_schema() AND table_name <> 'kuberight_schema_migrations'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("tables left after rolling back every migration: %d err=%v", tables, err)
	}
	if err := Migrate(ctx, store.Pool()); err != nil {
		t.Fatalf("reapply after a full rollback: %v", err)
	}
	if err := VerifySchema(ctx, store.Pool()); err != nil {
		t.Fatal(err)
	}
}
