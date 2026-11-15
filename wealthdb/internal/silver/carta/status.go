package carta

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// contentSpan is the MIN/MAX snapshot_at across the position-bearing content
// tables. carta reconstructs one download into event-dated deltas (an
// exercise, a later acquisition, a quarterly NAV), which sit years
// before the download. The observable snapshot range tracks THIS span, not
// dump_runs (the download time).
const contentSpan = `
SELECT MIN(snapshot_at), MAX(snapshot_at) FROM (
    SELECT snapshot_at FROM entities
    UNION ALL SELECT snapshot_at FROM securities
    UNION ALL SELECT snapshot_at FROM fund_metrics
)`

// transactionExtrema is the MIN/MAX cash-flow date — the transaction ledger
// (transactions.go). flow_date is mixed-format TEXT, so it is parsed in Go
// (flowDateUnix) rather than via strftime. A fund's calls can precede its
// first statement, so these dates can fall outside the content span: the
// transaction range Status reports.
func (c *Connection) transactionExtrema(ctx context.Context) (oldest, latest int64, ok bool, err error) {
	if err := silver.ScanRows(ctx, c.db, "carta transactionExtrema", `SELECT flow_date FROM cash_flows WHERE flow_date IS NOT NULL`, func(rows *sql.Rows) error {
		var d string
		if err := rows.Scan(&d); err != nil {
			return err
		}
		ts, parsed := flowDateUnix(d)
		if !parsed {
			return nil
		}
		if !ok || ts < oldest {
			oldest = ts
		}
		if !ok || ts > latest {
			latest = ts
		}
		ok = true
		return nil
	}); err != nil {
		return 0, 0, false, err
	}
	return oldest, latest, ok, nil
}

// clock: a new download, any dump past the watermark, re-emits the whole
// history (silver.FullSpan), so every reconstructed delta and every
// ledger row reaches gold. The snapshot range spans the
// event-dated content, the days a fund's carried value changes
// (fundcarry.go), and the removals and K-1 period ends (snapshots.go);
// the transaction range is the cash-flow ledger (transactions.go). The
// change number is the newest dump, one row per ingested bronze run, so
// an idle reload is a no-op.
func (c *Connection) clock() silver.Clock {
	return silver.Clock{
		Kind:        "carta",
		Runs:        silver.LatestDump,
		Snapshot:    []string{contentSpan},
		Extra:       c.snapshotDates,
		LedgerExtra: c.flowDates,
		Observed:    silver.Observed{Runs: `SELECT MAX(snapshot_at) FROM dump_runs WHERE dry_run = 0`},
		Pin:         true,
		Mode:        silver.FullSpan{},
	}
}

// flowDates are the cash-flow dates (transactionExtrema).
func (c *Connection) flowDates(ctx context.Context) (lo, hi int64, err error) {
	lo, hi, ok, err := c.transactionExtrema(ctx)
	if err != nil || !ok {
		return -1, -1, err
	}
	return lo, hi, nil
}

// reach is the latest removal and K-1 period end, -1 where there is none.
func (c *Connection) reach(ctx context.Context) (int64, error) {
	removals, err := c.memo.removals(ctx, c)
	if err != nil {
		return -1, err
	}
	inKind, err := c.memo.inKind(ctx, c)
	if err != nil {
		return -1, err
	}
	return inKind.Reach(removals.Reach(-1)), nil
}

// snapshotDates are the carry dates, the removals and the K-1 period ends.
func (c *Connection) snapshotDates(ctx context.Context) (lo, hi int64, err error) {
	ledger, err := c.memo.ledger(ctx, c)
	if err != nil {
		return -1, -1, err
	}
	lo, hi = -1, -1
	for _, t := range ledger.carryDates() {
		if lo == -1 || t < lo {
			lo = t
		}
		hi = max(hi, t)
	}
	r, err := c.reach(ctx)
	return lo, max(hi, r), err
}

// memo holds the reads that Status, ChangeWindow and Snapshots each need,
// read once per connection: the removals, the K-1 distributions and the
// fund ledger. The connection opens one silver read-only for one load. A
// nil field is not read yet; a failed read is read again.
type memo struct {
	removed silver.Removals
	k1      *silver.InKind
	funds   fundLedger
}

func (m *memo) removals(ctx context.Context, c *Connection) (silver.Removals, error) {
	if m.removed == nil {
		r, err := c.removals(ctx)
		if err != nil {
			return nil, err
		}
		m.removed = r
	}
	return m.removed, nil
}

func (m *memo) inKind(ctx context.Context, c *Connection) (*silver.InKind, error) {
	if m.k1 == nil {
		k, err := c.readInKind(ctx)
		if err != nil {
			return nil, err
		}
		m.k1 = k
	}
	return m.k1, nil
}

func (m *memo) ledger(ctx context.Context, c *Connection) (fundLedger, error) {
	if m.funds == nil {
		l, err := c.loadFundLedger(ctx)
		if err != nil {
			return nil, err
		}
		m.funds = l
	}
	return m.funds, nil
}

// Status and ChangeWindow are the clock's (silver.Clock).
func (c *Connection) Status(ctx context.Context) (canonical.Status, error) {
	return c.clock().Status(ctx, c.db)
}

func (c *Connection) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	return c.clock().ChangeWindow(ctx, c.db, since)
}
