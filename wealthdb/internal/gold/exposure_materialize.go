package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// exposureInsertSQL is the look-through at one instant, its leaves
// grouped by the dashboard's dimensions and valued in every reporting
// currency. The tree is walked once, in US dollars. Each leaf is a fixed
// share of the holding it descends from, so its value in another
// currency is its dollar value times the ratio of that holding's two
// values. A holding no rate converts to US dollars is left out in every
// currency. %s is the UNION of exposure_holdings per currency. The
// instant is bound twice, once per currency, then after computed_at.
const exposureInsertSQL = `INSERT INTO report_exposure BY NAME
WITH l AS (SELECT * FROM exposure_leaves(CAST(? AS BIGINT), 'USD')),
usd AS (
    SELECT src, acct, position_key, value AS v
      FROM exposure_holdings(CAST(? AS BIGINT), 'USD') WHERE value IS NOT NULL AND value <> 0),
h AS (%s)
SELECT CAST(? AS BIGINT) AS computed_at, h.out_ccy AS currency, CAST(? AS BIGINT) AS as_of,
       l.asset_class, l.sector, l.country, l.region, l.currency AS exposure_currency, l.currency_kind,
       l.depth > 0 AS via_funds, l.how,
       SUM(l.value * h.value / usd.v) AS value
  FROM l
  JOIN usd ON usd.src = l.src AND usd.acct = l.acct AND usd.position_key = l.position_key
  JOIN h ON h.src = l.src AND h.acct = l.acct AND h.position_key = l.position_key
 GROUP BY ALL`

// exposureHoldingsSQL is one currency's holdings for exposureInsertSQL.
const exposureHoldingsSQL = `SELECT '%s' AS out_ccy, src, acct, position_key, value
      FROM exposure_holdings(CAST(? AS BIGINT), '%s') WHERE value IS NOT NULL`

// MaterializeExposure rewrites report_exposure: the look-through at the
// last second of every month from the first holding gold holds, and at
// toEpoch, in every reporting currency, grouped by the dimensions the
// Metabase Exposure dashboard charts. It returns the number of rows
// written. One transaction, so a reader sees the old table or the new
// one.
func MaterializeExposure(ctx context.Context, db *sql.DB, toEpoch, computedAt int64) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeExposure begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM report_exposure`); err != nil {
		return 0, fmt.Errorf("MaterializeExposure delete: %w", err)
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
	for _, at := range ExposureInstants(first, toEpoch) {
		args := []any{at, at}
		for range materializeCurrencies {
			args = append(args, at)
		}
		args = append(args, computedAt, at)
		res, err := tx.ExecContext(ctx, insert, args...)
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure at %d: %w", at, err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure at %d: %w", at, err)
		}
		n += int(k)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeExposure commit: %w", err)
	}
	return n, nil
}

// rowQuerier is what *sql.DB and *sql.Tx share for a one-row read.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// FirstHoldingsAt is the earliest snapshot gold holds, of a position or
// of a cash balance. ok is false when gold holds neither.
func FirstHoldingsAt(ctx context.Context, q rowQuerier) (at int64, ok bool, err error) {
	var first sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MIN(t) FROM (
	    SELECT MIN(snapshot_at) AS t FROM positions
	    UNION ALL SELECT MIN(snapshot_at) FROM cash_balances)`).Scan(&first); err != nil {
		return 0, false, fmt.Errorf("FirstHoldingsAt: %w", err)
	}
	return first.Int64, first.Valid, nil
}

// ExposureInstants are the instants the Exposure dashboard charts: the
// last second of every month from the month of from, before to, and to
// itself.
func ExposureInstants(from, to int64) []int64 {
	var out []int64
	m := time.Unix(from, 0).UTC()
	m = time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
	for {
		end := m.AddDate(0, 1, 0).Unix() - 1
		if end >= to {
			break
		}
		out = append(out, end)
		m = m.AddDate(0, 1, 0)
	}
	return append(out, to)
}
