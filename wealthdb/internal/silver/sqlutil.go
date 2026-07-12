package silver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration for all adapters.

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// OpenReadOnlySQLite opens a silver SQLite file read-only with the
// query_only pragma set, pinging once to surface a missing /
// corrupt / locked file as a clear error. `label` names the source
// in error messages (e.g. "viac silver"). Every single-file SQLite
// adapter opens its backing DB through here; cointracking is the
// lone exception (DuckDB, opened with its own driver).
func OpenReadOnlySQLite(path, label string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(true)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", label, path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s %q: %w", label, path, err)
	}
	return db, nil
}

// HasColumn reports whether `table` has a column named `column`,
// via PRAGMA table_info. Adapters use it to read newer
// promoted columns from a silver DB when present without breaking
// on older silvers that predate the column. SQLite-only.
func HasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("HasColumn(%s.%s): %w", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// DecimalPtrOrNil parses a string-encoded decimal from a silver
// column. Returns nil for NULL / empty input, and nil (not an
// error) on a parse failure — adapters treat an unparseable
// numeric as "absent" rather than aborting the load.
func DecimalPtrOrNil(s sql.NullString) *canonical.Decimal {
	if !s.Valid || s.String == "" {
		return nil
	}
	d, err := canonical.NewDecimalFromString(s.String)
	if err != nil {
		return nil
	}
	return &d
}

// DecimalPtrFromNullFloat converts a nullable SQLite REAL to a
// canonical Decimal pointer: nil for SQL NULL, else the float
// reconstructed as a Decimal. Collectors round money to cents before
// storing, so the float→decimal step is exact at display precision.
func DecimalPtrFromNullFloat(n sql.NullFloat64) *canonical.Decimal {
	if !n.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(n.Float64)
	return &d
}

// DecimalOrZero parses a string-encoded decimal, returning a zero
// Decimal for NULL / empty input and a wrapped error on a genuine
// parse failure (the caller wants to know).
func DecimalOrZero(s sql.NullString) (canonical.Decimal, error) {
	if !s.Valid || s.String == "" {
		return canonical.Decimal{}, nil
	}
	return canonical.NewDecimalFromString(s.String)
}

// NullStringPtr converts a sql.NullString to a *string: nil when
// not valid, else a pointer to the value (empty string included).
func NullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// StrPtrIfNonEmpty returns nil for the empty string, else a pointer
// to s. Use for optional free-text columns where "" means absent.
func StrPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// JSONOrNil returns nil for NULL / empty input, else the raw JSON
// bytes. Used to pass a silver payload column through to a
// canonical record's Payload without re-encoding.
func JSONOrNil(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "" {
		return nil
	}
	return json.RawMessage(s.String)
}

// DatePtrFromNullUnix converts a nullable unix-seconds timestamp to a
// pointer to its UTC-midnight calendar date, or nil for SQL NULL.
// Gold stores acquisition dates as DATE, so the time-of-day is dropped.
func DatePtrFromNullUnix(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}
