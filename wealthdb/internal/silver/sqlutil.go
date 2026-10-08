package silver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration for all adapters.

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// HasTables reports whether a silver DB has every named table.
// Adapters use it to read the tables a newer silver migration adds
// without breaking on older silvers that predate them. SQLite-only.
func HasTables(ctx context.Context, db *sql.DB, names ...string) (bool, error) {
	if len(names) == 0 {
		return true, nil
	}
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
         WHERE type = 'table' AND name IN (?`+strings.Repeat(", ?", len(names)-1)+`)`,
		args...).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("HasTables(%s): %w", strings.Join(names, ", "), err)
	}
	return n == len(names), nil
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

// CheckNumberOnOutflow returns the cheque number only when the row is money
// leaving the account. Gold's contract (migration 0075) is that the field
// names an outgoing payment, so a number on an inflow or a zero-amount row is
// dropped.
func CheckNumberOnOutflow(checkNo string, net *canonical.Decimal) *string {
	if net == nil || !net.IsNegative() {
		return nil
	}
	return StrPtrIfNonEmpty(checkNo)
}

// JoinText composes one text column out of several parts: each part is
// trimmed, empty parts are dropped, and the rest are joined with "; " —
// the transaction-text contract's separator (docs/adapters/ubs.md §7).
// Returns "" when every part is empty, so a row with no text yields no
// column rather than a stray separator; callers pair it with
// StrPtrIfNonEmpty to get nil in that case.
func JoinText(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
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

// PayloadWith returns a silver payload with `extra`'s keys merged in — an
// adapter's own annotations beside the collector's. A payload that does not
// decode as a JSON object (no loader writes one, but the column is free text)
// is replaced by the annotations alone rather than losing them.
func PayloadWith(payload string, extra map[string]any) json.RawMessage {
	if len(extra) == 0 {
		return json.RawMessage(payload)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m == nil {
		m = map[string]any{}
	}
	for k, v := range extra {
		m[k] = v
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(payload)
	}
	return blob
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

// The load clock, shared by every collector whose silver records its
// dumps in a `dump_runs` table.
//
// Those sources have no change feed of their own: a dump is a whole
// re-read of what the source will show, so the only honest trigger is
// "a dump was loaded since the last watermark", and the only honest
// response is to re-emit the full history and let gold's deleteWindow
// over [Start,End] make that idempotent. What differs between sources is
// which tables bound the span. That arrives as `spanExtrema`, a query
// yielding (MIN, MAX) over every date the source's projection touches. A
// silver whose ledger is not one `transactions` table also passes the
// query that bounds its ledger (LoadClockStatusOver). `kind` names the
// source in errors.

// LoadClockStatus reports the content ranges and pins LatestChangeNumber
// to MAX(dump_runs.snapshot_at). Each bronze dump loaded bumps it and a
// subsequent `wealthdb load` re-emits; an idle reload is a no-op.
func LoadClockStatus(ctx context.Context, db *sql.DB, kind, spanExtrema string) (canonical.Status, error) {
	return LoadClockStatusOver(ctx, db, kind, spanExtrema,
		`SELECT MIN(posted_at), MAX(posted_at) FROM transactions`)
}

// LoadClockStatusOver is LoadClockStatus with the transaction range read by
// ledgerExtrema, for a silver whose ledger is not one `transactions` table.
func LoadClockStatusOver(ctx context.Context, db *sql.DB, kind, spanExtrema, ledgerExtrema string) (canonical.Status, error) {
	s := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}

	var oldS, newS sql.NullInt64
	if err := db.QueryRowContext(ctx, spanExtrema).Scan(&oldS, &newS); err != nil {
		return s, fmt.Errorf("%s Status snapshot: %w", kind, err)
	}
	if oldS.Valid {
		s.OldestSnapshotAt = oldS.Int64
	}
	if newS.Valid {
		s.LatestSnapshotAt = newS.Int64
	}

	var oldT, newT sql.NullInt64
	if err := db.QueryRowContext(ctx, ledgerExtrema).Scan(&oldT, &newT); err != nil {
		return s, fmt.Errorf("%s Status transactions: %w", kind, err)
	}
	if oldT.Valid {
		s.OldestTransactionAt = oldT.Int64
	}
	if newT.Valid {
		s.LatestTransactionAt = newT.Int64
	}

	var latestLoad sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestLoad); err != nil {
		return s, fmt.Errorf("%s Status load: %w", kind, err)
	}
	if latestLoad.Valid {
		s.LatestChangeNumber = latestLoad.Int64
	}
	return s, nil
}

// LoadClockChangeWindow triggers on a new load — any dump_run past
// `since` — and then re-emits the FULL history: Start/End span every
// date spanExtrema covers, so gold's deleteWindow over [Start,End]
// makes the re-emit idempotent. NewChangeNumber advances to the latest
// load so an idle reload does not re-trigger.
func LoadClockChangeWindow(ctx context.Context, db *sql.DB, kind, spanExtrema string, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}

	var latestLoad sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM dump_runs`).Scan(&latestLoad); err != nil {
		return w, fmt.Errorf("%s ChangeWindow load: %w", kind, err)
	}
	if !latestLoad.Valid || latestLoad.Int64 <= since {
		return w, nil // no new load since the watermark
	}

	var start, end sql.NullInt64
	if err := db.QueryRowContext(ctx, spanExtrema).Scan(&start, &end); err != nil {
		return w, fmt.Errorf("%s ChangeWindow span: %w", kind, err)
	}
	w.NewChangeNumber = latestLoad.Int64
	if start.Valid && end.Valid {
		w.Start = start.Int64
		w.End = end.Int64
		w.HasChanges = true
	}
	return w, nil
}
