package gold

import (
	"context"
	"database/sql"
)

// The exposure report's readers: one per view of migration 0126's
// report_exposure_* macros. docs/EXPOSURE.md defines every figure. Money
// leaves the macros as DOUBLE (the look-through splits values by weight)
// and is read here at four decimals, as every money column is.

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

// ExposureSecurityRow is one security, held directly and through every
// fund, or a leaf no security describes (cash, a holding, a remainder).
type ExposureSecurityRow struct {
	Security                                       string
	Name, Ticker, AssetClass, Sector, Country, Ccy *string
	MarketCap                                      *string
	Value, DirectValue, ViaFundsValue              *string
	Funds                                          int64
	SharePct                                       *float64
	Quality                                        string
}

// ExposureSecurities runs report_exposure_securities.
func ExposureSecurities(ctx context.Context, db *sql.DB, asOf int64, ccy, country string) ([]ExposureSecurityRow, error) {
	return scanRows(ctx, db, "ExposureSecurities", `
SELECT security, name, ticker, asset_class, sector, country, currency,
       CAST(market_cap::DECIMAL(28,0) AS VARCHAR), CAST(value::DECIMAL(28,4) AS VARCHAR),
       CAST(direct_value::DECIMAL(28,4) AS VARCHAR), CAST(via_funds_value::DECIMAL(28,4) AS VARCHAR),
       funds, share_pct, quality
  FROM report_exposure_securities(?, ?, p_country := ?)`, []any{asOf, ccy, country}, func(r *ExposureSecurityRow) []any {
		return []any{&r.Security, str(&r.Name), str(&r.Ticker), str(&r.AssetClass), str(&r.Sector), str(&r.Country),
			str(&r.Ccy), dec(&r.MarketCap), dec(&r.Value), dec(&r.DirectValue), dec(&r.ViaFundsValue),
			&r.Funds, flt(&r.SharePct), &r.Quality}
	})
}

// ExposureFundRow is one fund held directly, and what describes it at
// the instant; the last row is every other holding.
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
	Domicile, Replication    *string
	Lending                  *bool
	HedgedCurrency           *string
	Quality                  string
}

// ExposureFunds runs report_exposure_funds.
func ExposureFunds(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureFundRow, error) {
	return scanRows(ctx, db, "ExposureFunds", `
SELECT fund, name, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, via, proxy, leverage, provider, basis,
       composition_as_of, gap_days, lines, weight_sum, domicile, replication, lending, hedged_currency, quality
  FROM report_exposure_funds(?, ?)`, []any{asOf, ccy}, func(r *ExposureFundRow) []any {
		return []any{&r.Fund, str(&r.Name), dec(&r.Value), flt(&r.SharePct), str(&r.Via), str(&r.Proxy),
			flt(&r.Leverage), str(&r.Provider), str(&r.Basis), i64(&r.CompositionAsOf), i64(&r.GapDays),
			i64(&r.Lines), flt(&r.WeightSum), str(&r.Domicile), str(&r.Replication), boolp(&r.Lending),
			str(&r.HedgedCurrency), &r.Quality}
	})
}

// ExposureCoverageRow is one line of the coverage view: a section (how,
// age, unmapped, unmatched), a reason, and the value behind it.
type ExposureCoverageRow struct {
	Section, Reason string
	Items           int64
	Value           *string
	SharePct        *float64
	Detail          *string
}

// ExposureCoverage runs report_exposure_coverage.
func ExposureCoverage(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureCoverageRow, error) {
	return scanRows(ctx, db, "ExposureCoverage", `
SELECT section, reason, items, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, detail
  FROM report_exposure_coverage(?, ?)`, []any{asOf, ccy}, func(r *ExposureCoverageRow) []any {
		return []any{&r.Section, &r.Reason, &r.Items, dec(&r.Value), flt(&r.SharePct), str(&r.Detail)}
	})
}
