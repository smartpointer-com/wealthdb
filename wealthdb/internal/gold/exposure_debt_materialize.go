package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// exposureScaledSQL is each leaf of the instant $1, with its debt and
// issuer dimensions (exposure_yield_leaves), in each reporting currency
// (scaledLeavesSQL). Each statement walks it for itself, so the memory
// of one instant's walk is freed before the next.
var exposureScaledSQL = scaledLeavesSQL(`l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(CAST($1 AS BIGINT), 'USD'))`)

// The dimensions report_exposure_debt holds, one per debt and equity
// view: the dashboard's debt cards filter on them.
const (
	debtDimensionCredit   = "credit"
	debtDimensionMaturity = "maturity"
	debtDimensionCoupon   = "coupon"
	debtDimensionCycle    = "cycle"
)

// exposureDebtSQL is the instant's buckets of the four debt and equity
// views (EXPOSURE.md §7) per reporting currency, source and tax wrapper:
// a leaf's credit and maturity buckets split by its page fund's
// breakdowns as exposure_credit_leaves and exposure_maturity_leaves
// split them, its coupon kind and its cycle. Each dimension sums to the
// instant's net worth. A breakdown's join carries its condition in the
// key, so the join hashes.
var exposureDebtSQL = `INSERT INTO report_exposure_debt BY NAME
WITH ` + exposureScaledSQL + `,
c AS (
    SELECT s.out_ccy, s.src, s.wrapper, '` + debtDimensionCredit + `' AS dimension,
           exposure_credit_bucket(s.asset_class, s.debt, s.rating, b.bucket) AS bucket,
           exposure_credit_unknown(s.debt, s.rating, b.unknown) AS unknown,
           s.value * s.k * COALESCE(b.share, 1) AS value
      FROM s LEFT JOIN exposure_page_breakdown(CAST($1 AS BIGINT), 'credit_quality') b
           ON b.fund = CASE WHEN s.debt AND s.rating IS NULL THEN s.page_fund END
    UNION ALL
    SELECT s.out_ccy, s.src, s.wrapper, '` + debtDimensionMaturity + `', exposure_maturity_split(s.maturity_bucket, b.bucket), FALSE,
           s.value * s.k * COALESCE(b.share, 1)
      FROM s LEFT JOIN exposure_page_breakdown(CAST($1 AS BIGINT), 'maturity_breakdown') b
           ON b.fund = CASE WHEN s.maturity_bucket = 'unknown' THEN s.page_fund END
    UNION ALL
    SELECT s.out_ccy, s.src, s.wrapper, '` + debtDimensionCoupon + `', s.coupon_bucket, FALSE, s.value * s.k FROM s
    UNION ALL
    SELECT s.out_ccy, s.src, s.wrapper, '` + debtDimensionCycle + `', s.cycle, FALSE, s.value * s.k FROM s)
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, src AS silver_source_id,
       wrapper AS tax_wrapper, dimension, bucket, unknown, SUM(value) AS value
  FROM c
 GROUP BY ALL`

// exposureIssuersShown is how many issuers report_exposure_issuer_ranks
// keeps per instant and currency, beside the (none) row.
const exposureIssuersShown = 100

// exposureIssuersSQL is the instant's issuers per reporting currency as
// the report_exposure_issuers macro ranks them: the largest
// exposureIssuersShown and the (none) row; the running share counts
// every issuer.
var exposureIssuersSQL = `INSERT INTO report_exposure_issuer_ranks BY NAME
WITH ` + exposureScaledSQL + `,
g AS (
    SELECT out_ccy, COALESCE(issuer_key, '(none)') AS ik,
           CASE WHEN COALESCE(issuer_key, '(none)') = '(none)' THEN '(none)'
                ELSE first(issuer_name ORDER BY name_rank DESC, abs(value) DESC, issuer_name) END AS issuer,
           MIN(lei) AS lei, SUM(value * k) AS value,
           SUM(value * k) FILTER (WHERE asset_class = 'public_equity') AS equity,
           SUM(value * k) FILTER (WHERE debt) AS debt,
           SUM(value * k) FILTER (WHERE deposit) AS cash
      FROM s GROUP BY out_ccy, ik),
r AS (
    SELECT g.*, row_number() OVER w AS rank,
           100 * g.value / NULLIF(sum(g.value) OVER (PARTITION BY g.out_ccy), 0) AS share_pct,
           CASE WHEN g.ik <> '(none)' THEN 100 * sum(g.value) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
                / NULLIF(sum(g.value) OVER (PARTITION BY g.out_ccy), 0) END AS cumulative_share_pct
      FROM g
    WINDOW w AS (PARTITION BY g.out_ccy ORDER BY g.ik = '(none)', g.value DESC, g.ik))
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, rank, issuer, lei, value,
       share_pct, cumulative_share_pct,
       100 * COALESCE(equity, 0) / NULLIF(value, 0) AS equity_pct,
       100 * COALESCE(debt, 0) / NULLIF(value, 0) AS debt_pct,
       100 * COALESCE(cash, 0) / NULLIF(value, 0) AS cash_pct
  FROM r
 WHERE rank <= %d OR ik = '(none)'`

// exposureSecuritiesSQL is the instant's report_exposure_securities rows
// per reporting currency, cut by source, tax wrapper and the leaf's asset
// class: a security's slices (migration 0165). A leaf's security is the
// view's (report_exposure_securities, migration 0128): keep the CASE the
// same. Each label is its largest leaf's that has one, the greater
// label on a tie, with that leaf's absolute value as its weight, so a
// card that takes the label of the heaviest slice gives the view's.
var exposureSecuritiesSQL = `INSERT INTO report_exposure_security_slices BY NAME
WITH ` + scaledLeavesSQL(`l AS (SELECT * FROM exposure_leaves(CAST($1 AS BIGINT), 'USD'))`) + `,
g AS (
    SELECT s.*, abs(s.value * s.k) AS w,
           CASE WHEN s.kind IN ('remainder', 'leverage') THEN '(' || s.kind || ') ' || s.fund_ref
                WHEN s.asset_class = 'cash' AND NOT s.declared AND (s.key IS NULL OR s.key LIKE 'name:%%')
                     THEN 'cash:' || s.currency
                WHEN s.key IS NOT NULL THEN s.key
                WHEN s.depth > 0 THEN 'line:' || s.fund_ref || ':' || COALESCE(s.name, s.asset_type_raw, '')
                ELSE 'holding:' || s.src || ':' || COALESCE(s.iid, s.position_key) END AS security
      FROM s)
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, src AS silver_source_id,
       wrapper AS tax_wrapper, asset_class, security, MAX(name) AS name, MAX(ticker) AS ticker,
       arg_max(sector, (w, sector)) AS sector, arg_max(country, (w, country)) AS country,
       MAX(w) AS class_weight, MAX(w) FILTER (WHERE sector IS NOT NULL) AS sector_weight,
       MAX(w) FILTER (WHERE country IS NOT NULL) AS country_weight,
       MAX(market_cap) AS market_cap, MAX(size) AS size, SUM(value * k) AS value,
       COALESCE(SUM(value * k) FILTER (WHERE depth = 0), 0) AS direct_value,
       COALESCE(SUM(value * k) FILTER (WHERE depth > 0), 0) AS via_funds_value,
       COALESCE(list(DISTINCT top_key ORDER BY top_key) FILTER (WHERE depth > 0), []) AS funds
  FROM g
 GROUP BY out_ccy, src, wrapper, asset_class, security`

// materializeExposureRecent fills report_exposure_debt,
// report_exposure_issuer_ranks and report_exposure_security_slices at the
// instants the yield tables hold (yieldInstants: today and the 12 month
// ends before): the walk with the debt and issuer dimensions costs
// several times the plain one, and the securities are many rows an
// instant. parts are exposure_holdings per reporting currency.
func materializeExposureRecent(ctx context.Context, tx *sql.Tx, toEpoch, computedAt int64, parts []string) (int, error) {
	holdings := strings.Join(parts, "\n    UNION ALL ")
	stmts := []string{fmt.Sprintf(exposureDebtSQL, holdings),
		fmt.Sprintf(exposureIssuersSQL, holdings, exposureIssuersShown),
		fmt.Sprintf(exposureSecuritiesSQL, holdings)}
	n := 0
	for _, at := range yieldInstants(toEpoch) {
		for _, q := range stmts {
			res, err := tx.ExecContext(ctx, q, at, computedAt)
			if err != nil {
				return 0, fmt.Errorf("MaterializeExposure recent at %d: %w", at, err)
			}
			k, err := res.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("MaterializeExposure recent at %d: %w", at, err)
			}
			n += int(k)
		}
	}
	return n, nil
}
