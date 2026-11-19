package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// exposureInsertSQL is the look-through at each instant of the list $1,
// its leaves grouped by the dashboard's dimensions and valued in every
// reporting currency (scaledLeavesSQL), stamped computed_at $2. A stock's
// size is today's at every instant: securities holds one row per
// security. A leaf carries its holding's legal situs and its basis, and
// its holding's source and account's tax wrapper, which the dashboard's
// pickers narrow to. A leaf is via funds where it is not held directly
// (exposure_leaves' direct): a forward held directly holds its legs
// directly. Each value is an exact sum of its leaves (exposureSum).
//
// One statement walks many instants: the leaves and the holdings are
// read per instant (LATERAL), and DuckDB plans and runs the walk once
// for all of them. Its planning and its rate lookups, most of the cost
// of one instant's walk, are paid once.
var exposureInsertSQL = `INSERT INTO report_exposure BY NAME
WITH i AS (SELECT unnest(CAST($1 AS BIGINT[])) AS instant),
` + scaledLeavesSQL(`l AS (SELECT i.instant, x.* FROM i, LATERAL exposure_leaves(i.instant, 'USD') x)`) + `
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, instant AS as_of,
       asset_class, sector, country, region, currency AS exposure_currency, currency_kind,
       NOT direct AS via_funds, how, size, situs, situs_basis, src AS silver_source_id, wrapper AS tax_wrapper,
       ` + exposureSum("value * k") + ` AS value
  FROM s
 GROUP BY ALL`

// exposureInstantsHoldingsSQL is one currency's holdings at each instant
// of exposureInsertSQL's i, for scaledLeavesSQL.
const exposureInstantsHoldingsSQL = `SELECT '%s' AS out_ccy, i.instant, x.src, x.acct, x.position_key, x.value, x.snap
      FROM i, LATERAL exposure_holdings(i.instant, '%s') x WHERE x.value IS NOT NULL`

// exposureMonthEndsPerStatement is how many instants one exposureInsertSQL
// walks. A walk holds every instant's leaves at once. A larger batch
// pays the planning and the rate lookups fewer times, so it is faster,
// and it holds more memory at once; the bound keeps the walk beside the
// other containers of a host.
const exposureMonthEndsPerStatement = 15

// exposureSum is the exact sum of the DOUBLE expression x, as a DOUBLE:
// each term rounded to twelve decimals (exposureDecimal), the terms
// summed as decimals, the sum read back once. A float sum follows the order of its
// terms, which a parallel aggregate does not fix, so the same gold would
// write other last bits on another run.
func exposureSum(x string) string {
	return `CAST(SUM(` + exposureDecimal(x) + `) AS DOUBLE)`
}

// exposureDecimal is the DOUBLE expression x as the decimal exposureSum
// adds. Twelve decimals lie far below the four a money column shows.
func exposureDecimal(x string) string { return `CAST(` + x + ` AS DECIMAL(38,12))` }

// scaledLeavesSQL is the CTEs that value the leaves l (the look-through
// at each instant, walked once in US dollars) in every reporting
// currency: h, each holding in each currency (%s, the UNION of
// exposure_holdings per reporting currency); and s, each leaf in each
// currency with k, its holding's value in the currency over its dollar
// value, and its account's tax wrapper. Each leaf is a fixed share of the
// holding it descends from, so its value in a currency is its dollar
// value times k. A holding worth zero has no such share. Its leaves are
// zero, and left out, but for the legs of a forward held directly, which
// its mark does not scale (docs/EXPOSURE.md §5). A nonzero leaf of a
// holding worth zero takes for k the dollar's rate in the currency on
// the holding's day (the CTE zero), by the chain exposure_holdings
// values a holding by. A holding no rate converts to US dollars is left
// out in every currency. l and h name their instant in a column instant.
func scaledLeavesSQL(l string) string {
	return l + `,
h AS MATERIALIZED (%s),
usd AS (SELECT instant, src, acct, position_key, value AS v, snap FROM h WHERE out_ccy = 'USD'),
zero AS (
    SELECT z.out_ccy, z.day, COALESCE(CASE WHEN z.out_ccy = 'USD' THEN 1.0 END, o0.rate, o1.rate * o2.rate) AS k
      FROM (SELECT DISTINCT h.out_ccy, usd.snap // 86400 AS day
              FROM usd JOIN h ON h.instant = usd.instant AND h.src = usd.src AND h.acct = usd.acct
                   AND h.position_key = usd.position_key
             WHERE usd.v = 0) z
      ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = 'USD' AND o0.to_ccy = z.out_ccy AND o0.day <= z.day
      ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = 'USD' AND o1.to_ccy = 'CHF' AND o1.day <= z.day
      ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = z.out_ccy AND o2.day <= z.day),
s AS (SELECT l.*, h.out_ccy, COALESCE(h.value / NULLIF(usd.v, 0), zero.k) AS k, a.tax_wrapper AS wrapper
        FROM l
        JOIN usd ON usd.instant = l.instant AND usd.src = l.src AND usd.acct = l.acct
             AND usd.position_key = l.position_key
        JOIN h ON h.instant = l.instant AND h.src = l.src AND h.acct = l.acct AND h.position_key = l.position_key
        -- The zero test is in the key, so the join hashes.
        LEFT JOIN zero ON zero.out_ccy = h.out_ccy AND zero.day = CASE WHEN usd.v = 0 THEN usd.snap // 86400 END
        LEFT JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
       WHERE usd.v <> 0 OR (l.value <> 0 AND zero.k IS NOT NULL))`
}

// exposureHoldingsSQL is one currency's holdings at the instant $1, for
// scaledLeavesSQL.
const exposureHoldingsSQL = `SELECT '%s' AS out_ccy, CAST($1 AS BIGINT) AS instant, src, acct, position_key, value, snap
      FROM exposure_holdings(CAST($1 AS BIGINT), '%s') WHERE value IS NOT NULL`

// exposureHoldingParts is the UNION of the holdings query q
// (exposureHoldingsSQL or exposureInstantsHoldingsSQL) over every
// reporting currency.
func exposureHoldingParts(q string) string {
	parts := make([]string, len(materializeCurrencies))
	for i, c := range materializeCurrencies {
		parts[i] = fmt.Sprintf(q, c, c)
	}
	return strings.Join(parts, "\n    UNION ALL ")
}

// MaterializeExposure rewrites report_exposure: the look-through at the
// last second of every month from the first holding gold holds, and at
// p.ToEpoch, in every reporting currency, grouped by the dimensions the
// Metabase Exposure dashboard charts and the source and tax wrapper its
// pickers narrow to; and the debt, issuer and security tables at the
// recent instants (materializeExposureRecent); and at p.ToEpoch each
// benchmark's look-through (materializeBenchmarkExposure). It returns the
// number of rows written. Every month end reads the same endings and
// spans (rewriteTables).
func MaterializeExposure(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	return rewriteTables(ctx, db, "MaterializeExposure", beginPinned, exposureMaterialized, func(tx *sql.Tx) (int, error) {
		return fillExposure(ctx, tx, p.ToEpoch, p.ComputedAt)
	})
}

// exposureMaterialized are the tables MaterializeExposure rewrites.
var exposureMaterialized = []string{"report_exposure", "report_exposure_debt", "report_exposure_issuer_ranks",
	"report_exposure_security_slices"}

// fillExposure writes MaterializeExposure's rows: report_exposure at
// every month end from the first holding gold holds and at toEpoch, the
// recent tables at yieldInstants, the benchmarks at toEpoch.
func fillExposure(ctx context.Context, tx *sql.Tx, toEpoch, computedAt int64) (int, error) {
	first, ok, err := FirstHoldingsAt(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("MaterializeExposure: %w", err)
	}
	if !ok {
		return 0, nil
	}
	return materializeExposure(ctx, tx, PeriodEnds(first, toEpoch, 1), yieldInstants(toEpoch), toEpoch, computedAt)
}

// materializeExposure writes report_exposure at each of instants,
// exposureMonthEndsPerStatement at a time, the debt, issuer and security
// tables at each of recent (materializeExposureRecent), and each
// benchmark's look-through at benchmarkAt (materializeBenchmarkExposure),
// stamped computedAt. tx is pinned and the tables are empty. It returns
// the number of rows written.
func materializeExposure(ctx context.Context, tx *sql.Tx, instants, recent []int64, benchmarkAt, computedAt int64) (int, error) {
	n, err := materializeMonthEnds(ctx, tx, instants, exposureMonthEndsPerStatement, computedAt)
	if err != nil {
		return 0, err
	}
	k, err := materializeExposureRecent(ctx, tx, recent, computedAt)
	if err != nil {
		return 0, err
	}
	n += k
	// The virtual holdings shadow exposure_holdings, so they come last.
	if k, err = materializeBenchmarkExposure(ctx, tx, benchmarkAt, computedAt); err != nil {
		return 0, err
	}
	n += k
	return n, nil
}

// materializeMonthEnds writes report_exposure at each of instants, in
// statements of per instants each, stamped computedAt. It returns the
// number of rows written.
func materializeMonthEnds(ctx context.Context, tx *sql.Tx, instants []int64, per int, computedAt int64) (int, error) {
	insert := fmt.Sprintf(exposureInsertSQL, exposureHoldingParts(exposureInstantsHoldingsSQL))
	n := 0
	for chunk := range slices.Chunk(instants, per) {
		res, err := tx.ExecContext(ctx, insert, chunk, computedAt)
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure from %d to %d: %w", chunk[0], chunk[len(chunk)-1], err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeExposure from %d to %d: %w", chunk[0], chunk[len(chunk)-1], err)
		}
		n += int(k)
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
