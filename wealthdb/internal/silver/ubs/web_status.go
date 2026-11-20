package ubs

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// webReader reads from the ubs-web silver SQLite. The web cash pass
// applies the PSN cut (psnCut), and the statement history stops at the
// same PSN start (buildHistoricalCutoffs).
type webReader struct {
	db *sql.DB

	// valors keeps what valorIndex read, for the reason psnReader keeps
	// its cash coverage: the cash pass, the portfolio pass and the
	// realized lots each resolve against it.
	valors silver.Lazy[map[string]string]
	// rows is the ledger, read once (ledger).
	rows silver.Lazy[[]webRow]
	// printed is the printed statements of assets, read once
	// (printedPositions).
	printed silver.Lazy[[]printedPosition]

	// parts is the file's optional schema (schema).
	parts silver.Lazy[webSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
}

func (r *webReader) Close() error { return silver.CloseDB(&r.db) }

// observed sets the watermarks from the runs: each run reads the
// holdings, and the ledger over its window [window_since,
// window_until], so the ledger is observed through the end of the
// first gapless stretch of the windows. A silver from before the window
// columns counts every run as reading both through its own time.
func (r *webReader) observed(ctx context.Context, out *canonical.Status) error {
	s, err := r.schema(ctx)
	if err != nil {
		return err
	}
	windows := s.windows
	if err := silver.ObserveByRuns(ctx, r.db, "ubs-web", silver.LatestDump, out); err != nil {
		return err
	}
	if !windows {
		return nil
	}
	out.ObservedTransactionsThrough, err = silver.CoveredByRuns(ctx, r.db, "ubs-web", `
SELECT window_since, COALESCE(window_until, snapshot_at) FROM dump_runs WHERE window_since IS NOT NULL`)
	return err
}

// clock is the web feed's. Snapshot times come from dump_runs,
// widened to the historical (PDF) snapshots, which pre-date the first
// live dump; transaction times from transactions.value_date. The change
// number is the latest dump: a row can be valued after the dump that
// read it, and a value date ahead of the runs would hold the watermark
// past every later dump of both feeds. The watermarks are observed().
//
// A dump past the watermark opens a window over its own time and the
// value dates of the rows it wrote (transactions.snapshot_at), which can
// lie before the watermark. The window is widened to every date the web
// feed emits at that no dump run shares (windowDates): the historical
// snapshots, the card billing periods, the managed portfolios' trades
// and the private-markets quarterly reports. Gold's delete-then-reinsert
// would otherwise leave duplicates of them behind.
func (r *webReader) clock() silver.Clock {
	return silver.Clock{
		Kind:     "ubs-web",
		Runs:     silver.LatestDump,
		Snapshot: []string{`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs`},
		Extra:    r.historicalRange,
		Ledger:   []string{`SELECT MIN(value_date), MAX(value_date) FROM transactions`},
		Observed: silver.Observed{Set: r.observed},
		Mode: silver.Incremental{
			New: []string{
				`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?`,
				silver.RangeStampedPast("transactions", "value_date", "snapshot_at"),
			},
			Extra: r.windowDates,
		},
	}
}

// windowDates spans the dates the web feed emits at that no dump run
// shares (clock), -1 each where there is none.
func (r *webReader) windowDates(ctx context.Context) (lo, hi int64, err error) {
	lo, hi = -1, -1
	for _, extra := range []func(context.Context) (int64, int64, error){
		r.historicalRange, r.cardRange, r.portfolioTxnRange, r.reportRange,
	} {
		l, h, err := extra(ctx)
		if err != nil {
			return -1, -1, err
		}
		if l >= 0 && (lo < 0 || l < lo) {
			lo = l
		}
		hi = max(hi, h)
	}
	return lo, hi, nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	return r.clock().Status(ctx, r.db)
}

func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return r.clock().ChangeWindow(ctx, r.db, since)
}
