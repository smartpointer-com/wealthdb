package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// gainsInsertSQL copies the monthly gains_windows rows of every
// currency and reading it is given, from one evaluation
// (gains_windows_all, migration 0119), into report_gains (migrations
// 0117, 0118), by column name.
const gainsInsertSQL = `INSERT INTO report_gains BY NAME
    SELECT CAST(? AS BIGINT) AS computed_at, out_currency AS currency, missing_basis,
           b_from AS period_start,
           silver_source_id, account_external_id, k, symbol, name, asset_class, vehicle,
           book_x, value_x, unrealized_start_x, unrealized_end_x, unrealized_change_x,
           realized_x, realized_short_x, realized_long_x, realized_other_x, realized_book_x,
           proceeds_x, wash_x,
           at_end, end_applies, end_without_basis, end_unpriced, basis_changed, paid_in,
           account_unobserved, onboarded_source,
           n_lots, n_without_gain, n_undated, n_sells, n_undocumented, n_in_kind, n_corporate,
           n_fx_missing, n_rebuilt, n_seed, n_implied, n_blip, n_wash, n_fee_unvalued, n_assumed_zero,
           spliced, pooled, settlement
      FROM gains_windows_all(0, CAST(? AS BIGINT), 'month', CAST(? AS VARCHAR[]), CAST(? AS VARCHAR[]))`

// gainsReadingsAgreeSQL is true when no position lacks a cost basis it
// needs and no realized lot lacks the cost its gain needs: the two
// readings of a missing cost basis then give the same rows, and the
// second is a copy of the first.
const gainsReadingsAgreeSQL = `
SELECT NOT EXISTS (SELECT 1 FROM positions WHERE basis_missing(asset_class, vehicle, book_value))
   AND NOT EXISTS (SELECT 1 FROM realized_lots_all WHERE gain_needs_cost(realized_gain_loss, proceeds, book_value))`

// MaterializeGains rewrites report_gains: for every reporting currency
// and both readings of a missing cost basis, the monthly gains_windows
// rows from the first data gold holds through toEpoch, the rows the
// Metabase Gains dashboard sums, all from one evaluation. It returns
// the number of rows written. One transaction, so a reader sees the old
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
	var agree bool
	if err := tx.QueryRowContext(ctx, gainsReadingsAgreeSQL).Scan(&agree); err != nil {
		return 0, fmt.Errorf("MaterializeGains probe: %w", err)
	}
	readings := make([]string, 0, len(lots.MissingBasisReadings))
	for _, r := range lots.MissingBasisReadings {
		if !agree || r == lots.MissingIgnore {
			readings = append(readings, string(r))
		}
	}
	res, err := tx.ExecContext(ctx, gainsInsertSQL, computedAt, toEpoch, materializeCurrencies, readings)
	if err != nil {
		return 0, fmt.Errorf("MaterializeGains: %w", err)
	}
	k, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("MaterializeGains: %w", err)
	}
	n := int(k)
	if agree {
		res, err := tx.ExecContext(ctx, `INSERT INTO report_gains BY NAME
    SELECT * REPLACE (CAST(? AS VARCHAR) AS missing_basis) FROM report_gains WHERE missing_basis = ?`,
			string(lots.MissingZero), string(lots.MissingIgnore))
		if err != nil {
			return 0, fmt.Errorf("MaterializeGains copy: %w", err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeGains copy: %w", err)
		}
		n += int(k)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeGains commit: %w", err)
	}
	return n, nil
}
