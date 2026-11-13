package gold

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The derived set is every macro and view of the gold schema. It is a
// function of the tables, so it lives in derived/ as one file per family,
// each object once, and not in the migrations. derived/README.md states
// the contract.
//
//go:embed derived/*.sql derived/LOCK
var derivedFS embed.FS

// derivedSet is the derived set this binary carries.
type derivedSet struct {
	version int    // the LOCK's last version
	hash    string // derivedSetHash of stmts and serving
	stmts   []derivedStmt
	serving []servingTable
}

// derivedStmt is one CREATE MACRO or CREATE VIEW of a derived file.
type derivedStmt struct {
	file, name, sql string
}

// derived reads the embedded set once per process.
var derived = sync.OnceValues(func() (derivedSet, error) { return readDerived(derivedFS) })

// derivedHeadRE matches the head of a derived statement and names its
// object.
var derivedHeadRE = regexp.MustCompile(`(?i)^\s*CREATE\s+(?:MACRO|VIEW)\s+(\w+)`)

// readDerived reads the derived files of fsys in name order and the last
// line of its LOCK.
func readDerived(fsys fs.FS) (derivedSet, error) {
	var set derivedSet
	names, err := fs.Glob(fsys, "derived/*.sql")
	if err != nil {
		return set, err
	}
	for _, name := range names { // fs.Glob returns names in order
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return set, err
		}
		stmts, err := splitDerived(name, string(body))
		if err != nil {
			return set, err
		}
		set.stmts = append(set.stmts, stmts...)
	}
	lock, err := readLock(fsys)
	if err != nil {
		return set, err
	}
	set.version = lock[len(lock)-1].version
	set.serving = servingTables
	set.hash = derivedSetHash(set.stmts, set.serving)
	return set, nil
}

// derivedSetHash is the sha256 of what the apply gives a database: each
// statement's code, its whole-line comments left out, in order, and then
// each serving table with its source. DuckDB keeps no comment, so a
// comment edit keeps the hash; a serving entry changes it as a body edit
// does.
func derivedSetHash(stmts []derivedStmt, serving []servingTable) string {
	h := sha256.New()
	for _, s := range stmts {
		h.Write([]byte(stripComments(s.sql)))
	}
	for _, st := range serving {
		h.Write([]byte(st.table + "\x00" + st.source + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lockLine is one line of derived/LOCK: a derived version and the set's
// hash (derivedSetHash) at that version.
type lockLine struct {
	version int
	hash    string
}

// readLock reads derived/LOCK: one "<version> <hash>" line per derived
// version, the versions rising.
func readLock(fsys fs.FS) ([]lockLine, error) {
	body, err := fs.ReadFile(fsys, "derived/LOCK")
	if err != nil {
		return nil, err
	}
	var out []lockLine
	for i, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || len(f[1]) != sha256.Size*2 {
			return nil, fmt.Errorf("derived/LOCK line %d: want <version> <sha256>, got %q", i+1, line)
		}
		v, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("derived/LOCK line %d: %w", i+1, err)
		}
		if len(out) > 0 && v <= out[len(out)-1].version {
			return nil, fmt.Errorf("derived/LOCK line %d: version %d does not rise", i+1, v)
		}
		out = append(out, lockLine{v, f[1]})
	}
	return out, nil
}

// splitDerived splits a derived file into its statements. A statement
// ends at a line that ends with ';' and is no comment, and it is one
// CREATE MACRO or CREATE VIEW: the comment lines before it are its own.
func splitDerived(file, body string) ([]derivedStmt, error) {
	var out []derivedStmt
	var cur strings.Builder
	code := false
	for _, line := range strings.SplitAfter(body, "\n") {
		cur.WriteString(line)
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		code = true
		if !strings.HasSuffix(t, ";") {
			continue
		}
		stmt := cur.String()
		m := derivedHeadRE.FindStringSubmatch(stripComments(stmt))
		if m == nil {
			return nil, fmt.Errorf("%s: statement %d is not a CREATE MACRO or CREATE VIEW", file, len(out)+1)
		}
		out = append(out, derivedStmt{file: file, name: strings.ToLower(m[1]), sql: stmt})
		cur.Reset()
		code = false
	}
	if code {
		return nil, fmt.Errorf("%s: the last statement has no ';'", file)
	}
	return out, nil
}

// stripComments drops the whole-line comments of sql.
func stripComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// wantVersion is the schema version ms and set bring a database to: the
// newest migration, or the derived version where that is newer.
func wantVersion(ms []migration, set derivedSet) int {
	return max(ms[len(ms)-1].version, set.version)
}

// derivedHash is the hash of the derived set the database carries, ""
// where none was applied.
func derivedHash(ctx context.Context, q querier) (string, error) {
	var h sql.NullString
	err := q.QueryRowContext(ctx, `SELECT max(hash) FROM derived_state`).Scan(&h)
	return h.String, err
}

// applyDerived replaces every macro and view of the database with set,
// in one transaction: it drops them all, runs the files in order and
// records the set's hash, and it stamps want where the database is below
// it. Right after the statement that creates a serving table's macro, it
// gives the table that macro's columns (reshapeServing), so a view below
// reads the table in its new shape. On an error it rolls back, and the
// set the database had stays whole.
func applyDerived(ctx context.Context, db *sql.DB, set derivedSet, current, want int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after commit
	drops, err := derivedDrops(ctx, tx)
	if err != nil {
		return fmt.Errorf("list derived objects: %w", err)
	}
	for _, d := range drops {
		if _, err := tx.ExecContext(ctx, d); err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
	}
	for _, s := range set.stmts {
		if _, err := tx.ExecContext(ctx, s.sql); err != nil {
			return fmt.Errorf("%s: %s: %w", s.file, s.name, err)
		}
		for _, st := range set.serving {
			if st.macro() != s.name {
				continue
			}
			if err := reshapeServing(ctx, tx, st); err != nil {
				return fmt.Errorf("%s: %w", st.table, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM derived_state; INSERT INTO derived_state VALUES (?)`, set.hash); err != nil {
		return fmt.Errorf("record the derived hash: %w", err)
	}
	if current < want {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_meta (gold_schema_version, applied_at)
		    VALUES (?, CAST(epoch(now()) AS BIGINT))`, want); err != nil {
			return fmt.Errorf("stamp schema %d: %w", want, err)
		}
	}
	return tx.Commit()
}

// derivedDrops are the statements that drop every macro and view of the
// database: those of its own catalog's main schema that DuckDB does not
// define itself. A temporary macro lives in another catalog and stays.
func derivedDrops(ctx context.Context, tx *sql.Tx) ([]string, error) {
	var out []string
	err := scanAll(ctx, tx, `
        SELECT 'DROP VIEW "' || view_name || '"'
          FROM duckdb_views()
         WHERE database_name = current_database() AND schema_name = 'main' AND NOT internal
        UNION ALL
        SELECT DISTINCT CASE function_type WHEN 'table_macro' THEN 'DROP MACRO TABLE "' ELSE 'DROP MACRO "' END
                        || function_name || '"'
          FROM duckdb_functions()
         WHERE database_name = current_database() AND schema_name = 'main' AND NOT internal
           AND function_type IN ('macro', 'table_macro')`,
		func(rows *sql.Rows) error {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
			return nil
		})
	return out, err
}

// reshapeServing re-creates st's table, empty, with the columns of its
// macro, where the table is absent or its columns differ. LIMIT 0 takes
// the columns and computes nothing. The rows dropped are a cache: the
// materialize key holds the engine's hash, so the next refresh refills
// them.
func reshapeServing(ctx context.Context, tx *sql.Tx, st servingTable) error {
	want, err := columnsOf(ctx, tx, `SELECT * FROM `+st.source)
	if err != nil {
		return err
	}
	cols, err := tableColumns(ctx, tx, st.table)
	if err != nil {
		return err
	}
	have := make([]string, len(cols))
	for i, c := range cols {
		have[i] = c.name + " " + c.typ
	}
	if len(have) > 0 && slices.Equal(have, want) {
		return nil
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+st.table+`;
	    CREATE TABLE `+st.table+` AS SELECT * FROM `+st.source+` LIMIT 0`)
	return err
}

// columnsOf is the name and type of each column query returns, in order.
// DESCRIBE binds the query and runs nothing.
func columnsOf(ctx context.Context, q querier, query string) ([]string, error) {
	var out []string
	err := scanAll(ctx, q, `SELECT column_name || ' ' || column_type FROM (DESCRIBE `+query+`)`,
		func(rows *sql.Rows) error {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			out = append(out, c)
			return nil
		})
	return out, err
}
