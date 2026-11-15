package fidelity

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// observed sets the watermarks from the runs and the statements. A run
// observes the holdings when it read the positions page, and the ledger
// over its activity window [activity_since, activity_until]; the ledger
// is observed through the end of the first gapless stretch of those
// windows. A statement corpus (historical_position_snapshots) observes
// both through its latest statement, its ledger over the span of its
// statements.
func (c *Connection) observed(ctx context.Context, s *canonical.Status) error {
	snap, err := silver.LatestRun(ctx, c.db, "fidelity",
		`SELECT MAX(snapshot_at) FROM dump_runs WHERE positions_present = 1`)
	if err != nil {
		return err
	}
	s.ObservedSnapshotsThrough = snap
	spans, err := silver.RunSpans(ctx, c.db, "fidelity", `
SELECT activity_since, COALESCE(activity_until, snapshot_at) FROM dump_runs
 WHERE activity_present = 1 AND activity_since IS NOT NULL`)
	if err != nil {
		return err
	}
	if c.schema.historical {
		var lo, hi sql.NullInt64
		if err := c.db.QueryRowContext(ctx,
			`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`).Scan(&lo, &hi); err != nil {
			return fmt.Errorf("fidelity Status statements: %w", err)
		}
		if hi.Valid {
			s.ObservedSnapshotsThrough = max(s.ObservedSnapshotsThrough, hi.Int64)
			spans = append(spans, silver.Span{Since: lo.Int64, Until: hi.Int64})
		}
	}
	s.ObservedTransactionsThrough = max(s.ObservedTransactionsThrough, silver.CoveredThrough(spans, silver.SpanSlack))
	return nil
}

// clock: a dump or a transaction past the watermark opens a window over
// the new rows, widened to every position snapshot, every statement
// (historicalRange) and every stated period's last day, so the snapshot
// pass drops no row. The snapshot range spans the dumps, the positions
// and the statements; the change number is the latest dump, so an idle
// reload is a no-op even with historical content. The watermarks are
// observed().
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind: "fidelity",
		Runs: silver.LatestDump,
		Snapshot: []string{
			silver.Range("dump_runs", "snapshot_at"),
			silver.Range("positions", "snapshot_at"),
		},
		Extra:    c.historicalRange,
		Ledger:   []string{silver.Range("transactions", "timestamp")},
		Observed: silver.Observed{Set: c.observed},
		Pin:      true,
		Mode: silver.Incremental{
			New: []string{
				silver.RangePast("dump_runs", "snapshot_at"),
				silver.RangePast("transactions", "timestamp"),
			},
			Widen: []string{silver.Range("positions", "snapshot_at")},
			Extra: c.windowDates,
		},
	}
}

// historicalRange is the span of the statements' as-of dates, -1 each
// where the silver holds none.
func (c *Connection) historicalRange(ctx context.Context) (lo, hi int64, err error) {
	if !c.schema.historical {
		return -1, -1, nil
	}
	return silver.Extrema(ctx, c.db, "fidelity historical",
		[]string{`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`})
}

// windowDates are the dates a window with changes widens to past the
// positions: the statements' and every stated period's last day.
func (c *Connection) windowDates(ctx context.Context) (lo, hi int64, err error) {
	if lo, hi, err = c.historicalRange(ctx); err != nil {
		return -1, -1, err
	}
	w := canonical.Window{Start: math.MaxInt64, End: hi}
	if lo >= 0 {
		w.Start = lo
	}
	if err := c.widenToStatedPeriods(ctx, &w); err != nil {
		return -1, -1, err
	}
	if w.Start == math.MaxInt64 {
		w.Start = -1
	}
	return w.Start, w.End, nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
