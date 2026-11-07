// Package postgres implements the durable repository.Store on PostgreSQL,
// including embedded, checksummed schema migrations.
package postgres

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LatestSchemaVersion is the schema version understood by this source tree.
const LatestSchemaVersion uint64 = 5

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationNamePattern = regexp.MustCompile(`^(\d{6})_([a-z][a-z0-9_]*?)\.(up|down)\.sql$`)

// Migration is a paired, transactional PostgreSQL schema change. A migration
// runner should record Version and UpChecksum after applying UpSQL in the same
// transaction. Changed checksums for an applied version must fail closed.
type Migration struct {
	Version      uint64
	Name         string
	UpSQL        string
	DownSQL      string
	UpChecksum   [sha256.Size]byte
	DownChecksum [sha256.Size]byte
}

// Migrations returns validated migrations in ascending version order.
func Migrations() ([]Migration, error) {
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		return nil, err
	}
	if len(migrations) == 0 || migrations[len(migrations)-1].Version != LatestSchemaVersion {
		return nil, fmt.Errorf("embedded migrations end at version %d, source expects version %d", len(migrations), LatestSchemaVersion)
	}
	return migrations, nil
}

type migrationPair struct {
	version uint64
	name    string
	up      string
	down    string
}

func loadMigrations(source fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	pairs := make(map[uint64]*migrationPair)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationNamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.ParseUint(matches[1], 10, 64)
		if err != nil || version == 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		pair := pairs[version]
		if pair == nil {
			pair = &migrationPair{version: version, name: matches[2]}
			pairs[version] = pair
		} else if pair.name != matches[2] {
			return nil, fmt.Errorf("migration %06d has inconsistent names %q and %q", version, pair.name, matches[2])
		}
		contents, err := fs.ReadFile(source, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		// Normalize line endings so a checkout with CRLF files produces the same
		// checksums as the Linux build that migrated the database.
		sql := strings.TrimSpace(strings.ReplaceAll(string(contents), "\r\n", "\n"))
		if sql == "" {
			return nil, fmt.Errorf("migration %q is empty", entry.Name())
		}
		if matches[3] == "up" {
			if pair.up != "" {
				return nil, fmt.Errorf("migration %06d has duplicate up scripts", version)
			}
			pair.up = sql
		} else {
			if pair.down != "" {
				return nil, fmt.Errorf("migration %06d has duplicate down scripts", version)
			}
			pair.down = sql
		}
	}

	versions := make([]uint64, 0, len(pairs))
	for version := range pairs {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left] < versions[right] })
	migrations := make([]Migration, 0, len(versions))
	for index, version := range versions {
		expected := uint64(index + 1)
		if version != expected {
			return nil, fmt.Errorf("migration sequence has version %06d, expected %06d", version, expected)
		}
		pair := pairs[version]
		if pair.up == "" || pair.down == "" {
			return nil, fmt.Errorf("migration %06d must have both up and down scripts", version)
		}
		migrations = append(migrations, Migration{
			Version:      pair.version,
			Name:         pair.name,
			UpSQL:        pair.up,
			DownSQL:      pair.down,
			UpChecksum:   sha256.Sum256([]byte(pair.up)),
			DownChecksum: sha256.Sum256([]byte(pair.down)),
		})
	}
	return migrations, nil
}
