package gold

import (
	"context"
	"database/sql"
	"encoding/json"
)

// The exposure report's readers: one per report_exposure_* macro.
// docs/EXPOSURE.md defines every figure. Money leaves the macros as
// DOUBLE (the look-through splits values by weight) and is read here at
// four decimals, as every money column is. A macro's ORDER BY does not
// bind the query that reads it, so a reader whose macro orders by its
// own columns repeats that ORDER BY, and tied rows print in one order.

// ExposureShares are the share columns every aggregate view carries, in
// percent of net worth.
type ExposureShares struct {
	SharePct, DirectPct, ViaFundsPct *float64
}

// exposureDimension is one aggregate view of the exposure report: the
// op its errors name, its macro, the columns that key its rows, whether
// they carry a class share, whether the macro takes a country reading,
// and the ORDER BY its reader repeats, empty where the reader keeps the
// macro's order as it comes.
type exposureDimension struct {
	op, macro, keys     string
	classShare, country bool
	order               string
}

// exposureDimensions are the aggregate views, by view name. Each row is
// its keys, then its value, its shares and its quality (exposureRows).
var exposureDimensions = map[string]exposureDimension{
	"classes": {op: "ExposureClasses", macro: "report_exposure_classes", keys: "asset_class",
		order: "value DESC, asset_class"},
	"sectors": {op: "ExposureSectors", macro: "report_exposure_sectors", keys: "asset_class, sector", classShare: true},
	"sizes":   {op: "ExposureSizes", macro: "report_exposure_sizes", keys: "asset_class, size", classShare: true},
	"countries": {op: "ExposureCountries", macro: "report_exposure_countries", keys: "country, country_name, region",
		country: true, order: "value DESC, country, region"},
	"currencies": {op: "ExposureCurrencies", macro: "report_exposure_currencies", keys: "currency, currency_kind",
		order: "value DESC, currency"},
	"credit": {op: "ExposureCredit", macro: "report_exposure_credit", keys: "bucket, grade", classShare: true},
	"maturities": {op: "ExposureMaturities", macro: "report_exposure_maturities", keys: "bucket, NULL",
		classShare: true},
	"coupons": {op: "ExposureCoupons", macro: "report_exposure_coupons", keys: "coupon_kind, NULL", classShare: true},
	"cycles":  {op: "ExposureCycles", macro: "report_exposure_cycles", keys: "cycle, NULL", classShare: true},
}

// exposureCells are the scan targets an aggregate row shares: its value,
// its shares, its class share (nil for a view without one) and its
// quality.
type exposureCells struct {
	value      **string
	shares     *ExposureShares
	classShare **float64
	quality    *string
}

// exposureRows runs the aggregate view at asOf in ccy, and country for
// a view that takes one. keys gives a row's key targets, cells the rest.
func exposureRows[T any](ctx context.Context, db *sql.DB, view string, asOf int64, ccy, country string,
	keys func(*T) []any, cells func(*T) exposureCells) ([]T, error) {
	d := exposureDimensions[view]
	cols := "share_pct, "
	if d.classShare {
		cols += "class_share_pct, "
	}
	q := `SELECT ` + d.keys + `, CAST(value::DECIMAL(28,4) AS VARCHAR), ` + cols + `direct_pct, via_funds_pct, quality
  FROM ` + d.macro + `(?, ?`
	args := []any{asOf, ccy}
	if d.country {
		q += `, p_country := ?`
		args = append(args, country)
	}
	q += `)`
	if d.order != "" {
		q += ` ORDER BY ` + d.order
	}
	return scanRows(ctx, db, d.op, q, args, func(r *T) []any {
		c := cells(r)
		out := append(keys(r), dec(c.value), flt(&c.shares.SharePct))
		if d.classShare {
			out = append(out, flt(c.classShare))
		}
		return append(out, flt(&c.shares.DirectPct), flt(&c.shares.ViaFundsPct), c.quality)
	})
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
	return exposureRows(ctx, db, "classes", asOf, ccy, "",
		func(r *ExposureClassRow) []any { return []any{&r.AssetClass} },
		func(r *ExposureClassRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, quality: &r.Quality}
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
	return exposureRows(ctx, db, "sectors", asOf, ccy, "",
		func(r *ExposureSectorRow) []any { return []any{&r.AssetClass, &r.Sector} },
		func(r *ExposureSectorRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, classShare: &r.ClassSharePct, quality: &r.Quality}
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
	return exposureRows(ctx, db, "sizes", asOf, ccy, "",
		func(r *ExposureSizeRow) []any { return []any{&r.AssetClass, &r.Size} },
		func(r *ExposureSizeRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, classShare: &r.ClassSharePct, quality: &r.Quality}
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
	return exposureRows(ctx, db, "countries", asOf, ccy, country,
		func(r *ExposureCountryRow) []any { return []any{&r.Country, str(&r.CountryName), str(&r.Region)} },
		func(r *ExposureCountryRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, quality: &r.Quality}
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
	return exposureRows(ctx, db, "currencies", asOf, ccy, "",
		func(r *ExposureCurrencyRow) []any { return []any{&r.Currency, &r.Kind} },
		func(r *ExposureCurrencyRow) exposureCells {
			return exposureCells{value: &r.Value, shares: &r.ExposureShares, quality: &r.Quality}
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
  FROM report_exposure_situs(?, ?)
 ORDER BY value DESC, situs`, []any{asOf, ccy}, func(r *ExposureSitusRow) []any {
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
  FROM report_exposure_securities(?, ?, p_country := ?)
 ORDER BY abs(value) DESC, security`, []any{asOf, ccy, country}, func(r *ExposureSecurityRow) []any {
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
	// it is unknown; SitusBasis what it rests on.
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
  FROM report_exposure_funds(?, ?)
 ORDER BY fund = '(other holdings)', abs(value) DESC, fund`, []any{asOf, ccy}, func(r *ExposureFundRow) []any {
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
	// Skeleton is the declaration that fills the gap, on the first line
	// of each holding an undeclared or situs line names; nil on every
	// other line.
	Skeleton *DeclarationSkeleton
}

// DeclarationSkeleton is a record of the declarations ledger with its
// holding named and its dimensions empty. An empty dimension is refused
// at load, so a skeleton loads only once it is filled in or the
// dimension is taken out.
type DeclarationSkeleton struct {
	SilverSourceID string          `json:"silver_source_id"`
	Instrument     string          `json:"instrument"`
	Country        json.RawMessage `json:"country,omitempty"`
	Currency       json.RawMessage `json:"currency,omitempty"`
	Sector         json.RawMessage `json:"sector,omitempty"`
	Situs          json.RawMessage `json:"situs,omitempty"`
	Note           string          `json:"note,omitempty"`
}

// ExposureCoverage runs report_exposure_coverage, and gives each
// holding coverage lists as undeclared a skeleton with country,
// currency and sector, and each of unknown situs one with situs alone.
// Its note is the holding's name. A situs-only record leaves a
// holding's other records as they are, so each skeleton loads beside
// them. A holding with no instrument, or no value, takes no record.
func ExposureCoverage(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]ExposureCoverageRow, error) {
	rows, err := scanRows(ctx, db, "ExposureCoverage", `
SELECT section, reason, items, CAST(value::DECIMAL(28,4) AS VARCHAR), share_pct, detail, silver_source_id, instrument
  FROM report_exposure_coverage(?, ?)`, []any{asOf, ccy}, func(r *ExposureCoverageRow) []any {
		return []any{&r.Section, &r.Reason, &r.Items, dec(&r.Value), flt(&r.SharePct), str(&r.Detail),
			str(&r.SilverSourceID), str(&r.Instrument)}
	})
	if err != nil {
		return nil, err
	}
	empty := json.RawMessage(`{}`)
	type written struct{ section, src, iid string }
	seen := map[written]bool{}
	for i := range rows {
		r := &rows[i]
		if (r.Section != "undeclared" && r.Section != "situs") || r.SilverSourceID == nil || *r.SilverSourceID == "" ||
			r.Instrument == nil || *r.Instrument == "" {
			continue
		}
		k := written{r.Section, *r.SilverSourceID, *r.Instrument}
		if seen[k] {
			continue
		}
		seen[k] = true
		s := &DeclarationSkeleton{SilverSourceID: k.src, Instrument: k.iid}
		if r.Detail != nil {
			s.Note = *r.Detail
		}
		if r.Section == "undeclared" {
			s.Country, s.Currency, s.Sector = empty, empty, empty
		} else {
			s.Situs = empty
		}
		r.Skeleton = s
	}
	return rows, nil
}
