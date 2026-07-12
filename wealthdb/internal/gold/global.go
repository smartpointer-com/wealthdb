package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// GlobalRow is the single-row, whole-portfolio rollup: it sums every
// account's output-currency aggregates and records the span of their
// snapshot dates. Computed (by the report_global macro) as Σ over
// report_accounts, so it reconciles exactly with `wealthdb accounts`.
type GlobalRow struct {
	// MinSnapshotAt / MaxSnapshotAt bound the per-account snapshot
	// dates; both 0 when no account has any data as of the requested
	// date (accounts with no observations are excluded).
	MinSnapshotAt int64
	MaxSnapshotAt int64

	// Output-currency totals across all accounts, as decimal strings.
	// Always non-nil: a sum is "0", not "unknown". An account whose
	// value can't resolve an FX path to outCcy contributes nothing.
	CashBalanceOutCcy    *string
	PositionsValueOutCcy *string
	TotalValueOutCcy     *string
}

// GlobalAsOf rolls the per-account view into one GlobalRow via the
// report_global macro (Σ over report_accounts), guaranteeing
// `global` == Σ `accounts`. See migration 0021.
func GlobalAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) (GlobalRow, error) {
	var (
		g                      GlobalRow
		cash, positions, total sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT * FROM report_global(?, ?)`, asOf, outCcy).
		Scan(&g.MinSnapshotAt, &g.MaxSnapshotAt, &cash, &positions, &total)
	if err != nil {
		return GlobalRow{}, fmt.Errorf("GlobalAsOf: %w", err)
	}
	g.CashBalanceOutCcy = trimmedDecimalPtr(cash)
	g.PositionsValueOutCcy = trimmedDecimalPtr(positions)
	g.TotalValueOutCcy = trimmedDecimalPtr(total)
	return g, nil
}
