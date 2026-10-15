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

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// `wealthdb exposure <view>` — what is really owned, every fund looked
// through to what it holds, as of a day. docs/EXPOSURE.md defines every
// figure; the look-through is gold's (gold/exposure.go and the exposure
// migrations), and this file is the views, their columns and the usage
// text.
//
// The holdings idiom: a positional view and an as-of day (-d, default
// today). --country belongs to the views that place a security in a
// country, countries and securities, and is refused elsewhere. --period
// P turns the as-of view into a series over a positional window (FROM
// [TO]); it applies to exposureSeriesViews and refuses -d.
func init() {
	register("exposure", cmdExposure)
}

// exposureViews are the views, coarse to fine, then the diagnostics.
var exposureViews = []string{"classes", "sectors", "sizes", "countries", "currencies", "securities", "funds", "coverage"}

// exposureCountryViews are the views --country applies to.
var exposureCountryViews = []string{"countries", "securities"}

// exposureCountryBases are the bases a security's country is read on,
// the default first.
var exposureCountryBases = []string{"risk", "domicile"}

// errCountryView refuses the country basis, by the name its front-end
// gives it, on a view that places no security in a country.
func errCountryView(name string) error {
	return errs.Newf(2, "exposure: %s belongs to the %s views", name, strings.Join(exposureCountryViews, " and "))
}

func cmdExposure(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("exposure", subargs, exposureViews, exposureUsage, stderr)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("wealthdb exposure "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asOf := registerAsOf(fs)
	period := fs.String("period", "", strings.Join(exposurePeriods, " | ")+" — a series over a window")
	country := fs.String("country", exposureCountryBases[0],
		strings.Join(exposureCountryBases, " | ")+" — which country a security counts in")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, exposureUsage()) }
	if err := parseFlags(fs, rest, "exposure"); err != nil {
		return err
	}
	now := time.Now()
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
		return errCountryView("--country")
	}
	if err := checkChoice("exposure", "country", *country, exposureCountryBases...); err != nil {
		return err
	}
	asOfEpoch, err := parseAsOf(*asOf, now)
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
	case "sizes":
		return exposureView(req, buildExposureSizeColumns(ccy), defaultExposureSizeColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSizeRow, error) {
				return gold.ExposureSizes(ctx, db, at, ccy)
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

// exposurePeriodMonths are the periods a series takes, each with its
// length in months; a finer one would repeat the look-through more often
// than compositions change.
var exposurePeriodMonths = map[string]int{"monthly": 1, "quarterly": 3, "annual": 12}

// exposurePeriods is exposurePeriodMonths' key set in display order.
var exposurePeriods = []string{"monthly", "quarterly", "annual"}

// exposureSeriesViews are the views a series takes: the aggregates and
// coverage. A series of securities or funds would be thousands of rows a
// period.
var exposureSeriesViews = []string{"classes", "sectors", "sizes", "countries", "currencies", "coverage"}

// seriesPoints are the periods of `period` that meet the window [from,
// to], each taken at its end (gold.PeriodEnds) and labelled by it. None
// when from is after to.
func seriesPoints(from, to int64, period string) []seriesPoint {
	var out []seriesPoint
	for _, at := range gold.PeriodEnds(from, to, exposurePeriodMonths[period]) {
		out = append(out, seriesPoint{label: periodLabel(&at, period), at: at})
	}
	return out
}

// ---- column registries ---------------------------------------------------
//
// value prints as value_<CCY>; a share of net worth is named share and
// prints as share_pct. -p hides every amount and keeps the shares.

// shareCols are the share columns of the aggregate views.
func shareCols[T any](get func(T) gold.ExposureShares) []columnSpec[T] {
	return []columnSpec[T]{
		pctCol("share", func(r T) *float64 { return get(r).SharePct }),
		pctCol("direct", func(r T) *float64 { return get(r).DirectPct }),
		pctCol("via_funds", func(r T) *float64 { return get(r).ViaFundsPct }),
	}
}

func qualityCol[T any](get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: "quality", Align: output.AlignLeft, Extract: get}
}

// holdingKeyCol is a key column. A security key is a public identifier
// and stays legible under -p, as position_key does; a holding no key
// names reads as its source and instrument id, which -p hides whole.
func holdingKeyCol[T any](name string, get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: get,
		PrivacyFunc: func(r T) PrivacyClass {
			if strings.HasPrefix(get(r), gold.HoldingKeyPrefix) {
				return PrivacyFreeText
			}
			return PrivacyNone
		}}
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

var defaultExposureClassColumns = []string{"asset_class", "value", "share", "direct", "via_funds", "quality"}

func buildExposureSectorColumns(ccy string) []columnSpec[gold.ExposureSectorRow] {
	type row = gold.ExposureSectorRow
	cols := []columnSpec[row]{
		{Name: "asset_class", Align: output.AlignLeft, Extract: func(r row) string { return r.AssetClass }},
		{Name: "sector", Align: output.AlignLeft, Extract: func(r row) string { return r.Sector }},
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols,
		pctCol("class_share", func(r row) *float64 { return r.ClassSharePct }),
		qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureSectorColumns = []string{"asset_class", "sector", "value", "share", "class_share", "quality"}

func buildExposureSizeColumns(ccy string) []columnSpec[gold.ExposureSizeRow] {
	type row = gold.ExposureSizeRow
	cols := []columnSpec[row]{
		{Name: "asset_class", Align: output.AlignLeft, Extract: func(r row) string { return r.AssetClass }},
		{Name: "size", Align: output.AlignLeft, Extract: func(r row) string { return r.Size }},
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
	}
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols,
		pctCol("class_share", func(r row) *float64 { return r.ClassSharePct }),
		qualityCol(func(r row) string { return r.Quality }))
}

var defaultExposureSizeColumns = []string{"asset_class", "size", "value", "share", "class_share", "quality"}

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

var defaultExposureCountryColumns = []string{"country", "country_name", "region", "value", "share",
	"direct", "via_funds", "quality"}

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

var defaultExposureCurrencyColumns = []string{"currency", "kind", "value", "share", "direct", "via_funds", "quality"}

func buildExposureSecurityColumns(ccy string) []columnSpec[gold.ExposureSecurityRow] {
	type row = gold.ExposureSecurityRow
	return []columnSpec[row]{
		holdingKeyCol("security", func(r row) string { return r.Security }),
		textCol("name", func(r row) *string { return r.Name }),
		textCol("ticker", func(r row) *string { return r.Ticker }),
		textCol("asset_class", func(r row) *string { return r.AssetClass }),
		textCol("sector", func(r row) *string { return r.Sector }),
		textCol("country", func(r row) *string { return r.Country }),
		textCol("currency", func(r row) *string { return r.Ccy }),
		// A public figure, as a source states it or EDGAR's shares and a
		// price make it: legible under -p.
		textCol("market_cap", func(r row) *string { return r.MarketCap }),
		textCol("size", func(r row) *string { return r.Size }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		outCcyCol("direct_value", ccy, func(r row) *string { return r.DirectValue }),
		outCcyCol("via_funds_value", ccy, func(r row) *string { return r.ViaFundsValue }),
		countCol("funds", func(r row) int64 { return r.Funds }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureSecurityColumns = []string{"security", "name", "asset_class", "sector", "country", "currency",
	"value", "direct_value", "via_funds_value", "share", "quality"}

func buildExposureFundColumns(ccy string) []columnSpec[gold.ExposureFundRow] {
	type row = gold.ExposureFundRow
	return []columnSpec[row]{
		holdingKeyCol("fund", func(r row) string { return r.Fund }),
		textCol("name", func(r row) *string { return r.Name }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
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
		datePtrCol("composition_date", func(r row) *int64 { return r.CompositionAsOf }),
		{Name: "gap_days", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.GapDays) }},
		{Name: "lines", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.Lines) }},
		pctCol("weight_sum", func(r row) *float64 { return r.WeightSum }),
		textCol("domicile", func(r row) *string { return r.Domicile }),
		textCol("replication", func(r row) *string { return r.Replication }),
		{Name: "lending", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(r.Lending) }},
		textCol("hedged_currency", func(r row) *string { return r.HedgedCurrency }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureFundColumns = []string{"fund", "name", "value", "share", "via", "proxy", "provider",
	"composition_date", "gap_days", "lines", "weight_sum", "quality"}

func buildExposureCoverageColumns(ccy string) []columnSpec[gold.ExposureCoverageRow] {
	type row = gold.ExposureCoverageRow
	return []columnSpec[row]{
		{Name: "section", Align: output.AlignLeft, Extract: func(r row) string { return r.Section }},
		// An unmatched holding no key names reads as its instrument id.
		holdingKeyCol("reason", func(r row) string { return r.Reason }),
		countCol("items", func(r row) int64 { return r.Items }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		textCol("detail", func(r row) *string { return r.Detail }),
		// What a tags ledger row names; -p hides the instrument id whole.
		textCol("silver_source_id", func(r row) *string { return r.SilverSourceID }),
		{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r row) string { return strOrEmpty(r.Instrument) }},
	}
}

var defaultExposureCoverageColumns = []string{"section", "reason", "items", "value", "share", "detail",
	"silver_source_id", "instrument"}

// exposureQualityTags are the tags of the exposure quality column and
// what each means: the CLI usage and the MCP help both render them.
var exposureQualityTags = []struct{ tag, meaning string }{
	{"proxy", "another fund's composition stands in"},
	{"aged", fmt.Sprintf("the composition is older than %d days", canonical.DefaultAgedDays)},
	{"forward", "the composition is dated after the day"},
	{"partial", fmt.Sprintf("the weights leave more than %g%% unexplained, a forward states no legs, "+
		"or a fund lies deeper than three levels", canonical.DefaultPartialPct)},
	{"unmapped", "a provider's label has no place in the vocabulary"},
	{"by_name", "a receipt's or a report line's sector or country comes from a share matched by name " +
		"among all shares"},
	{"opaque", "a private equity fund nothing looks through; it reads diversified"},
	{"untagged", "a private holding has no row in the tags ledger"},
	{"unmatched", "a listed security or fund no source describes"},
	{"no_composition", "a fund nothing describes on the day, in the funds view"},
}

// exposureQualityNotes are the quality tags as an aligned list, each
// meaning wrapped beside its tag.
func exposureQualityNotes() string {
	width := 0
	for _, q := range exposureQualityTags {
		width = max(width, len(q.tag))
	}
	var b strings.Builder
	for _, q := range exposureQualityTags {
		tag := q.tag
		for _, line := range wrapWords(q.meaning, 72-4-width-2) {
			fmt.Fprintf(&b, "\n    %-*s  %s", width, tag, line)
			tag = ""
		}
	}
	return b.String()
}

// wrapWords breaks s at spaces into lines of at most width bytes; a
// longer word stands alone on its line.
func wrapWords(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) > width:
			lines = append(lines, line)
			line = w
		default:
			line += " " + w
		}
	}
	return append(lines, line)
}

func exposureUsage() string {
	cols := map[string]string{
		"classes":    joinColumnNames(buildExposureClassColumns("CCY")),
		"sectors":    joinColumnNames(buildExposureSectorColumns("CCY")),
		"sizes":      joinColumnNames(buildExposureSizeColumns("CCY")),
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
exposure.max_gap_days (default ` + fmt.Sprint(canonical.DefaultMaxGapDays) + `). Every view sums to net worth, as
'wealthdb holdings global' gives it; what a composition does not explain
is a remainder row. Read the quality column, and the coverage view,
before trusting a figure. docs/EXPOSURE.md defines every figure.

Views
  classes      one row per asset class
  sectors      one row per asset class and sector, with the share within
               the class
  sizes        one row per asset class and size (large, mid, small; for
               public equity alone), with the share within the class
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
                     column first; on classes, sectors, sizes,
                     countries, currencies and coverage
  --country C        risk (default) | domicile: the country a security
                     counts in, on countries and securities
  -f FORMAT          table | csv | csv_plain | json
  -C COLS            comma-separated names, 'default', 'all', or a
                     +ADD,-REMOVE delta on the default set
  -x CCY             output currency (default: config.default_currency)
  -p                 hide every amount; shares, names and labels stay

Notes
  share is a share of net worth, so a loan is a negative row; direct
  and via_funds split it into what is held directly and what through
  funds. A share prints with a _pct suffix: share_pct.

  quality names what a figure rests on, each tag with the share of the
  row it covers (proxy=12%):` + exposureQualityNotes() + `

  ` + strings.Join(wrapWords("A stock's size comes from its market cap (large from "+capBn(canonical.LargeCapUSD)+
		" US dollars, mid from "+capBn(canonical.MidCapUSD)+"), else from its rank in a broad fund the config names "+
		"(exposure.size_funds).", 70), "\n  ") + `

Available columns (per view):
  classes      ` + cols["classes"] + `
  sectors      ` + cols["sectors"] + `
  sizes        ` + cols["sizes"] + `
  countries    ` + cols["countries"] + `
  currencies   ` + cols["currencies"] + `
  securities   ` + cols["securities"] + `
  funds        ` + cols["funds"] + `
  coverage     ` + cols["coverage"] + `

Default column sets:
  classes      ` + strings.Join(defaultExposureClassColumns, ", ") + `
  sectors      ` + strings.Join(defaultExposureSectorColumns, ", ") + `
  sizes        ` + strings.Join(defaultExposureSizeColumns, ", ") + `
  countries    ` + strings.Join(defaultExposureCountryColumns, ", ") + `
  currencies   ` + strings.Join(defaultExposureCurrencyColumns, ", ") + `
  securities   ` + strings.Join(defaultExposureSecurityColumns, ", ") + `
  funds        ` + strings.Join(defaultExposureFundColumns, ", ") + `
  coverage     ` + strings.Join(defaultExposureCoverageColumns, ", ")
}

// capBn is a market cap in billions: 10bn.
func capBn(usd float64) string {
	return fmt.Sprintf("%gbn", usd/1e9)
}
