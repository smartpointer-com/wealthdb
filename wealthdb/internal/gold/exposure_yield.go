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
	Quality string
}

// exposureBuckets runs one of the bucket views; grade is the column that
// holds a credit bucket's grade, NULL for the views that have none.
func exposureBuckets(ctx context.Context, db *sql.DB, op, macro, bucket, grade string, asOf int64,
	ccy string) ([]ExposureBucketRow, error) {
	return scanRows(ctx, db, op, `
SELECT `+bucket+`, `+grade+`, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, direct_pct, via_funds_pct, quality
  FROM `+macro+`(?, ?)`, []any{asOf, ccy}, func(r *ExposureBucketRow) []any {
		return []any{&r.Bucket, str(&r.Grade), dec(&r.Value), flt(&r.SharePct), flt(&r.DirectPct), flt(&r.ViaFundsPct),
			&r.Quality}
	})
}

// ExposureCredit runs report_exposure_credit.
func ExposureCredit(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "ExposureCredit", "report_exposure_credit", "bucket", "grade", asOf, ccy)
}

// ExposureMaturities runs report_exposure_maturities.
func ExposureMaturities(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "ExposureMaturities", "report_exposure_maturities", "bucket", "NULL", asOf, ccy)
}

// ExposureCoupons runs report_exposure_coupons.
func ExposureCoupons(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "ExposureCoupons", "report_exposure_coupons", "coupon_kind", "NULL", asOf, ccy)
}

// ExposureCycles runs report_exposure_cycles.
func ExposureCycles(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureBucketRow, error) {
	return exposureBuckets(ctx, db, "ExposureCycles", "report_exposure_cycles", "cycle", "NULL", asOf, ccy)
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

// ExposureIssuers runs report_exposure_issuers.
func ExposureIssuers(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureIssuerRow, error) {
	return scanRows(ctx, db, "ExposureIssuers", `
SELECT issuer, lei, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, cumulative_share_pct, equity_pct, debt_pct,
       cash_pct, securities, quality
  FROM report_exposure_issuers(?, ?)`, []any{asOf, ccy}, func(r *ExposureIssuerRow) []any {
		return []any{&r.Issuer, str(&r.LEI), dec(&r.Value), flt(&r.SharePct), flt(&r.CumulativeSharePct),
			flt(&r.EquityPct), flt(&r.DebtPct), flt(&r.CashPct), &r.Securities, &r.Quality}
	})
}
