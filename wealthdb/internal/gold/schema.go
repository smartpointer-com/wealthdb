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

// Migrate brings the database to this binary's schema. It applies every
// migration in migrations/ whose number is strictly greater than
// MAX(gold_schema_version) in schema_meta, then the derived set
// (derived.go) where the database is below the binary's version or
// carries another set's hash. It treats a missing schema_meta table as
// version 0, and it leaves a database above the binary's version alone.
//
// A migration file runs once, in one transaction: a failure mid-file
// rolls back. It holds tables and data moves only; macros and views are
// the derived set. Its last statement is an INSERT INTO
// schema_meta(...), so that "what's the current version" stays
// accurate.
func Migrate(ctx context.Context, db *sql.DB) error {
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}

	migrations, err := listMigrations()
	if err != nil {
		return err
	}
	set, err := derived()
	if err != nil {
		return err
	}
	want := wantVersion(migrations, set)
	if current > want {
		return nil
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %04d: %w", m.version, err)
		}
	}
	if current, err = currentSchemaVersion(ctx, db); err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}
	hash, err := derivedHash(ctx, db)
	if err != nil {
		return fmt.Errorf("read the derived hash: %w", err)
	}
	if current == want && hash == set.hash {
		return nil
	}
	if err := applyDerived(ctx, db, set, current, want); err != nil {
		return fmt.Errorf("apply the derived set %d: %w", set.version, err)
	}
	return nil
}

// MigrationsPending reports whether this binary carries a migration or a
// derived set the database has not applied. A read-only open applies
// neither, so such a database still lacks what they add.
func MigrationsPending(ctx context.Context, db *sql.DB) (bool, error) {
	current, want, err := schemaVersions(ctx, db)
	if err != nil || current != want {
		return current < want, err
	}
	set, err := derived()
	if err != nil {
		return false, err
	}
	hash, err := derivedHash(ctx, db)
	return hash != set.hash, err
}

// CheckSchemaKnown returns ErrStaleBinary, wrapped, when the database's
// schema is newer than this binary's version: a later build has written
// the database, and this one may misread it. Open runs it in every mode.
// Unlike Open's build-stamp check it needs no build stamp, so it also
// holds a binary built without VCS information, such as the engine
// image's: an older image is refused on a newer database, and so is a
// long-running server after the image is rebuilt and a load migrates
// the database under it.
func CheckSchemaKnown(ctx context.Context, db *sql.DB) error {
	current, want, err := schemaVersions(ctx, db)
	if err != nil {
		return err
	}
	if current > want {
		return fmt.Errorf("%w: the database is at schema %d, and this binary knows schema %d at most",
			ErrStaleBinary, current, want)
	}
	return nil
}

// schemaVersions is the database's schema version and this binary's: the
// newer of its newest migration and its derived set's version.
func schemaVersions(ctx context.Context, db *sql.DB) (current, want int, err error) {
	if current, err = currentSchemaVersion(ctx, db); err != nil {
		return 0, 0, err
	}
	migrations, err := listMigrations()
	if err != nil {
		return 0, 0, err
	}
	set, err := derived()
	if err != nil {
		return 0, 0, err
	}
	return current, wantVersion(migrations, set), nil
}

// currentSchemaVersion returns MAX(gold_schema_version) or 0 if
// schema_meta does not exist. Any other failure to read it is returned.
func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(gold_schema_version), 0) FROM schema_meta`).Scan(&v)
	if err != nil {
		// Only on a failure does the catalog say whether the table is
		// missing, so a database that has it pays one query.
		if ok, cerr := hasTable(ctx, db, "schema_meta"); cerr == nil && !ok {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
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
// DuckDB runs a multi-statement Exec, so the file is not split on `;`.
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
