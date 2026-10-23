package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
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

// exposureNone is the bucket of what a dimension does not describe.
const exposureNone = "(none)"

// exposureBuckets runs one of the bucket views; grade is the column that
// holds a credit bucket's grade, NULL for the views that have none.
func exposureBuckets(ctx context.Context, db *sql.DB, op, macro, bucket, grade string, asOf int64,
	ccy string) ([]ExposureBucketRow, error) {
	rows, err := scanRows(ctx, db, op, `
SELECT `+bucket+`, `+grade+`, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, direct_pct, via_funds_pct, quality
  FROM `+macro+`(?, ?)`, []any{asOf, ccy}, func(r *ExposureBucketRow) []any {
		return []any{&r.Bucket, str(&r.Grade), dec(&r.Value), flt(&r.SharePct), flt(&r.DirectPct), flt(&r.ViaFundsPct),
			&r.Quality}
	})
	if err != nil {
		return nil, err
	}
	if err := setClassShares(rows); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return rows, nil
}

// setClassShares gives each row but (none) its value's share of the sum
// of their values. Where that sum is zero or less, a loan or an
// overdraft outweighing the rest, no row has one: a share of it would
// flip its sign or have none. It runs over the rows read rather than in
// the view: the view orders its rows by bucket, and a window over them
// need not keep that order.
func setClassShares(rows []ExposureBucketRow) error {
	values := make([]float64, len(rows))
	total := 0.0
	for i, r := range rows {
		if r.Bucket == exposureNone || r.Value == nil {
			continue
		}
		v, err := strconv.ParseFloat(*r.Value, 64)
		if err != nil {
			return fmt.Errorf("bucket %s: value %q: %w", r.Bucket, *r.Value, err)
		}
		values[i], total = v, total+v
	}
	if total <= 0 {
		return nil
	}
	for i := range rows {
		if rows[i].Bucket != exposureNone && rows[i].Value != nil {
			share := 100 * values[i] / total
			rows[i].ClassSharePct = &share
		}
	}
	return nil
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
