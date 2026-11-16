package schwab

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// webReader reads from the schwab-web silver SQLite. The web
// feed complements the api feed by:
//
//   - Backfilling transactions older than each account's
//     api-coverage-start (Trader API only goes back ~2y; web
//     statements reach further).
//   - Providing per-statement-period position snapshots, their
//     open lots and cash balances that api doesn't surface at all
//     (api emits only live snapshots per dump run).
//   - Providing the realized lots of the year-end tax documents
//     (realized.go).
//   - Supplying the human-readable `nickname` Schwab attaches to
//     each account.
//
// Identity: web's `account_external_id` is the 3-to-5-digit
// account suffix Schwab renders in the UI. The orchestrator
// (merge.go) builds a suffix → api-hashValue bridge at adapter
// open time and the methods on this reader rewrite the suffix to
// the bridged hash before emitting downstream.
type webReader struct {
	db *sql.DB
	// parts is the file's optional schema (schema).
	parts silver.Lazy[webSchema]
	note  silver.SchemaNote
	file  string // the silver's base name, for the note
}

func (r *webReader) Close() error { return silver.CloseDB(&r.db) }

// webTxNot1099B is the web ledger the transaction stream reads: every
// row but the form_1099b ones (Transactions says why).
const webTxNot1099B = `transactions WHERE source <> '` + sourceFORM1099B + `'`

// clock is the web feed's. The snapshot range is the dumps', widened
// to the historical tables (historicalDates), which use as_of_date /
// period_end rather than dump times. The transaction range is the
// ledger without its form_1099b rows. The change number is the latest
// dump or transaction time, the value the window advances the watermark
// to; the historical period ends widen the snapshot range but not the
// change number, since a silver whose latest period end is past its
// latest dump and transaction would otherwise never read as up to date.
//
// A dump or a transaction past the watermark opens a window over the new
// rows, widened to the historical content so the loader's windowed
// delete covers the historical gold rows before they are re-inserted.
// The form_1099b rows reach no stream, so they widen no window.
func (r *webReader) clock() silver.Clock {
	return silver.Clock{
		Kind: "schwab-web",
		Runs: `SELECT MAX(t) FROM (SELECT MAX(snapshot_at) AS t FROM dump_runs
                    UNION ALL SELECT MAX(timestamp) FROM ` + webTxNot1099B + `)`,
		Snapshot: []string{`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs`},
		Extra:    r.historicalDates,
		Ledger:   []string{`SELECT MIN(timestamp), MAX(timestamp) FROM ` + webTxNot1099B},
		Observed: silver.Observed{Runs: silver.LatestDump},
		Mode: silver.Incremental{
			New: []string{
				`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?`,
				`SELECT MIN(timestamp), MAX(timestamp) FROM ` + webTxNot1099B + ` AND timestamp > ?`,
			},
			Extra: r.historicalDates,
		},
	}
}

// historicalDates is historicalRange where the silver carries the
// historical tables, -1 each where it does not.
func (r *webReader) historicalDates(ctx context.Context) (lo, hi int64, err error) {
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return -1, -1, err
	}
	return r.historicalRange(ctx)
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	return r.clock().Status(ctx, r.db)
}

func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return r.clock().ChangeWindow(ctx, r.db, since)
}
