package gold

import (
	"context"
	"database/sql"
)

// The exposure report's readers: one per report_exposure_* macro.
// docs/EXPOSURE.md defines every figure. Money leaves the macros as
// DOUBLE (the look-through splits values by weight) and is read here at
// four decimals, as every money column is.

// querier is what *sql.DB and *sql.Tx share for a read.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanAll runs a query and hands each row to scan.
func scanAll(ctx context.Context, q querier, query string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExposureShares are the share columns every aggregate view carries, in
// percent of net worth.
type ExposureShares struct {
	SharePct, DirectPct, ViaFundsPct *float64
}

// ExposureClassRow is one asset class after look-through.
type ExposureClassRow struct {
	AssetClass string
	Value      *string
	ExposureShares
	Quality string
}

// ExposureClasses runs report_exposure_classes.
func ExposureClasses(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureClassRow, error) {
	return scanRows(ctx, db, "ExposureClasses", `
SELECT asset_class, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, direct_pct, via_funds_pct, quality
  FROM report_exposure_classes(?, ?)`, []any{asOf, ccy}, func(r *ExposureClassRow) []any {
		return []any{&r.AssetClass, dec(&r.Value), flt(&r.SharePct), flt(&r.DirectPct), flt(&r.ViaFundsPct), &r.Quality}
	})
}

// ExposureSectorRow is one sector of one asset class.
type ExposureSectorRow struct {
	AssetClass, Sector string
	Value              *string
	ExposureShares
	ClassSharePct *float64
	Quality       string
}

// ExposureSectors runs report_exposure_sectors.
func ExposureSectors(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureSectorRow, error) {
	return scanRows(ctx, db, "ExposureSectors", `
SELECT asset_class, sector, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, class_share_pct,
       direct_pct, via_funds_pct, quality
  FROM report_exposure_sectors(?, ?)`, []any{asOf, ccy}, func(r *ExposureSectorRow) []any {
		return []any{&r.AssetClass, &r.Sector, dec(&r.Value), flt(&r.SharePct), flt(&r.ClassSharePct),
			flt(&r.DirectPct), flt(&r.ViaFundsPct), &r.Quality}
	})
}

// ExposureSizeRow is one size of one asset class. A size applies to
// public equity alone; any other class has the size `(none)`.
type ExposureSizeRow struct {
	AssetClass, Size string
	Value            *string
	ExposureShares
	ClassSharePct *float64
	Quality       string
}

// ExposureSizes runs report_exposure_sizes.
func ExposureSizes(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureSizeRow, error) {
	return scanRows(ctx, db, "ExposureSizes", `
SELECT asset_class, size, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, class_share_pct,
       direct_pct, via_funds_pct, quality
  FROM report_exposure_sizes(?, ?)`, []any{asOf, ccy}, func(r *ExposureSizeRow) []any {
		return []any{&r.AssetClass, &r.Size, dec(&r.Value), flt(&r.SharePct), flt(&r.ClassSharePct),
			flt(&r.DirectPct), flt(&r.ViaFundsPct), &r.Quality}
	})
}

// ExposureCountryRow is one country, with its region.
type ExposureCountryRow struct {
	Country     string
	CountryName *string
	Region      *string
	Value       *string
	ExposureShares
	Quality string
}

// ExposureCountries runs report_exposure_countries; country is risk or
// domicile.
func ExposureCountries(ctx context.Context, db *sql.DB, asOf int64, ccy, country string) ([]ExposureCountryRow, error) {
	return scanRows(ctx, db, "ExposureCountries", `
SELECT country, country_name, region, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, direct_pct,
       via_funds_pct, quality
  FROM report_exposure_countries(?, ?, p_country := ?)`, []any{asOf, ccy, country}, func(r *ExposureCountryRow) []any {
		return []any{&r.Country, str(&r.CountryName), str(&r.Region), dec(&r.Value), flt(&r.SharePct),
			flt(&r.DirectPct), flt(&r.ViaFundsPct), &r.Quality}
	})
}

// ExposureCurrencyRow is one currency: a fiat currency, a metal (XAU)
// or a coin.
type ExposureCurrencyRow struct {
	Currency, Kind string
	Value          *string
	ExposureShares
	Quality string
}

// ExposureCurrencies runs report_exposure_currencies.
func ExposureCurrencies(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureCurrencyRow, error) {
	return scanRows(ctx, db, "ExposureCurrencies", `
SELECT currency, currency_kind, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, direct_pct, via_funds_pct, quality
  FROM report_exposure_currencies(?, ?)`, []any{asOf, ccy}, func(r *ExposureCurrencyRow) []any {
		return []any{&r.Currency, &r.Kind, dec(&r.Value), flt(&r.SharePct), flt(&r.DirectPct), flt(&r.ViaFundsPct), &r.Quality}
	})
}

// ExposureSitusRow is one legal situs: the country whose law a holding
// is an interest under, every fund at its own. FundsPct is the share of
// the row's gross value held in funds; Quality the bases the row rests
// on.
type ExposureSitusRow struct {
	Situs       string
	CountryName *string
	Region      *string
	Value       *string
	SharePct    *float64
	FundsPct    *float64
	Quality     string
}

// ExposureSitus runs report_exposure_situs.
func ExposureSitus(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureSitusRow, error) {
	return scanRows(ctx, db, "ExposureSitus", `
SELECT situs, country_name, region, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, funds_pct, quality
  FROM report_exposure_situs(?, ?)`, []any{asOf, ccy}, func(r *ExposureSitusRow) []any {
		return []any{&r.Situs, str(&r.CountryName), str(&r.Region), dec(&r.Value), flt(&r.SharePct), flt(&r.FundsPct),
			&r.Quality}
	})
}

// ExposureSecurityRow is one security, held directly and through every
// fund, or a leaf no security describes (cash, a holding, a remainder).
type ExposureSecurityRow struct {
	Security                                       string
	Name, Ticker, AssetClass, Sector, Country, Ccy *string
	MarketCap, Size                                *string
	Value, DirectValue, ViaFundsValue              *string
	Funds                                          int64
	SharePct                                       *float64
	Quality                                        string
}

// ExposureSecurities runs report_exposure_securities.
func ExposureSecurities(ctx context.Context, db *sql.DB, asOf int64, ccy, country string) ([]ExposureSecurityRow, error) {
	return scanRows(ctx, db, "ExposureSecurities", `
SELECT security, name, ticker, asset_class, sector, country, currency,
       CAST(market_cap::DECIMAL(28,0) AS VARCHAR), size, CAST(value::DECIMAL(28,4) AS VARCHAR),
       CAST(direct_value::DECIMAL(28,4) AS VARCHAR), CAST(via_funds_value::DECIMAL(28,4) AS VARCHAR),
       funds, share_pct, quality
  FROM report_exposure_securities(?, ?, p_country := ?)`, []any{asOf, ccy, country}, func(r *ExposureSecurityRow) []any {
		return []any{&r.Security, str(&r.Name), str(&r.Ticker), str(&r.AssetClass), str(&r.Sector), str(&r.Country),
			str(&r.Ccy), dec(&r.MarketCap), str(&r.Size), dec(&r.Value), dec(&r.DirectValue), dec(&r.ViaFundsValue),
			&r.Funds, flt(&r.SharePct), &r.Quality}
	})
}

// ExposureFundRow is one fund held directly, and what describes it at
// the instant: a composition, or a declaration; the last row is every
// other holding.
type ExposureFundRow struct {
	Fund                     string
	Name                     *string
	Value                    *string
	SharePct                 *float64
	Via, Proxy               *string
	Leverage                 *float64
	Provider, Basis          *string
	CompositionAsOf, GapDays *int64
	Lines                    *int64
	WeightSum                *float64
	Domicile                 *string
	// Situs is the legal situs of the fund's largest holding, nil where
	// it is unclassified; SitusBasis what it rests on.
	Situs, SitusBasis *string
	Replication       *string
	Lending           *bool
	HedgedCurrency    *string
	// DeclarationAsOf is the declaration's date, nil for one without a
	// date; DeclaredDimensions are the dimensions it states.
	DeclarationAsOf    *int64
	DeclaredDimensions *string
	Quality            string
}

// ExposureFunds runs report_exposure_funds.
func ExposureFunds(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureFundRow, error) {
	return scanRows(ctx, db, "ExposureFunds", `
SELECT fund, name, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, via, proxy, leverage, provider, basis,
       composition_as_of, gap_days, lines, weight_sum, domicile, situs, situs_basis, replication, lending,
       hedged_currency, declaration_as_of, declared_dimensions, quality
  FROM report_exposure_funds(?, ?)`, []any{asOf, ccy}, func(r *ExposureFundRow) []any {
		return []any{&r.Fund, str(&r.Name), dec(&r.Value), flt(&r.SharePct), str(&r.Via), str(&r.Proxy),
			flt(&r.Leverage), str(&r.Provider), str(&r.Basis), i64(&r.CompositionAsOf), i64(&r.GapDays),
			i64(&r.Lines), flt(&r.WeightSum), str(&r.Domicile), str(&r.Situs), str(&r.SitusBasis),
			str(&r.Replication), boolp(&r.Lending), str(&r.HedgedCurrency), i64(&r.DeclarationAsOf),
			str(&r.DeclaredDimensions), &r.Quality}
	})
}

// ExposureCoverageRow is one line of the coverage view: a section (how,
// age, unmapped, unmatched, undeclared, divergence, situs), a reason, and
// the value behind it. A holding's line names its source and instrument,
// as a declaration does.
type ExposureCoverageRow struct {
	Section, Reason            string
	Items                      int64
	Value                      *string
	SharePct                   *float64
	Detail                     *string
	SilverSourceID, Instrument *string
}

// ExposureCoverage runs report_exposure_coverage.
func ExposureCoverage(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureCoverageRow, error) {
	return scanRows(ctx, db, "ExposureCoverage", `
SELECT section, reason, items, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, detail, silver_source_id, instrument
  FROM report_exposure_coverage(?, ?)`, []any{asOf, ccy}, func(r *ExposureCoverageRow) []any {
		return []any{&r.Section, &r.Reason, &r.Items, dec(&r.Value), flt(&r.SharePct), str(&r.Detail),
			str(&r.SilverSourceID), str(&r.Instrument)}
	})
}
