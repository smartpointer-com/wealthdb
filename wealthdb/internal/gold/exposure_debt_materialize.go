package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The dimensions report_exposure_debt holds, one per debt and equity
// view, as exposure_debt_buckets names them: the dashboard's debt cards
// filter on them.
const (
	debtDimensionCredit   = "credit"
	debtDimensionMaturity = "maturity"
	debtDimensionCoupon   = "coupon"
	debtDimensionCycle    = "cycle"
)

// exposureDebtSQL is the instant's buckets of the four debt and equity
// views (EXPOSURE.md §7) per reporting currency, source and tax wrapper:
// each leaf in each dimension as exposure_debt_buckets gives it, valued
// in each reporting currency (scaledLeavesSQL). Each dimension sums to
// the instant's net worth.
var exposureDebtSQL = `INSERT INTO report_exposure_debt BY NAME
WITH ` + scaledLeavesSQL(`l AS (SELECT * FROM exposure_debt_buckets(CAST($1 AS BIGINT), 'USD'))`) + `
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, src AS silver_source_id,
       wrapper AS tax_wrapper, dimension, bucket, unknown, ` + exposureSum("value * k") + ` AS value
  FROM s
 GROUP BY ALL`

// exposureIssuersShown is how many issuers report_exposure_issuer_ranks
// keeps per instant and currency, beside the (none) row.
const exposureIssuersShown = 100

// exposureIssuersSQL is the instant's issuers per reporting currency as
// the report_exposure_issuers macro ranks them: the largest
// exposureIssuersShown and the (none) row; the running share counts
// every issuer. Each leaf has its issuer from exposure_yield_leaves; an
// issuer's name is picked by its leaves' values in the row's currency,
// as the macro picks it in the currency it is asked for. The sums, the
// running one and the total among them, are exact (exposureSum), so the
// ranks and shares never follow row order.
var exposureIssuersSQL = `INSERT INTO report_exposure_issuer_ranks BY NAME
WITH ` + scaledLeavesSQL(`l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(CAST($1 AS BIGINT), 'USD'))`) + `,
g AS (
    SELECT out_ccy, COALESCE(issuer_key, '(none)') AS ik,
           CASE WHEN COALESCE(issuer_key, '(none)') = '(none)' THEN '(none)'
                ELSE first(issuer_name ORDER BY name_rank DESC, abs(value * k) DESC, issuer_name) END AS issuer,
           MIN(lei) AS lei, SUM(` + exposureDecimal("value * k") + `) AS d_value,
           SUM(` + exposureDecimal("value * k") + `) FILTER (WHERE asset_class = 'public_equity') AS d_equity,
           SUM(` + exposureDecimal("value * k") + `) FILTER (WHERE debt) AS d_debt,
           SUM(` + exposureDecimal("value * k") + `) FILTER (WHERE deposit) AS d_cash
      FROM s GROUP BY out_ccy, ik),
r AS (
    SELECT g.*, CAST(g.d_value AS DOUBLE) AS value, row_number() OVER w AS rank,
           CAST(sum(g.d_value) OVER (PARTITION BY g.out_ccy) AS DOUBLE) AS total,
           CAST(sum(g.d_value) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS DOUBLE) AS running
      FROM g
    WINDOW w AS (PARTITION BY g.out_ccy ORDER BY g.ik = '(none)', CAST(g.d_value AS DECIMAL(28,4)) DESC, g.ik))
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, rank, issuer, lei, value,
       100 * value / NULLIF(total, 0) AS share_pct,
       CASE WHEN ik <> '(none)' THEN 100 * running / NULLIF(total, 0) END AS cumulative_share_pct,
       100 * COALESCE(CAST(d_equity AS DOUBLE), 0) / NULLIF(value, 0) AS equity_pct,
       100 * COALESCE(CAST(d_debt AS DOUBLE), 0) / NULLIF(value, 0) AS debt_pct,
       100 * COALESCE(CAST(d_cash AS DOUBLE), 0) / NULLIF(value, 0) AS cash_pct
  FROM r
 WHERE rank <= %d OR ik = '(none)'`

// exposureSecuritiesSQL is the instant's report_exposure_securities rows
// per reporting currency, cut by source, tax wrapper and the leaf's asset
// class: a security's slices (migration 0165). A leaf's security is the
// one exposure_labeled_leaves names, as in the view. Each label is its largest
// leaf's that has one, the greater label on a tie, with that leaf's
// absolute value as its weight, so a card that takes the label of the
// heaviest slice gives the view's.
var exposureSecuritiesSQL = `INSERT INTO report_exposure_security_slices BY NAME
WITH ` + scaledLeavesSQL(`l AS (SELECT * FROM exposure_labeled_leaves(CAST($1 AS BIGINT), 'USD'))`) + `,
g AS (SELECT s.*, abs(s.value * s.k) AS w FROM s)
SELECT CAST($2 AS BIGINT) AS computed_at, out_ccy AS currency, CAST($1 AS BIGINT) AS as_of, src AS silver_source_id,
       wrapper AS tax_wrapper, asset_class, security, MAX(name) AS name, MAX(ticker) AS ticker,
       arg_max(sector, (w, sector)) AS sector, arg_max(country, (w, country)) AS country,
       MAX(w) AS class_weight, MAX(w) FILTER (WHERE sector IS NOT NULL) AS sector_weight,
       MAX(w) FILTER (WHERE country IS NOT NULL) AS country_weight,
       MAX(market_cap) AS market_cap, MAX(size) AS size, ` + exposureSum("value * k") + ` AS value,
       COALESCE(CAST(SUM(` + exposureDecimal("value * k") + `) FILTER (WHERE direct) AS DOUBLE), 0) AS direct_value,
       COALESCE(CAST(SUM(` + exposureDecimal("value * k") + `) FILTER (WHERE NOT direct) AS DOUBLE), 0)
           AS via_funds_value,
       COALESCE(list(DISTINCT top_key ORDER BY top_key) FILTER (WHERE NOT direct), []) AS funds
  FROM g
 GROUP BY out_ccy, src, wrapper, asset_class, security`

// materializeExposureRecent fills report_exposure_debt,
// report_exposure_issuer_ranks and report_exposure_security_slices at the
// instants recent, those the yield tables hold (yieldInstants: today and
// the 12 month ends before): the walk with the debt and issuer
// dimensions costs several times the plain one, and the securities are
// many rows an instant. parts are exposure_holdings per reporting
// currency.
func materializeExposureRecent(ctx context.Context, tx *sql.Tx, recent []int64, computedAt int64, parts []string) (int, error) {
	holdings := strings.Join(parts, "\n    UNION ALL ")
	stmts := []string{fmt.Sprintf(exposureDebtSQL, holdings),
		fmt.Sprintf(exposureIssuersSQL, holdings, exposureIssuersShown),
		fmt.Sprintf(exposureSecuritiesSQL, holdings)}
	n := 0
	for _, at := range recent {
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
