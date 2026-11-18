package gold

import (
	"context"
	"database/sql"
)

// ExposureBucketRow is one bucket of a debt or equity dimension after
// look-through: a credit bucket (with its grade, IG or HY), a time to
// maturity, a coupon kind or a cycle.
type ExposureBucketRow struct {
	Bucket string
	Grade  *string
	Value  *string
	ExposureShares
	// ClassSharePct is the row's share of the rows its dimension
	// describes, every row but (none): the debt and the cash in credit
	// and maturities, the debt in coupons, the public equity in cycles.
	// So those rows sum to 100. The (none) row has none, and no row has
	// one where those rows sum to zero or less.
	ClassSharePct *float64
	Quality       string
}

// exposureBuckets runs one of the bucket views.
func exposureBuckets(ctx context.Context, db *sql.DB, view string, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureRows(ctx, db, view, asOf, ccy, "",
		func(r *ExposureBucketRow) []any { return []any{&r.Bucket, str(&r.Grade)} },
		func(r *ExposureBucketRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, classShare: &r.ClassSharePct, quality: &r.Quality}
		})
}

// ExposureCredit runs report_exposure_credit.
func ExposureCredit(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "credit", asOf, ccy)
}

// ExposureMaturities runs report_exposure_maturities.
func ExposureMaturities(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "maturities", asOf, ccy)
}

// ExposureCoupons runs report_exposure_coupons.
func ExposureCoupons(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "coupons", asOf, ccy)
}

// ExposureCycles runs report_exposure_cycles.
func ExposureCycles(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "cycles", asOf, ccy)
}

// ExposureIssuerRow is one issuer after look-through: its stock, bonds
// and deposits together.
type ExposureIssuerRow struct {
	Issuer string
	LEI    *string
	Value  *string
	// SharePct is a share of net worth; CumulativeSharePct the running
	// share down to this issuer; EquityPct, DebtPct and CashPct the parts
	// of the issuer's value.
	SharePct, CumulativeSharePct, EquityPct, DebtPct, CashPct *float64
	Securities                                                int64
	Quality                                                   string
}

// ExposureIssuers runs report_exposure_issuers in the macro's ranking,
// the largest value as printed first, then by issuer: issuers of one
// printed value keep their order whatever the last bits of their sums.
// The (none) row comes last.
func ExposureIssuers(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureIssuerRow, error) {
	return scanRows(ctx, db, "ExposureIssuers", `
SELECT issuer, lei, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, cumulative_share_pct, equity_pct, debt_pct,
       cash_pct, securities, quality
  FROM report_exposure_issuers(?, ?)
 ORDER BY issuer = '(none)', CAST(value AS DECIMAL(28,4)) DESC, issuer, lei`, []any{asOf, ccy}, func(r *ExposureIssuerRow) []any {
		return []any{&r.Issuer, str(&r.LEI), dec(&r.Value), flt(&r.SharePct), flt(&r.CumulativeSharePct),
			flt(&r.EquityPct), flt(&r.DebtPct), flt(&r.CashPct), &r.Securities, &r.Quality}
	})
}
