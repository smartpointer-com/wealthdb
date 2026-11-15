package silver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Clock declares how a silver answers Status and ChangeWindow: which run
// is its change number, which dates bound its snapshots and its ledger,
// how far it is observed, and which window a change opens (Mode). An
// adapter's Status and ChangeWindow call the methods below. A Runs may
// name the latest row rather than a run, as the schwab and ubs web feeds'
// do. A source read through several feeds merges their clocks
// (Feeds).
//
// A range query yields one row, the (MIN, MAX) of a set of dates in unix
// seconds, NULL where the set is empty. A list of them spans the least
// MIN and the greatest MAX any of them yields. Dates read in Go come as
// Dates, -1 where there is none.
type Clock struct {
	// Kind names the source in errors.
	Kind string
	// Runs yields one row: the latest run, the source's change number.
	Runs string
	// Snapshot and Ledger are range queries: the snapshot and the
	// transaction range Status reports.
	Snapshot, Ledger []string
	// Extra and LedgerExtra are dates read in Go that the snapshot and the
	// transaction range span too.
	Extra, LedgerExtra Dates
	// Observed sets the observed watermarks.
	Observed Observed
	// Pin sets the new change number of every window to the latest run,
	// whatever the watermark. Without it the change number only advances:
	// a window past the latest run keeps the watermark it was asked from.
	Pin bool
	// Mode is the window a change opens.
	Mode Mode
}

// LatestDump is the Runs of a silver that records each dump it loaded in
// dump_runs.
const LatestDump = `SELECT MAX(snapshot_at) FROM dump_runs`

// LoadClock is the clock of a silver that records its dumps in dump_runs
// and has no change feed of its own: a loaded dump is the trigger and a
// full re-emit the response (FullSpan). snapshot is the snapshot range
// Status reports; ledger is the transaction range.
func LoadClock(kind, snapshot string, ledger ...string) Clock {
	return Clock{
		Kind: kind, Runs: LatestDump,
		Snapshot: []string{snapshot}, Ledger: ledger,
		Mode: FullSpan{},
	}
}

// Range is the range query over one column of one table. MIN and MAX
// are read apart, so an index on the column serves each.
func Range(table, column string) string {
	return fmt.Sprintf(`SELECT (SELECT MIN(%[2]s) FROM %[1]s), (SELECT MAX(%[2]s) FROM %[1]s)`, table, column)
}

// RangePast is Range over the values past the one argument it binds.
func RangePast(table, column string) string {
	return fmt.Sprintf(`SELECT (SELECT MIN(%[2]s) FROM %[1]s WHERE %[2]s > ?1), `+
		`(SELECT MAX(%[2]s) FROM %[1]s WHERE %[2]s > ?1)`, table, column)
}

// Dates reads a set of dates in Go: their earliest and latest, -1 each
// when the set is empty.
type Dates func(ctx context.Context) (lo, hi int64, err error)

// Observed is how far a silver is observed (canonical.Status). By
// default both watermarks are the latest run.
type Observed struct {
	// Runs yields one row: the latest run that read the source, in place
	// of Clock.Runs. Both watermarks are it.
	Runs string
	// Windows yields one (since, until) row per run, the ledger window
	// it fetched. The ledger is then observed through the end of the
	// first gapless stretch they cover (CoveredThrough).
	Windows string
	// Set sets both watermarks in place of Runs and Windows, for a
	// silver whose runs say per run what they read.
	Set func(ctx context.Context, s *canonical.Status) error
}

// Mode is the window a clock opens past a watermark: FullSpan,
// Incremental or PerKind.
type Mode interface {
	window(ctx context.Context, db *sql.DB, k *Clock, since int64, latest sql.NullInt64,
		w canonical.Window) (canonical.Window, error)
}

// FullSpan is the clock of a source with no change feed of its own: a
// run is a whole re-read of what the source shows, so a run past the
// watermark re-emits the whole history (canonical.FullWindow), and the
// load deletes the source's rows whole before it re-inserts them. An
// idle reload is a no-op.
type FullSpan struct{}

// Incremental is the clock of a source whose rows carry their own time:
// the window spans what lies past the watermark, widened to the rows a
// re-emit must cover, and has changes only where something lies past it.
type Incremental struct {
	// New are range queries over what lies past the watermark, each
	// binding it once.
	New []string
	// Widen are range queries the window widens to once it has changes.
	Widen []string
	// Recent are range queries the window widens to once it has
	// changes, each binding once the watermark less Lookback (a
	// watermark below zero binds as it is): rows a run stamps with a
	// date before its own.
	Recent   []string
	Lookback int64
	// Extra are dates read in Go the window widens to once it has
	// changes.
	Extra Dates
	// ReachBack moves the start of a window with changes further back.
	ReachBack func(ctx context.Context, start int64) (int64, error)
}

// PerKind is the clock of a source whose kinds of record change over
// spans of their own (canonical.Window.Spans): a run past the watermark
// re-reads each kind over the span it returns. The window spans their
// union and has changes where any span is.
type PerKind func(ctx context.Context, since int64) (map[string]canonical.Span, error)

// Status reports the ranges, the change number and the observed
// watermarks the clock declares.
func (k Clock) Status(ctx context.Context, db *sql.DB) (canonical.Status, error) {
	s := canonical.UnknownStatus()
	var err error
	if s.OldestSnapshotAt, s.LatestSnapshotAt, err = k.status(ctx, db, k.Snapshot, k.Extra, "snapshot"); err != nil {
		return s, err
	}
	if s.OldestTransactionAt, s.LatestTransactionAt, err = k.status(ctx, db, k.Ledger, k.LedgerExtra, "transactions"); err != nil {
		return s, err
	}
	latest, err := k.latest(ctx, db, "Status load")
	if err != nil {
		return s, err
	}
	if latest.Valid {
		s.LatestChangeNumber = latest.Int64
	}
	switch o := k.Observed; {
	case o.Set != nil:
		err = o.Set(ctx, &s)
	case o.Runs != "":
		err = ObserveByRuns(ctx, db, k.Kind, o.Runs, &s)
	default:
		s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough = s.LatestChangeNumber, s.LatestChangeNumber
	}
	if err == nil && k.Observed.Windows != "" {
		s.ObservedTransactionsThrough, err = CoveredByRuns(ctx, db, k.Kind, k.Observed.Windows)
	}
	return s, err
}

// status is one range of Status: the span of queries widened to extra,
// -1 each where neither holds a date.
func (k Clock) status(ctx context.Context, db *sql.DB, queries []string, extra Dates, what string) (lo, hi int64, err error) {
	lo, hi = -1, -1
	if l, h, ok, err := rangeOf(ctx, db, queries); err != nil {
		return lo, hi, fmt.Errorf("%s Status %s: %w", k.Kind, what, err)
	} else if ok {
		lo, hi = l, h
	}
	if extra == nil {
		return lo, hi, nil
	}
	l, h, err := extra(ctx)
	if err != nil {
		return lo, hi, err
	}
	if l >= 0 && (lo == -1 || l < lo) {
		lo = l
	}
	return lo, max(hi, h), nil
}

// ChangeWindow is the window past since that the clock's mode opens.
func (k Clock) ChangeWindow(ctx context.Context, db *sql.DB, since int64) (canonical.Window, error) {
	w := canonical.Window{NewChangeNumber: since}
	latest, err := k.latest(ctx, db, "ChangeWindow load")
	if err != nil {
		return w, err
	}
	if latest.Valid && (k.Pin || latest.Int64 > since) {
		w.NewChangeNumber = latest.Int64
	}
	return k.Mode.window(ctx, db, &k, since, latest, w)
}

// latest is the latest run Runs yields. what labels the error.
func (k Clock) latest(ctx context.Context, db *sql.DB, what string) (sql.NullInt64, error) {
	var t sql.NullInt64
	if err := db.QueryRowContext(ctx, k.Runs).Scan(&t); err != nil {
		return t, fmt.Errorf("%s %s: %w", k.Kind, what, err)
	}
	return t, nil
}

func (FullSpan) window(_ context.Context, _ *sql.DB, _ *Clock, since int64, latest sql.NullInt64,
	w canonical.Window) (canonical.Window, error) {
	if !latest.Valid || latest.Int64 <= since {
		return w, nil // no run past the watermark
	}
	return canonical.FullWindow(w.NewChangeNumber), nil
}

func (m Incremental) window(ctx context.Context, db *sql.DB, k *Clock, since int64, _ sql.NullInt64,
	w canonical.Window) (canonical.Window, error) {
	lo, hi, ok, err := rangeOf(ctx, db, m.New, since)
	if err != nil {
		return w, fmt.Errorf("%s ChangeWindow: %w", k.Kind, err)
	}
	if !ok {
		return w, nil // nothing past the watermark
	}
	w.Start, w.End, w.HasChanges = lo, hi, true
	floor := since
	if since >= 0 {
		floor = since - m.Lookback
	}
	for _, q := range []struct {
		queries []string
		args    []any
	}{{m.Widen, nil}, {m.Recent, []any{floor}}} {
		lo, hi, ok, err := rangeOf(ctx, db, q.queries, q.args...)
		if err != nil {
			return w, fmt.Errorf("%s ChangeWindow: %w", k.Kind, err)
		}
		if ok {
			w.Start, w.End = min(w.Start, lo), max(w.End, hi)
		}
	}
	if err := widen(ctx, &w, m.Extra); err != nil {
		return w, err
	}
	if m.ReachBack != nil {
		if w.Start, err = m.ReachBack(ctx, w.Start); err != nil {
			return w, fmt.Errorf("%s ChangeWindow: %w", k.Kind, err)
		}
	}
	return w, nil
}

func (m PerKind) window(ctx context.Context, _ *sql.DB, _ *Clock, since int64, latest sql.NullInt64,
	w canonical.Window) (canonical.Window, error) {
	if !latest.Valid || latest.Int64 <= since {
		return w, nil // no run past the watermark
	}
	spans, err := m(ctx, since)
	if err != nil {
		return w, err
	}
	w.Spans = spans
	for _, s := range spans {
		if !w.HasChanges {
			w.Start, w.End = s.Start, s.End
		}
		w.Start, w.End = min(w.Start, s.Start), max(w.End, s.End)
		w.HasChanges = true
	}
	return w, nil
}

// widen widens a window with changes to the dates extra reads.
func widen(ctx context.Context, w *canonical.Window, extra Dates) error {
	if extra == nil {
		return nil
	}
	lo, hi, err := extra(ctx)
	if err != nil {
		return err
	}
	if lo >= 0 && lo < w.Start {
		w.Start = lo
	}
	w.End = max(w.End, hi)
	return nil
}

// rangeOf is the span of range queries, each bound by args: the least
// MIN and the greatest MAX they yield, and false when every one is empty.
func rangeOf(ctx context.Context, db *sql.DB, queries []string, args ...any) (lo, hi int64, ok bool, err error) {
	for _, q := range queries {
		var a, b sql.NullInt64
		if err := db.QueryRowContext(ctx, q, args...).Scan(&a, &b); err != nil {
			return 0, 0, false, err
		}
		if !a.Valid || !b.Valid {
			continue
		}
		if !ok {
			lo, hi, ok = a.Int64, b.Int64, true
			continue
		}
		lo, hi = min(lo, a.Int64), max(hi, b.Int64)
	}
	return lo, hi, ok, nil
}
