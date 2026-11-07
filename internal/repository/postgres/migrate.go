package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is the session advisory lock that serializes migration
// runners from every replica sharing a database.
const migrationLockID int64 = 0x4b75626552696768

const createMigrationTable = `CREATE TABLE IF NOT EXISTS kuberight_schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL,
    checksum bytea NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT transaction_timestamp()
)`

// ErrSchemaMismatch indicates that the database schema is not the version
// this binary was built for, or that an applied migration has been altered.
var ErrSchemaMismatch = errors.New("database schema does not match this KubeRight version")

type appliedMigration struct {
	version  uint64
	name     string
	checksum []byte
}

// Migrate applies every pending embedded migration, each in its own
// transaction, while holding a database-wide advisory lock. It fails closed
// when an applied migration's checksum differs from the embedded copy or the
// database is newer than this binary.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, unlockErr := conn.Exec(unlockContext, "SELECT pg_advisory_unlock($1)", migrationLockID); unlockErr != nil {
			// A session lock that cannot be released must not return to the pool.
			_ = conn.Conn().Close(unlockContext)
		}
	}()

	if _, err := conn.Exec(ctx, createMigrationTable); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	applied, err := readAppliedMigrations(ctx, conn.Conn())
	if err != nil {
		return err
	}
	if err := verifyApplied(applied, migrations); err != nil {
		return err
	}
	for _, migration := range migrations[len(applied):] {
		if err := applyMigration(ctx, conn.Conn(), migration); err != nil {
			return err
		}
	}
	return nil
}

// VerifySchema confirms that every embedded migration has been applied with
// a matching checksum, without changing the database.
func VerifySchema(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	applied, err := readAppliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	if err := verifyApplied(applied, migrations); err != nil {
		return err
	}
	if len(applied) != len(migrations) {
		return fmt.Errorf("%w: database is at version %d, this binary requires %d; run migrations", ErrSchemaMismatch, len(applied), LatestSchemaVersion)
	}
	return nil
}

func readAppliedMigrations(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]appliedMigration, error) {
	rows, err := db.Query(ctx, "SELECT version, name, checksum FROM kuberight_schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	applied, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (appliedMigration, error) {
		var migration appliedMigration
		var version int64
		if err := row.Scan(&version, &migration.name, &migration.checksum); err != nil {
			return appliedMigration{}, err
		}
		if version < 1 {
			return appliedMigration{}, fmt.Errorf("%w: invalid applied version %d", ErrSchemaMismatch, version)
		}
		migration.version = uint64(version)
		return migration, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

func verifyApplied(applied []appliedMigration, migrations []Migration) error {
	for index, record := range applied {
		if record.version > LatestSchemaVersion {
			return fmt.Errorf("%w: database has version %d, this binary supports up to %d", ErrSchemaMismatch, record.version, LatestSchemaVersion)
		}
		if record.version != uint64(index+1) {
			return fmt.Errorf("%w: applied migrations are not contiguous at version %d", ErrSchemaMismatch, record.version)
		}
		expected := migrations[index]
		if record.name != expected.Name || !bytes.Equal(record.checksum, expected.UpChecksum[:]) {
			return fmt.Errorf("%w: applied migration %06d differs from the embedded migration", ErrSchemaMismatch, record.version)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, migration Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %06d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Exec without arguments uses the simple query protocol, which permits
	// the multiple statements contained in a migration file.
	if _, err := tx.Exec(ctx, migration.UpSQL); err != nil {
		return fmt.Errorf("apply migration %06d_%s: %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO kuberight_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", int64(migration.Version), migration.Name, migration.UpChecksum[:]); err != nil {
		return fmt.Errorf("record migration %06d: %w", migration.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %06d: %w", migration.Version, err)
	}
	return nil
}
