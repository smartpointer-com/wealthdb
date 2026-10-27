package gold

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every migration in migrations/ whose number is
// strictly greater than MAX(gold_schema_version) in schema_meta.
// Treats a missing schema_meta table as version 0.
//
// Each migration file is executed in its entirety inside a single
// transaction; a failure mid-file rolls back. The last statement
// of every migration must be an INSERT INTO schema_meta(...) so
// that "what's the current version" stays accurate.
//
// REPLAY. Nothing at runtime replays a migration: the go-duckdb
// driver executes each statement of a multi-statement Exec exactly
// once (verified against duckdb/duckdb-go/v2 v2.10505.0), a version
// at-or-below schema_meta is skipped above, and a file that fails
// rolls back whole. Additive migrations are nevertheless written
// replay-safe — IF NOT EXISTS on DDL, OR REPLACE on a seed or a macro
// — so the TestMigrationNNNNDDLIsRerunnable tests can re-execute a
// migration's pre-stamp body against an already-migrated database and
// pin that it is safe to.
//
// A replay can undo a later migration. A later one may delete a row an
// earlier seed inserts (0111 deletes five that 0069 seeds), and
// replaying the seed brings the row back. A rerun test of such a seed
// therefore replays the later migration after it, as Migrate would,
// before it asserts anything about the current schema. A replayed macro
// or view a later migration re-issues is back at its older body too,
// and one a later migration drops is back at all: the tests' rerun
// replays every such later migration after it.
//
// The CHECK-widening rename-swap migrations (0007-0011, 0013-0018,
// 0033-0037, 0053, 0107, 0109, 0125) are exempt and deliberately carry no rerun
// test: replaying one rebuilds its table from that migration's own
// column list, which would drop columns later migrations added, so a
// rerun test there would commit a truncated table.
func Migrate(ctx context.Context, db *sql.DB) error {
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}

	migrations, err := listMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %04d: %w", m.version, err)
		}
	}
	return nil
}

// MigrationsPending reports whether this binary carries a migration the
// database has not applied. A read-only open applies none, so such a
// database still lacks what they add.
func MigrationsPending(ctx context.Context, db *sql.DB) (bool, error) {
	current, latest, err := schemaVersions(ctx, db)
	return current < latest, err
}

// CheckSchemaKnown returns ErrStaleBinary, wrapped, when the database's
// schema is newer than the newest migration this binary carries: a later
// build has written the database, and this one may misread it. Unlike
// Open's stale-binary check it needs no build stamp, so it also holds a
// binary built without VCS information, such as the engine image's —
// which is what a long-running server needs after the image is rebuilt
// and a load migrates the database under it.
func CheckSchemaKnown(ctx context.Context, db *sql.DB) error {
	current, latest, err := schemaVersions(ctx, db)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("%w: the database is at schema %d, and this binary knows schema %d at most",
			ErrStaleBinary, current, latest)
	}
	return nil
}

// schemaVersions is the database's schema version and the newest
// migration this binary carries. A binary that carries none reports the
// database's version as its newest, so neither check above fires.
func schemaVersions(ctx context.Context, db *sql.DB) (current, latest int, err error) {
	if current, err = currentSchemaVersion(ctx, db); err != nil {
		return 0, 0, err
	}
	migrations, err := listMigrations()
	if err != nil {
		return 0, 0, err
	}
	if len(migrations) == 0 {
		return current, current, nil
	}
	return current, migrations[len(migrations)-1].version, nil
}

// currentSchemaVersion returns MAX(gold_schema_version) or 0 if
// schema_meta does not exist.
func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	row := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(gold_schema_version), 0) FROM schema_meta`)
	if err := row.Scan(&v); err != nil {
		// DuckDB raises a Catalog Error when the table is missing.
		// The message includes "Table with name schema_meta does
		// not exist"; rather than string-matching, treat any error
		// at this point as "schema_meta absent ⇒ version 0".
		return 0, nil
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

type migration struct {
	version int
	name    string
	body    string
}

func listMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseMigrationVersion(e.Name())
		if err != nil {
			return nil, fmt.Errorf("migration filename %q: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseMigrationVersion expects a filename like "0001_initial.sql"
// and returns the leading integer.
func parseMigrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) == 0 || parts[0] == "" {
		return 0, fmt.Errorf("expected NNNN_<slug>.sql prefix")
	}
	v, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("expected numeric prefix: %w", err)
	}
	return v, nil
}

// applyMigration executes one migration inside a transaction.
// DuckDB supports multi-statement Exec, so we don't have to split
// on `;` ourselves.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
