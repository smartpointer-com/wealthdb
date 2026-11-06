package gold

import (
	"context"
	"database/sql"
)

// MarkRow is one month end of `wealthdb status --marks`.
type MarkRow struct {
	MonthEnd    int64    // epoch seconds, the month's last day
	MarkedPct   *float64 // nil at a month end with no value
	ValueMarked *string  // output currency
	ValueTotal  *string
	Quality     string
}

// MarkAges reads each month end in [fromEpoch, toEpoch]: the share of value
// marked within five days, and the two values in outCcy.
func MarkAges(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string) ([]MarkRow, error) {
	return scanRows(ctx, db, "MarkAges",
		`SELECT month_end, marked_pct, CAST(value_marked AS VARCHAR), CAST(value_total AS VARCHAR), quality
		   FROM report_marks(?) WHERE month_end BETWEEN ? AND ? ORDER BY month_end`,
		[]any{outCcy, fromEpoch, toEpoch}, func(r *MarkRow) []any {
			return []any{&r.MonthEnd, flt(&r.MarkedPct), dec(&r.ValueMarked), dec(&r.ValueTotal), &r.Quality}
		})
}
