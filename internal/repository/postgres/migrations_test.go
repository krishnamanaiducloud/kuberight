package postgres

import (
	"crypto/sha256"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedMigrationsAreCompleteAndVersioned(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != int(LatestSchemaVersion) {
		t.Fatalf("got %d migrations, latest schema version is %d", len(migrations), LatestSchemaVersion)
	}
	for index, migration := range migrations {
		wantVersion := uint64(index + 1)
		if migration.Version != wantVersion {
			t.Fatalf("migration %d has version %d", index, migration.Version)
		}
		if migration.UpChecksum != sha256.Sum256([]byte(migration.UpSQL)) {
			t.Fatalf("migration %d up checksum does not match", migration.Version)
		}
		if migration.DownChecksum != sha256.Sum256([]byte(migration.DownSQL)) {
			t.Fatalf("migration %d down checksum does not match", migration.Version)
		}
		upper := strings.ToUpper(migration.UpSQL + "\n" + migration.DownSQL)
		if strings.Contains(upper, "BEGIN;") || strings.Contains(upper, "COMMIT;") {
			t.Fatalf("migration %d controls its own transaction", migration.Version)
		}
	}
}

func TestInitialSchemaProtectsAtomicIdempotencyAndAuditIntegrity(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	up := normalizeSQL(migrations[0].UpSQL)
	for _, required := range []string{
		"create table recommendations",
		"payload jsonb not null",
		"recommendations_payload_id_matches check",
		"recommendations_payload_cluster_matches check",
		"recommendations_payload_kind_matches check",
		"create table recommendation_idempotency",
		"primary key (scope_digest, idempotency_key)",
		"check (octet_length(scope_digest) = 32)",
		"check (octet_length(request_fingerprint) = 32)",
		"foreign key (recommendation_id) references recommendations (id) on delete cascade deferrable initially deferred",
		"create table audit_events",
		"metadata jsonb not null",
		"audit_events_metadata_object check",
	} {
		if !strings.Contains(up, required) {
			t.Errorf("initial migration is missing integrity clause %q", required)
		}
	}
}

func TestInitialRollbackDropsTablesInDependencyOrder(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	down := normalizeSQL(migrations[0].DownSQL)
	audit := strings.Index(down, "drop table if exists audit_events")
	idempotency := strings.Index(down, "drop table if exists recommendation_idempotency")
	recommendations := strings.Index(down, "drop table if exists recommendations")
	if audit < 0 || idempotency < 0 || recommendations < 0 || !(audit < idempotency && idempotency < recommendations) {
		t.Fatalf("rollback is not in dependency order: %q", down)
	}
}

func TestMigrationLoaderRejectsMissingPairAndVersionGaps(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"missing down": {
			"migrations/000001_initial.up.sql": {Data: []byte("SELECT 1;")},
		},
		"version gap": {
			"migrations/000002_initial.up.sql":   {Data: []byte("SELECT 1;")},
			"migrations/000002_initial.down.sql": {Data: []byte("SELECT 1;")},
		},
		"mismatched names": {
			"migrations/000001_initial.up.sql": {Data: []byte("SELECT 1;")},
			"migrations/000001_other.down.sql": {Data: []byte("SELECT 1;")},
		},
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(source); err == nil {
				t.Fatal("loadMigrations succeeded, want validation error")
			}
		})
	}
}

func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(strings.ToLower(sql)), " ")
}

func TestMigrationChecksumsIgnoreLineEndings(t *testing.T) {
	load := func(up string) Migration {
		t.Helper()
		migrations, err := loadMigrations(fstest.MapFS{
			"migrations/000001_initial.up.sql":   {Data: []byte(up)},
			"migrations/000001_initial.down.sql": {Data: []byte("DROP TABLE t;\n")},
		})
		if err != nil {
			t.Fatal(err)
		}
		return migrations[0]
	}
	unix := load("CREATE TABLE t (\n    id text\n);\n")
	windows := load("CREATE TABLE t (\r\n    id text\r\n);\r\n")
	if unix.UpChecksum != windows.UpChecksum || unix.UpSQL != windows.UpSQL {
		t.Fatal("CRLF and LF checkouts must produce identical migrations")
	}
}
