package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// gainsInsertSQL copies one currency's monthly gains_windows rows into
// report_gains (migration 0117), by column name.
const gainsInsertSQL = `INSERT INTO report_gains BY NAME
    SELECT CAST(? AS BIGINT) AS computed_at, CAST(? AS VARCHAR) AS currency, b_from AS period_start,
           silver_source_id, account_external_id, k, symbol, name, asset_class, vehicle,
           book_x, value_x, unrealized_start_x, unrealized_end_x, unrealized_change_x,
           realized_x, realized_short_x, realized_long_x, realized_other_x, realized_book_x,
           proceeds_x, wash_x,
           at_end, end_applies, end_without_basis, end_unpriced, basis_changed, paid_in,
           account_unobserved, onboarded_source,
           n_lots, n_without_gain, n_undated, n_sells, n_undocumented, n_in_kind, n_corporate,
           n_fx_missing
      FROM gains_windows(0, CAST(? AS BIGINT), CAST(? AS VARCHAR), 'month')`

// MaterializeGains rewrites report_gains: for every reporting currency,
// the monthly gains_windows rows from the first data gold holds through
// toEpoch, the rows the Metabase Gains dashboard sums. It returns the
// number of rows written. One transaction, so a reader sees the old
// table or the new one.
func MaterializeGains(ctx context.Context, db *sql.DB, toEpoch, computedAt int64) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeGains begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM report_gains`); err != nil {
		return 0, fmt.Errorf("MaterializeGains delete: %w", err)
	}
	n := 0
	for _, ccy := range materializeCurrencies {
		res, err := tx.ExecContext(ctx, gainsInsertSQL, computedAt, ccy, toEpoch, ccy)
		if err != nil {
			return 0, fmt.Errorf("MaterializeGains %s: %w", ccy, err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeGains %s: %w", ccy, err)
		}
		n += int(k)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeGains commit: %w", err)
	}
	return n, nil
}
