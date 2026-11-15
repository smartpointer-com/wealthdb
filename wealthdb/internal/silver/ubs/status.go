package ubs

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// psnBusinessDayLookback is how far before the previous dump the
// window reaches for business-dated rows. A report arrives at the
// earliest the night after its business day, and up to about four
// days later over a weekend or holiday; seven days covers that with
// room to spare. A report older than this (an archive backfill after
// a long outage) needs a full `wealthdb reload`.
const psnBusinessDayLookback = int64(7 * 24 * 3600)

// psnContent are the PSN tables of business-dated content rows.
var psnContent = []string{
	"cash_accounts", "safekeeping_accounts", "portfolios", "holdings",
	"cash_balances", "instruments", "fx_rates", "forward_contracts",
}

// psnClock is the PSN feed's clock. The snapshot range spans the dumps and the
// content tables: PSN promotes snapshot_at on content rows to the
// business date's midnight, which can be earlier than the dump's
// wall-clock run time. A dump or an event past the watermark opens a
// window over the new rows. PSN delivers a report after the business day
// it covers, so a new dump's holdings and events are dated before the
// dump, and before the watermark, the previous dump's time, as well:
// the window widens to every content row and event dated after the
// watermark less psnBusinessDayLookback. The rows in that stretch that
// gold already holds are re-emitted unchanged. The change number is the
// latest dump, so an idle reload stays a no-op; a nightly report states
// the holdings and the day's events, so it is both observed watermarks.
var psnClock = func() silver.Clock {
	snapshot := []string{silver.Range("dump_runs", "snapshot_at")}
	var recent []string
	for _, t := range psnContent {
		snapshot = append(snapshot, silver.Range(t, "snapshot_at"))
		recent = append(recent, silver.RangePast(t, "snapshot_at"))
	}
	return silver.Clock{
		Kind:     "ubs",
		Runs:     silver.LatestDump,
		Snapshot: snapshot,
		Ledger:   []string{silver.Range("events", "timestamp")},
		Pin:      true,
		Mode: silver.Incremental{
			New: []string{
				silver.RangePast("dump_runs", "snapshot_at"),
				silver.RangePast("events", "timestamp"),
			},
			Recent:   append(recent, silver.RangePast("events", "timestamp")),
			Lookback: psnBusinessDayLookback,
		},
	}
}()

// Status and ChangeWindow are the clock's (silver.Clock).
func (r *psnReader) Status(ctx context.Context) (canonical.Status, error) {
	return psnClock.Status(ctx, r.db)
}

func (r *psnReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return psnClock.ChangeWindow(ctx, r.db, since)
}
