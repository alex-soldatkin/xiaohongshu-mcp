package pgstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is the key for the advisory lock every migration is applied
// under. Two containers pointed at one database start at the same moment on a
// deploy; without the lock they would both see version 0 and both run the DDL.
// The value is arbitrary but must never change: it is the whole agreement.
const migrationLockID int64 = 0x7868736d6370 // "xhsmcp"

// createMigrationsTable is executed inside every migration transaction rather
// than once up front, so there is no ordering problem between creating the
// bookkeeping table and taking the lock that protects it.
const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    int PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

type migration struct {
	version int
	name    string
	body    string
}

// loadMigrations reads the embedded files and orders them by the numeric
// prefix of their name. Sorting numerically rather than by string is the point:
// a lexical sort would run 10 before 9 the day this reaches a tenth migration.
func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}

	out := make([]migration, 0, len(names))
	seen := map[int]string{}
	for _, name := range names {
		base := path.Base(name)
		prefix, _, ok := strings.Cut(strings.TrimSuffix(base, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("pgstore: migration %q is not named <version>_<description>.sql", base)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("pgstore: migration %q has no positive numeric version prefix", base)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("pgstore: migrations %q and %q share version %d", other, base, version)
		}
		seen[version] = base

		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: base, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// execer is what applying a migration needs, so the runner can be handed a
// pool or a single connection.
type execer interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Migrate applies every embedded migration that this database has not seen.
// It is safe to run concurrently from several processes and safe to run again
// on an up-to-date database, where it does nothing but read schema_migrations.
//
// There are no down migrations. Rolling back a cache schema is not a thing an
// operator needs: the data is refetchable, and the recovery for a bad
// migration is a forward one.
func Migrate(ctx context.Context, db execer) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("pgstore: migration %s: %w", m.name, err)
		}
	}
	return nil
}

// applyMigration runs one migration in its own transaction. The advisory lock
// is transaction-scoped, so it is released by the commit or the rollback and
// cannot be leaked by a crash mid-migration.
func applyMigration(ctx context.Context, db execer, m migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, createMigrationsTable); err != nil {
		return err
	}

	var applied bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version,
	).Scan(&applied); err != nil {
		return err
	}
	if applied {
		// Commit rather than roll back: the CREATE TABLE IF NOT EXISTS above
		// may have been the thing that mattered on a brand-new database.
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, m.body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// querier is the read side of a pool or a connection.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SchemaVersion reports the highest applied migration version, or 0 when the
// database has never been migrated. It exists for tests and for an operator
// answering "did this container migrate".
func SchemaVersion(ctx context.Context, q querier) (int, error) {
	// The bookkeeping table may not exist yet, and a query naming a missing
	// table fails at parse time, so its presence is checked first.
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	var version int
	if err := q.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}
