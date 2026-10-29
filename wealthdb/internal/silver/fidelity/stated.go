package fidelity

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Stated periods: an account's summary of one period as a document
// states it (fidelity-web silver migration 0015). The svb collector
// fills the table from the month-end workbook; a silver without it, or
// with no row in the window, states none. The rows go to gold's
// stated_periods as they are: informative, never a position or a flow.

// statedPeriodsTable is the silver table's name.
const statedPeriodsTable = "stated_periods"

// statedPeriods reads the window's stated periods, in (period_end,
// account, document) order. period_end is the period's last day, which
// gold keys the row by.
func (c *Connection) statedPeriods(ctx context.Context, w canonical.Window) ([]canonical.StatedPeriodChange, error) {
	ok, err := silver.HasTables(ctx, c.db, statedPeriodsTable)
	if err != nil || !ok {
		return nil, err
	}
	var out []canonical.StatedPeriodChange
	err = silver.ScanRows(ctx, c.db, `
SELECT period_end, period_start, account_external_id, currency,
       CAST(beginning_value AS VARCHAR), CAST(ending_value AS VARCHAR),
       CAST(accrual AS VARCHAR), CAST(net_additions AS VARCHAR),
       CAST(net_gain AS VARCHAR), CAST(income AS VARCHAR), CAST(fees AS VARCHAR),
       source_sha256, payload
  FROM `+statedPeriodsTable+`
 WHERE period_end BETWEEN ? AND ?
 ORDER BY period_end, account_external_id, source_sha256`, func(rows *sql.Rows) error {
		var (
			r                                         canonical.StatedPeriodChange
			begin, end, accrual, additions, gain, inc sql.NullString
			fees, payload                             sql.NullString
		)
		if err := rows.Scan(&r.AsOf, &r.PeriodStart, &r.AccountExternalID, &r.Currency,
			&begin, &end, &accrual, &additions, &gain, &inc, &fees,
			&r.SourceSHA256, &payload); err != nil {
			return err
		}
		r.BeginningValue, r.EndingValue = silver.DecimalPtrOrNil(begin), silver.DecimalPtrOrNil(end)
		r.Accrual, r.NetAdditions = silver.DecimalPtrOrNil(accrual), silver.DecimalPtrOrNil(additions)
		r.NetGain, r.Income = silver.DecimalPtrOrNil(gain), silver.DecimalPtrOrNil(inc)
		r.Fees = silver.DecimalPtrOrNil(fees)
		r.Payload = silver.JSONOrNil(payload)
		out = append(out, r)
		return nil
	}, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("fidelity stated periods: %w", err)
	}
	return out, nil
}

// widenToStatedPeriods widens a window with changes to every stated
// period's last day, so a document's periods before or after the
// statements' span still load.
func (c *Connection) widenToStatedPeriods(ctx context.Context, w *canonical.Window) error {
	ok, err := silver.HasTables(ctx, c.db, statedPeriodsTable)
	if err != nil || !ok {
		return err
	}
	lo, hi, err := silver.Extrema(ctx, c.db, "fidelity ChangeWindow stated periods",
		[]string{`SELECT MIN(period_end), MAX(period_end) FROM ` + statedPeriodsTable})
	if err != nil {
		return err
	}
	if lo >= 0 && lo < w.Start {
		w.Start = lo
	}
	if hi > w.End {
		w.End = hi
	}
	return nil
}
