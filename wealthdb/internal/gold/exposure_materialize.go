package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// exposureInsertSQL is the look-through at the instant $1, its leaves
// grouped by the dashboard's dimensions and valued in every reporting
// currency (scaledLeavesSQL), stamped computed_at $2. A stock's size is
// today's at every instant: securities holds one row per security. A
// leaf carries its holding's legal situs and its basis, and its
// holding's source and account's tax wrapper, which the dashboard's
// pickers narrow to.
var exposureInsertSQL = `INSERT INTO report_exposure BY NAME
WITH ` + scaledLeavesSQL(`l AS (SELECT * FROM exposure_leaves(CAST($1 AS BIGINT), 'USD'))`) + `
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of,
       asset_class, sector, country, region, currency AS exposure_currency, currency_kind,
       depth > 0 AS via_funds, how, size, situs, situs_basis, src AS silver_source_id, wrapper AS tax_wrapper,
       SUM(value * k) AS value
  FROM s
 GROUP BY ALL`

// scaledLeavesSQL is the CTEs that value the leaves l (the look-through
// at the instant $1, walked once in US dollars) in every reporting
// currency: h, each holding in each currency (%s, the UNION of
// exposure_holdings per reporting currency); and s, each leaf in each
// currency with k, its holding's value in the currency over its dollar
// value, and its account's tax wrapper. Each leaf is a fixed share of the
// holding it descends from, so its value in a currency is its dollar
// value times k. A holding no rate converts to US dollars is left out in
// every currency.
func scaledLeavesSQL(l string) string {
	return l + `,
h AS MATERIALIZED (%s),
usd AS (SELECT src, acct, position_key, value AS v FROM h WHERE out_ccy = 'USD' AND value <> 0),
s AS (SELECT l.*, h.out_ccy, h.value / usd.v AS k, a.tax_wrapper AS wrapper
        FROM l
        JOIN usd ON usd.src = l.src AND usd.acct = l.acct AND usd.position_key = l.position_key
        JOIN h ON h.src = l.src AND h.acct = l.acct AND h.position_key = l.position_key
        LEFT JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct)`
}

// exposureHoldingsSQL is one currency's holdings for scaledLeavesSQL.
const exposureHoldingsSQL = `SELECT '%s' AS out_ccy, src, acct, position_key, value
      FROM exposure_holdings(CAST($1 AS BIGINT), '%s') WHERE value IS NOT NULL`

// MaterializeExposure rewrites report_exposure: the look-through at the
// last second of every month from the first holding gold holds, and at
// toEpoch, in every reporting currency, grouped by the dimensions the
// Metabase Exposure dashboard charts and the source and tax wrapper its
// pickers narrow to; and the debt and issuer tables at the recent
// instants (materializeExposureDebt). It returns the number of rows
// written. One transaction, so a reader sees the old tables or the new
// ones.
func MaterializeExposure(ctx context.Context, db *sql.DB, toEpoch, computedAt int64) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeExposure begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	for _, t := range []string{"report_exposure", "report_exposure_debt", "report_exposure_issuer_ranks"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("MaterializeExposure delete %s: %w", t, err)
		}
	}
	first, ok, err := FirstHoldingsAt(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("MaterializeExposure: %w", err)
	}
	if !ok {
		return 0, tx.Commit()
	}
	parts := make([]string, len(materializeCurrencies))
	for i, c := range materializeCurrencies {
		parts[i] = fmt.Sprintf(exposureHoldingsSQL, c, c)
	}
	insert := fmt.Sprintf(exposureInsertSQL, strings.Join(parts, "\n    UNION ALL "))
	n := 0
	for _, at := range PeriodEnds(first, toEpoch, 1) {
		res, err := tx.ExecContext(ctx, insert, at, computedAt)
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure at %d: %w", at, err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure at %d: %w", at, err)
		}
		n += int(k)
	}
	k, err := materializeExposureDebt(ctx, tx, toEpoch, computedAt, parts)
	if err != nil {
		return 0, err
	}
	n += k
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeExposure commit: %w", err)
	}
	return n, nil
}

// FirstHoldingsAt is the earliest snapshot gold holds, of a position or
// of a cash balance. ok is false when gold holds neither.
func FirstHoldingsAt(ctx context.Context, q querier) (at int64, ok bool, err error) {
	var first sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MIN(t) FROM (
	    SELECT MIN(snapshot_at) AS t FROM positions
	    UNION ALL SELECT MIN(snapshot_at) FROM cash_balances)`).Scan(&first); err != nil {
		return 0, false, fmt.Errorf("FirstHoldingsAt: %w", err)
	}
	return first.Int64, first.Valid, nil
}
