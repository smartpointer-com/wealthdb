package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// `wealthdb exposure <view>` — what is really owned, every fund looked
// through to what it holds, as of a day. docs/EXPOSURE.md defines every
// figure; the look-through is gold's (migration 0126), and this file is
// the views, their columns and the usage text.
//
// The holdings idiom: a positional view and an as-of day (-d, default
// today). --country belongs to the views that place a security in a
// country, countries and securities, and is refused elsewhere.
func init() {
	register("exposure", cmdExposure)
}

// exposureViews are the views, coarse to fine, then the diagnostics.
var exposureViews = []string{"classes", "sectors", "countries", "currencies", "securities", "funds", "coverage"}

// exposureCountryViews are the views --country applies to.
var exposureCountryViews = []string{"countries", "securities"}

func cmdExposure(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("exposure", subargs, exposureViews, exposureUsage, stderr)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("wealthdb exposure "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	period := fs.String("period", "", strings.Join(exposurePeriods, " | ")+" — a series over a window")
	country := fs.String("country", "risk", "risk | domicile — which country a security counts in")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, exposureUsage()) }
	if err := parseFlags(fs, rest, "exposure"); err != nil {
		return err
	}
	var series []seriesPoint
	if *period == "" {
		if err := noPositionals(fs, "exposure"); err != nil {
			return err
		}
	} else {
		if err := checkChoice("exposure", "period", *period, exposurePeriods...); err != nil {
			return err
		}
		if !slices.Contains(exposureSeriesViews, view) {
			return errs.Newf(2, "exposure: --period belongs to the %s views", strings.Join(exposureSeriesViews, ", "))
		}
		if isSet(fs, "d", "as-of") {
			return errs.Newf(2, "exposure: -d names one day; a series takes a window instead (FROM [TO])")
		}
		now := time.Now()
		from, to, err := parseTrailingYearWindow(fs.Args(), now)
		if err != nil {
			fs.Usage()
			return errs.Newf(2, "exposure: %s", err.Error())
		}
		// Nothing is held after today, so the window stops there.
		series = seriesPoints(from, min(to, anchorToDay(now.UTC(), true).Unix()), *period)
		if len(series) == 0 {
			return errs.Newf(2, "exposure: the window starts after today")
		}
	}
	if !slices.Contains(exposureCountryViews, view) && isSet(fs, "country") {
		return errs.Newf(2, "exposure: --country belongs to the %s views", strings.Join(exposureCountryViews, " and "))
	}
	if err := checkChoice("exposure", "country", *country, "risk", "domicile"); err != nil {
		return err
	}
	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "exposure: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "exposure")
	if err != nil {
		return err
	}
	rep := exposureReport(request{view: view, currency: outCcy, asOf: asOfEpoch, country: *country, series: series})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "exposure", open, *rf.privacy, fmtChoice, stdout)
}

// exposureReport is one view of the exposure family, the runner the CLI
// and the MCP server share: at one instant, or, with req.series, at the
// end of every period of a window, one row set per period.
func exposureReport(req request) *report {
	ccy, country := req.currency, req.country
	switch req.view {
	case "classes":
		return exposureView(req, buildExposureClassColumns(ccy), defaultExposureClassColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureClassRow, error) {
				return gold.ExposureClasses(ctx, db, at, ccy)
			})
	case "sectors":
		return exposureView(req, buildExposureSectorColumns(ccy), defaultExposureSectorColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSectorRow, error) {
				return gold.ExposureSectors(ctx, db, at, ccy)
			})
	case "countries":
		return exposureView(req, buildExposureCountryColumns(ccy), defaultExposureCountryColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCountryRow, error) {
				return gold.ExposureCountries(ctx, db, at, ccy, country)
			})
	case "currencies":
		return exposureView(req, buildExposureCurrencyColumns(ccy), defaultExposureCurrencyColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCurrencyRow, error) {
				return gold.ExposureCurrencies(ctx, db, at, ccy)
			})
	case "securities":
		return exposureView(req, buildExposureSecurityColumns(ccy), defaultExposureSecurityColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSecurityRow, error) {
				return gold.ExposureSecurities(ctx, db, at, ccy, country)
			})
	case "funds":
		return exposureView(req, buildExposureFundColumns(ccy), defaultExposureFundColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureFundRow, error) {
				return gold.ExposureFunds(ctx, db, at, ccy)
			})
	default: // coverage
		return exposureView(req, buildExposureCoverageColumns(ccy), defaultExposureCoverageColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCoverageRow, error) {
				return gold.ExposureCoverage(ctx, db, at, ccy)
			})
	}
}

// exposureView is a view at req.asOf, or its series over req.series
// when that holds a point.
func exposureView[T any](req request, registry []columnSpec[T], defaults []string,
	fetch func(context.Context, *sql.DB, int64) ([]T, error)) *report {
	if len(req.series) == 0 {
		return newReport(registry, defaults, func(ctx context.Context, db *sql.DB) ([]T, error) {
			return fetch(ctx, db, req.asOf)
		})
	}
	return seriesReport(registry, defaults, req.series, fetch)
}

// seriesPoint is one period of a series: its label and the instant its
// figures are taken at, the period's last second or the window's end.
type seriesPoint struct {
	label string
	at    int64
}

// seriesRow is a row of a view, tagged with its period.
type seriesRow[T any] struct {
	period string
	row    T
}

// seriesReport repeats a view at every point of a series, a period
// column ahead of the view's own. A point before the first holding gold
// holds is skipped: nothing was held then.
func seriesReport[T any](registry []columnSpec[T], defaults []string, points []seriesPoint,
	fetch func(context.Context, *sql.DB, int64) ([]T, error)) *report {
	reg := []columnSpec[seriesRow[T]]{{Name: "period", Align: output.AlignLeft,
		Extract: func(r seriesRow[T]) string { return r.period }}}
	for _, c := range registry {
		col := columnSpec[seriesRow[T]]{Name: c.Name, Header: c.Header, Align: c.Align, Privacy: c.Privacy,
			Extract: func(r seriesRow[T]) string { return c.Extract(r.row) }}
		if c.PrivacyFunc != nil {
			col.PrivacyFunc = func(r seriesRow[T]) PrivacyClass { return c.PrivacyFunc(r.row) }
		}
		reg = append(reg, col)
	}
	return newReport(reg, append([]string{"period"}, defaults...), func(ctx context.Context, db *sql.DB) ([]seriesRow[T], error) {
		first, ok, err := gold.FirstHoldingsAt(ctx, db)
		if err != nil || !ok {
			return nil, err
		}
		var out []seriesRow[T]
		for _, p := range points {
			if p.at < first {
				continue
			}
			rows, err := fetch(ctx, db, p.at)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				out = append(out, seriesRow[T]{period: p.label, row: r})
			}
		}
		return out, nil
	})
}

// exposurePeriods are the periods a series takes; a finer one would
// repeat the look-through more often than compositions change.
var exposurePeriods = []string{"monthly", "quarterly", "annual"}

// exposureSeriesViews are the views a series takes: the aggregates and
// coverage. A series of securities or funds would be thousands of rows a
// period.
var exposureSeriesViews = []string{"classes", "sectors", "countries", "currencies", "coverage"}

// seriesPoints are the ends of the periods of `period` that meet the
// window [from, to]: each period's last second, the window's end for the
// last one. None when from is after to.
func seriesPoints(from, to int64, period string) []seriesPoint {
	if from > to {
		return nil
	}
	t := time.Unix(from, 0).UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	var step int
	switch period {
	case "monthly":
		step = 1
	case "quarterly":
		step = 3
		start = start.AddDate(0, -((int(start.Month()) - 1) % 3), 0)
	case "annual":
		step = 12
		start = time.Date(start.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return nil
	}
	var out []seriesPoint
	for ; start.Unix() <= to; start = start.AddDate(0, step, 0) {
		b := start.Unix()
		out = append(out, seriesPoint{label: periodLabel(&b, period), at: min(start.AddDate(0, step, 0).Unix()-1, to)})
	}
	return out
}

// ---- column registries ---------------------------------------------------
//
// value prints as value_<CCY>, a share of net worth as share_pct. -p
// hides every amount and keeps the shares.

// pctCol is a column that is already in percent: two decimals, blank
// for nil.
func pctCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Extract: func(r T) string {
		if p := get(r); p != nil {
			return fmt.Sprintf("%.2f", *p)
		}
		return ""
	}}
}

// shareCols are the share columns of the aggregate views.
func shareCols[T any](get func(T) gold.ExposureShares) []columnSpec[T] {
	return []columnSpec[T]{
		pctCol("share_pct", func(r T) *float64 { return get(r).SharePct }),
		pctCol("direct_pct", func(r T) *float64 { return get(r).DirectPct }),
		pctCol("via_funds_pct", func(r T) *float64 { return get(r).ViaFundsPct }),
	}
}

func qualityCol[T any](get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: "quality", Align: output.AlignLeft, Extract: get}
}

func buildExposureClassColumns(ccy string) []columnSpec[gold.ExposureClassRow] {
	type row = gold.ExposureClassRow
	cols := []columnSpec[row]{
		{Name: "asset_class", Align: output.AlignLeft, Extract: func(r row) string { return r.AssetClass }},
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols, qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureClassColumns = []string{"asset_class", "value", "share_pct", "direct_pct", "via_funds_pct", "quality"}

func buildExposureSectorColumns(ccy string) []columnSpec[gold.ExposureSectorRow] {
	type row = gold.ExposureSectorRow
	cols := []columnSpec[row]{
		{Name: "asset_class", Align: output.AlignLeft, Extract: func(r row) string { return r.AssetClass }},
		{Name: "sector", Align: output.AlignLeft, Extract: func(r row) string { return r.Sector }},
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols,
		pctCol("class_share_pct", func(r row) *float64 { return r.ClassSharePct }),
		qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureSectorColumns = []string{"asset_class", "sector", "value", "share_pct", "class_share_pct", "quality"}

func buildExposureCountryColumns(ccy string) []columnSpec[gold.ExposureCountryRow] {
	type row = gold.ExposureCountryRow
	cols := []columnSpec[row]{
		{Name: "country", Align: output.AlignLeft, Extract: func(r row) string { return r.Country }},
		textCol("country_name", func(r row) *string { return r.CountryName }),
		textCol("region", func(r row) *string { return r.Region }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols, qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureCountryColumns = []string{"country", "country_name", "region", "value", "share_pct",
	"direct_pct", "via_funds_pct", "quality"}

func buildExposureCurrencyColumns(ccy string) []columnSpec[gold.ExposureCurrencyRow] {
	type row = gold.ExposureCurrencyRow
	cols := []columnSpec[row]{
		{Name: "currency", Align: output.AlignLeft, Extract: func(r row) string { return r.Currency }},
		{Name: "kind", Align: output.AlignLeft, Extract: func(r row) string { return r.Kind }},
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols, qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureCurrencyColumns = []string{"currency", "kind", "value", "share_pct", "direct_pct", "via_funds_pct", "quality"}

func buildExposureSecurityColumns(ccy string) []columnSpec[gold.ExposureSecurityRow] {
	type row = gold.ExposureSecurityRow
	return []columnSpec[row]{
		// A security key is a public identifier and stays legible under
		// -p, as position_key does; a holding of one source keys by its
		// instrument id, which -p hides whole.
		{Name: "security", Align: output.AlignLeft,
			PrivacyFunc: func(r row) PrivacyClass {
				if strings.HasPrefix(r.Security, "holding:") {
					return PrivacyFreeText
				}
				return PrivacyNone
			},
			Extract: func(r row) string { return r.Security }},
		textCol("name", func(r row) *string { return r.Name }),
		textCol("ticker", func(r row) *string { return r.Ticker }),
		textCol("asset_class", func(r row) *string { return r.AssetClass }),
		textCol("sector", func(r row) *string { return r.Sector }),
		textCol("country", func(r row) *string { return r.Country }),
		textCol("currency", func(r row) *string { return r.Ccy }),
		// A public figure, as its source states it: legible under -p.
		textCol("market_cap", func(r row) *string { return r.MarketCap }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		outCcyCol("direct_value", ccy, func(r row) *string { return r.DirectValue }),
		outCcyCol("via_funds_value", ccy, func(r row) *string { return r.ViaFundsValue }),
		countCol("funds", func(r row) int64 { return r.Funds }),
		pctCol("share_pct", func(r row) *float64 { return r.SharePct }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureSecurityColumns = []string{"security", "name", "asset_class", "sector", "country", "currency",
	"value", "direct_value", "via_funds_value", "share_pct", "quality"}

func buildExposureFundColumns(ccy string) []columnSpec[gold.ExposureFundRow] {
	type row = gold.ExposureFundRow
	return []columnSpec[row]{
		{Name: "fund", Align: output.AlignLeft,
			PrivacyFunc: func(r row) PrivacyClass {
				if strings.HasPrefix(r.Fund, "holding:") {
					return PrivacyFreeText
				}
				return PrivacyNone
			},
			Extract: func(r row) string { return r.Fund }},
		textCol("name", func(r row) *string { return r.Name }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share_pct", func(r row) *float64 { return r.SharePct }),
		textCol("via", func(r row) *string { return r.Via }),
		textCol("proxy", func(r row) *string { return r.Proxy }),
		{Name: "leverage", Align: output.AlignRight, Extract: func(r row) string {
			if r.Leverage == nil {
				return ""
			}
			return fmt.Sprintf("%g", *r.Leverage)
		}},
		textCol("provider", func(r row) *string { return r.Provider }),
		textCol("basis", func(r row) *string { return r.Basis }),
		{Name: "composition_date", Align: output.AlignLeft, Extract: func(r row) string {
			if r.CompositionAsOf == nil {
				return ""
			}
			return formatDate(*r.CompositionAsOf)
		}},
		{Name: "gap_days", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.GapDays) }},
		{Name: "lines", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.Lines) }},
		pctCol("weight_sum_pct", func(r row) *float64 { return r.WeightSum }),
		textCol("domicile", func(r row) *string { return r.Domicile }),
		textCol("replication", func(r row) *string { return r.Replication }),
		{Name: "lending", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(r.Lending) }},
		textCol("hedged_currency", func(r row) *string { return r.HedgedCurrency }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureFundColumns = []string{"fund", "name", "value", "share_pct", "via", "proxy", "provider",
	"composition_date", "gap_days", "lines", "weight_sum_pct", "quality"}

func buildExposureCoverageColumns(ccy string) []columnSpec[gold.ExposureCoverageRow] {
	type row = gold.ExposureCoverageRow
	return []columnSpec[row]{
		{Name: "section", Align: output.AlignLeft, Extract: func(r row) string { return r.Section }},
		// An unmatched holding no key names reads as its instrument id,
		// which -p hides whole.
		{Name: "reason", Align: output.AlignLeft,
			PrivacyFunc: func(r row) PrivacyClass {
				if strings.HasPrefix(r.Reason, "holding:") {
					return PrivacyFreeText
				}
				return PrivacyNone
			},
			Extract: func(r row) string { return r.Reason }},
		countCol("items", func(r row) int64 { return r.Items }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share_pct", func(r row) *float64 { return r.SharePct }),
		textCol("detail", func(r row) *string { return r.Detail }),
		// What a tags ledger row names; -p hides the instrument id whole.
		textCol("silver_source_id", func(r row) *string { return r.SilverSourceID }),
		{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r row) string { return strOrEmpty(r.Instrument) }},
	}
}

var defaultExposureCoverageColumns = []string{"section", "reason", "items", "value", "share_pct", "detail",
	"silver_source_id", "instrument"}

func exposureUsage() string {
	cols := map[string]string{
		"classes":    joinColumnNames(buildExposureClassColumns("CCY")),
		"sectors":    joinColumnNames(buildExposureSectorColumns("CCY")),
		"countries":  joinColumnNames(buildExposureCountryColumns("CCY")),
		"currencies": joinColumnNames(buildExposureCurrencyColumns("CCY")),
		"securities": joinColumnNames(buildExposureSecurityColumns("CCY")),
		"funds":      joinColumnNames(buildExposureFundColumns("CCY")),
		"coverage":   joinColumnNames(buildExposureCoverageColumns("CCY")),
	}
	return `usage: wealthdb exposure <view> [-d DATE] [--country risk|domicile]
                        [-f FORMAT] [-C COLS] [-x CCY] [-p]
       wealthdb exposure <view> --period monthly|quarterly|annual [FROM [TO]]

What is really owned. Every fund is looked through to what it holds, a
fund of funds down to three levels, and the result is added to what is
held directly. Each holding joins the fund composition nearest the day:
the latest on or before it, else the earliest after, within
exposure.max_gap_days (default 400). Every view sums to net worth, as
'wealthdb holdings global' gives it; what a composition does not explain
is a remainder row. Read the quality column, and the coverage view,
before trusting a figure. docs/EXPOSURE.md defines every figure.

Views
  classes      one row per asset class
  sectors      one row per asset class and sector, with the share within
               the class
  countries    one row per country, with its region
  currencies   one row per currency a value moves with: a fiat currency,
               a metal (XAU) or a coin (BTC)
  securities   one row per security, held directly and through every fund
  funds        one row per fund held directly: what describes it on the
               day, and how old it is; then one row for all other
               holdings
  coverage     what the figures rest on: how each part of net worth is
               classified, how old the compositions are, the labels no
               map knows, the securities no source describes, the
               private holdings the tags ledger has no row for, and how
               far each fund read from a report lies from its proxy

Flags
  -d, --as-of DATE   the day (default today UTC)
  --period P         monthly | quarterly | annual: the view at the end of
                     every period of the window FROM..TO (default the
                     last twelve months; never past today), a period
                     column first; on classes, sectors, countries,
                     currencies and coverage
  --country C        risk (default) | domicile: the country a security
                     counts in, on countries and securities
  -f FORMAT          table | csv | csv_plain | json
  -C COLS            comma-separated names, 'default', 'all', or a
                     +ADD,-REMOVE delta on the default set
  -x CCY             output currency (default: config.default_currency)
  -p                 hide every amount; shares, names and labels stay

Notes
  share_pct is a share of net worth, so a loan is a negative row;
  direct_pct and via_funds_pct split it into what is held directly and
  what through funds.

  quality names what a figure rests on: proxy (another fund's
  composition stands in), aged (the composition is older than 35 days),
  forward (the composition is dated after the day), partial (the
  weights leave more than 3% unexplained, a forward states no legs, or
  a fund lies deeper than three levels), unmapped (a provider's label
  has no place in the vocabulary), by_name (a receipt's or a report
  line's sector or country comes from a share matched by name among all
  shares), opaque (a private equity
  fund nothing looks through; it reads diversified), untagged (a
  private holding has no row in the tags ledger), unmatched (a listed
  security no source describes). In funds, no_composition marks a fund
  nothing describes on the day.

Available columns (per view):
  classes      ` + cols["classes"] + `
  sectors      ` + cols["sectors"] + `
  countries    ` + cols["countries"] + `
  currencies   ` + cols["currencies"] + `
  securities   ` + cols["securities"] + `
  funds        ` + cols["funds"] + `
  coverage     ` + cols["coverage"] + `

Default column sets:
  classes      ` + strings.Join(defaultExposureClassColumns, ", ") + `
  sectors      ` + strings.Join(defaultExposureSectorColumns, ", ") + `
  countries    ` + strings.Join(defaultExposureCountryColumns, ", ") + `
  currencies   ` + strings.Join(defaultExposureCurrencyColumns, ", ") + `
  securities   ` + strings.Join(defaultExposureSecurityColumns, ", ") + `
  funds        ` + strings.Join(defaultExposureFundColumns, ", ") + `
  coverage     ` + strings.Join(defaultExposureCoverageColumns, ", ")
}
