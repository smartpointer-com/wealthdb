package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The dimensions of benchmark_exposure. Each names the card or view that
// reads it (docs/EXPOSURE.md, "Benchmarks").
const (
	BenchmarkDimClass        = "asset_class"
	BenchmarkDimSector       = "sector"
	BenchmarkDimClassSector  = "class_sector"
	BenchmarkDimRegion       = "region"
	BenchmarkDimCountry      = "country"
	BenchmarkDimCurrency     = "currency"
	BenchmarkDimCurrencyKind = "currency_kind"
	BenchmarkDimSize         = "size"
	BenchmarkDimFundCurrency = "fund_currency"
	BenchmarkDimSecurity     = "security"
	BenchmarkDimIssuer       = "issuer"
)

// benchmarkHoldingsSQL shadows exposure_holdings for the rest of the
// transaction: one virtual holding per benchmark and fund, worth its
// weight, in an account named after the benchmark. Every exposure macro
// reads holdings through exposure_holdings, so the same walk, the same
// labels and the same fund picks describe a benchmark. Nothing but this
// connection sees the macro, as with beginPinned's.
const benchmarkHoldingsSQL = `CREATE OR REPLACE TEMP MACRO exposure_holdings(p_asof, p_ccy) AS TABLE
SELECT '` + benchmarkSource + `' AS src, benchmark AS acct, fund_ref AS position_key, fund_ref AS iid,
       'multi_asset' AS asset_class, 'etf' AS vehicle, 'USD' AS ccy, CAST(p_asof AS BIGINT) AS snap, FALSE AS is_cash,
       weight_pct AS value, CAST(NULL AS VARCHAR) AS symbol, CAST(NULL AS VARCHAR) AS name,
       exposure_key(fund_ref) AS key
  FROM benchmark_funds`

// benchmarkSource is the source the virtual holdings name. It is no
// silver source, so nothing else joins them.
const benchmarkSource = "(benchmark)"

// benchmarkExposureSQL is every benchmark's look-through at the instant
// $1, on the serving views' definitions: web_exposure's buckets, the
// debt buckets of exposure_debt_buckets, and a security and an issuer as
// the dashboard's tables name them. A leaf's value is its share of the
// benchmark in percent, the weights summing to 100. The shares are exact
// sums of the leaves (exposureSum).
//
// The place buckets call exposure_place_bucket, as
// exposure_labeled_leaves does; the size leaves out the (none) of every
// class but public equity. The security CASE mirrors
// exposure_labeled_leaves' with no arm for a holding no key names: every
// virtual holding has its fund's key. It stays written out because one
// walk, exposure_yield_leaves, serves every dimension here; reading
// exposure_labeled_leaves beside it would walk the leaves again.
var benchmarkExposureSQL = `INSERT INTO benchmark_exposure BY NAME
WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(CAST($1 AS BIGINT), 'USD')),
d AS (
    SELECT acct AS benchmark, '` + BenchmarkDimClass + `' AS dimension, asset_class AS bucket, value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimSector + `', COALESCE(sector, 'unknown'), value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimClassSector + `', asset_class || '|' || COALESCE(sector, 'unknown'), value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimRegion + `',
           exposure_place_bucket(region, asset_class), value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimCountry + `',
           exposure_place_bucket(country, asset_class), value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimCurrency + `', COALESCE(currency, 'unknown'), value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimCurrencyKind + `', COALESCE(currency, 'unknown') || '|' || currency_kind, value FROM l
    UNION ALL
    SELECT acct, '` + BenchmarkDimSize + `', COALESCE(size, 'unknown'), value FROM l WHERE asset_class = 'public_equity'
    UNION ALL
    SELECT l.acct, '` + debtDimensionCredit + `',
           exposure_credit_bucket(l.asset_class, l.debt, l.rating, b.bucket), l.value * COALESCE(b.share, 1)
      FROM l LEFT JOIN exposure_page_breakdown(CAST($1 AS BIGINT), 'credit_quality') b
           ON b.fund = CASE WHEN l.debt AND l.rating IS NULL THEN l.page_fund END
    UNION ALL
    SELECT l.acct, '` + debtDimensionMaturity + `', exposure_maturity_split(l.maturity_bucket, b.bucket),
           l.value * COALESCE(b.share, 1)
      FROM l LEFT JOIN exposure_page_breakdown(CAST($1 AS BIGINT), 'maturity_breakdown') b
           ON b.fund = CASE WHEN l.maturity_bucket = 'unknown' THEN l.page_fund END
    UNION ALL
    SELECT acct, '` + debtDimensionCoupon + `', coupon_bucket, value FROM l
    UNION ALL
    SELECT acct, '` + debtDimensionCycle + `', cycle, value FROM l),
named AS (
    SELECT acct AS benchmark, '` + BenchmarkDimSecurity + `' AS dimension,
           CASE WHEN kind IN ('remainder', 'leverage') THEN '(' || kind || ') ' || fund_ref
                WHEN asset_class = 'cash' AND NOT declared AND (key IS NULL OR key LIKE 'name:%') THEN 'cash:' || currency
                WHEN key IS NOT NULL THEN key
                ELSE 'line:' || fund_ref || ':' || COALESCE(name, asset_type_raw, '') END AS bucket,
           MAX(name) AS label, CAST(NULL AS VARCHAR) AS lei, ` + exposureSum("value") + ` AS value
      FROM l GROUP BY 1, 2, 3
    UNION ALL
    SELECT acct, '` + BenchmarkDimIssuer + `', COALESCE(issuer_key, '(none)'),
           first(issuer_name ORDER BY name_rank DESC, abs(value) DESC, issuer_name), MIN(lei), ` + exposureSum("value") + `
      FROM l GROUP BY 1, 2, 3)
SELECT CAST($2 AS BIGINT) AS computed_at, benchmark, dimension, bucket, CAST(NULL AS VARCHAR) AS label,
       CAST(NULL AS VARCHAR) AS lei, ` + exposureSum("value") + ` AS share_pct
  FROM d WHERE bucket IS NOT NULL
 GROUP BY 2, 3, 4
UNION ALL
SELECT CAST($2 AS BIGINT), benchmark, dimension, bucket, label, lei, value FROM named`

// BenchmarkKey is the row's bucket in its view's dimension of
// benchmark_exposure, as benchmarkExposureSQL writes it: a benchmark's
// weight of the row is the share stored under this key. Each exposure
// row type a benchmark compares with has one.
func (r ExposureClassRow) BenchmarkKey() string { return r.AssetClass }

// BenchmarkKey is the row's class_sector bucket.
func (r ExposureSectorRow) BenchmarkKey() string { return r.AssetClass + "|" + r.Sector }

// BenchmarkKey is the row's size bucket.
func (r ExposureSizeRow) BenchmarkKey() string { return r.Size }

// BenchmarkKey is the row's country bucket.
func (r ExposureCountryRow) BenchmarkKey() string { return r.Country }

// BenchmarkKey is the row's currency_kind bucket.
func (r ExposureCurrencyRow) BenchmarkKey() string { return r.Currency + "|" + r.Kind }

// BenchmarkKey is the row's bucket of its debt or equity dimension.
func (r ExposureBucketRow) BenchmarkKey() string { return r.Bucket }

// BenchmarkKey is the row's security bucket.
func (r ExposureSecurityRow) BenchmarkKey() string { return r.Security }

// BenchmarkKey is the row's issuer bucket: by its LEI where it has
// one, else by its name.
func (r ExposureIssuerRow) BenchmarkKey() string {
	if r.LEI != nil {
		return "lei:" + *r.LEI
	}
	return strings.ToUpper(r.Issuer)
}

// benchmarkFundCurrencySQL is each benchmark by the currency its funds
// are priced in: Allocation's reading of a currency, which looks through
// no fund. A fund with no composition counts in dollars, the currency
// N-PORT states a fund in.
var benchmarkFundCurrencySQL = `INSERT INTO benchmark_exposure BY NAME
SELECT CAST($2 AS BIGINT) AS computed_at, b.benchmark, '` + BenchmarkDimFundCurrency + `' AS dimension,
       COALESCE(f.currency, '` + benchmarkFundCurrency + `') AS bucket, ` + exposureSum("b.weight_pct") + ` AS share_pct
  FROM benchmark_funds b
  LEFT JOIN exposure_funds_at(CAST($1 AS BIGINT)) f ON f.fund_ref = exposure_key(b.fund_ref)
 GROUP BY 1, 2, 3, 4`

// materializeBenchmarkExposure rewrites benchmark_exposure: each
// benchmark's look-through at the instant at, the composition the
// exposure engine picks for its funds on that day. tx is pinned. A gold
// with no benchmark writes nothing.
func materializeBenchmarkExposure(ctx context.Context, tx *sql.Tx, at, computedAt int64) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM benchmark_exposure`); err != nil {
		return 0, fmt.Errorf("benchmark exposure: delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, benchmarkHoldingsSQL); err != nil {
		return 0, fmt.Errorf("benchmark exposure: %w", err)
	}
	n := 0
	for _, q := range []string{benchmarkExposureSQL, benchmarkFundCurrencySQL} {
		res, err := tx.ExecContext(ctx, q, at, computedAt)
		if err != nil {
			return 0, fmt.Errorf("benchmark exposure: %w", err)
		}
		k, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("benchmark exposure: %w", err)
		}
		n += int(k)
	}
	if _, err := tx.ExecContext(ctx, `DROP MACRO TABLE IF EXISTS temp.exposure_holdings`); err != nil {
		return 0, fmt.Errorf("benchmark exposure: %w", err)
	}
	return n, nil
}

// BenchmarkWeights is a benchmark's shares in a dimension, by bucket, as
// the last `web refresh` wrote them. An issuer also reads under its name
// in upper case, for a row that states no LEI.
func BenchmarkWeights(ctx context.Context, db *sql.DB, name, dimension string) (map[string]float64, error) {
	out := map[string]float64{}
	err := scanAll(ctx, db, `SELECT bucket, label, share_pct FROM benchmark_exposure WHERE benchmark = ? AND dimension = ?`,
		func(rows *sql.Rows) error {
			var bucket string
			var label sql.NullString
			var share float64
			if err := rows.Scan(&bucket, &label, &share); err != nil {
				return err
			}
			out[bucket] = share
			if dimension == BenchmarkDimIssuer && label.Valid {
				out[strings.ToUpper(label.String)] = share
			}
			return nil
		}, name, dimension)
	if err != nil {
		return nil, fmt.Errorf("benchmark weights: %w", err)
	}
	return out, nil
}
