package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

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
// [TO]); it applies to exposureSeriesViews and refuses -d. -f ndjson is
// coverage's alone: the declarations ledger's records still to fill in.
// situs reads every holding whole, at its legal situs: it looks through
// no fund.
func init() {
	register("exposure", cmdExposure)
}

// exposureViews are the views: the breakdowns coarse to fine, the legal
// situs, then the lists and the diagnostics.
var exposureViews = []string{"classes", "sectors", "sizes", "countries", "currencies", "credit", "maturities",
	"coupons", "cycles", "situs", "securities", "issuers", "funds", "coverage"}

// exposureCountryViews are the views --country applies to.
var exposureCountryViews = []string{"countries", "securities"}

// exposureCountryBases are the bases a security's country is read on,
// the default first.
var exposureCountryBases = []string{"risk", "domicile"}

// errCountryView refuses the country basis, by the name its front-end
// gives it, on a view that places no security in a country.
func errCountryView(name string) error {
	return errs.Newf(errs.ExitUsage, "exposure: %s belongs to the %s views", name, strings.Join(exposureCountryViews, " and "))
}

func cmdExposure(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("exposure", subargs, exposureViews, exposureUsage, stderr)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("wealthdb exposure "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asOf := registerAsOf(fs)
	period := fs.String("period", "", strings.Join(seriesPeriods, " | ")+" — a series over a window")
	country := fs.String("country", exposureCountryBases[0],
		strings.Join(exposureCountryBases, " | ")+" — which country a security counts in")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, exposureUsage()) }
	if err := parseFlags(fs, rest, "exposure"); err != nil {
		return err
	}
	skeletons := *rf.format == skeletonFormat
	if skeletons {
		if err := checkSkeletonFlags("exposure", fs, view, *period, *rf.privacy); err != nil {
			return err
		}
		// The shared parser knows the table formats only; the skeletons
		// are written below.
		*rf.format = string(output.FormatTable)
	}
	now := g.clock()
	series, err := parseSeries("exposure", fs, view, *period, exposureSeriesViews, now)
	if err != nil {
		return err
	}
	if !slices.Contains(exposureCountryViews, view) && isSet(fs, "country") {
		return errCountryView("--country")
	}
	if err := checkChoice("exposure", "country", *country, exposureCountryBases...); err != nil {
		return err
	}
	asOfEpoch, err := parseAsOf(*asOf, now)
	if err != nil {
		return errs.Newf(errs.ExitUsage, "exposure: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "exposure")
	if err != nil {
		return err
	}
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	if skeletons {
		return writeDeclarationSkeletons(ctx, open, asOfEpoch, outCcy, stdout)
	}
	rep := exposureReport(request{view: view, currency: outCcy, asOf: asOfEpoch, country: *country, series: series})
	return writeReport(ctx, rep, *rf.cols, "exposure", open, *rf.privacy, fmtChoice, stdout)
}

// skeletonFormat is the -f value that turns coverage into declaration
// skeletons.
const skeletonFormat = "ndjson"

// checkSkeletonFlags refuses what cmd's -f ndjson cannot honour:
// another view, a series, a column set, and -p, since a record names its
// instrument.
func checkSkeletonFlags(cmd string, fs *flag.FlagSet, view, period string, privacy bool) error {
	switch {
	case view != "coverage":
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson belongs to the coverage view", cmd)
	case period != "":
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson prints the records still to write on one day; it takes no --period", cmd)
	case isSet(fs, "C", "columns"):
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson prints whole records; it takes no -C", cmd)
	case privacy:
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson names each holding's instrument, which -p hides", cmd)
	}
	return nil
}

// ndjsonWriter writes records one JSON line each, as typed by hand:
// <, > and & as they are. A record written already is not written again.
type ndjsonWriter struct {
	w    io.Writer
	seen map[string]bool
}

func newNDJSONWriter(w io.Writer) *ndjsonWriter { return &ndjsonWriter{w: w, seen: map[string]bool{}} }

func (n *ndjsonWriter) write(rec any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return err
	}
	if n.seen[b.String()] {
		return nil
	}
	n.seen[b.String()] = true
	_, err := n.w.Write(b.Bytes())
	return err
}

// declarationSkeleton is a record of the declarations ledger with its
// holding named and its dimensions empty. An empty dimension is refused
// at load, so a skeleton loads only once it is filled in or the
// dimension is taken out.
type declarationSkeleton struct {
	SilverSourceID string          `json:"silver_source_id"`
	Instrument     string          `json:"instrument"`
	Country        json.RawMessage `json:"country,omitempty"`
	Currency       json.RawMessage `json:"currency,omitempty"`
	Sector         json.RawMessage `json:"sector,omitempty"`
	Situs          json.RawMessage `json:"situs,omitempty"`
	Note           string          `json:"note,omitempty"`
}

// writeDeclarationSkeletons writes a skeleton for each holding coverage
// lists as undeclared, with country, currency and sector, and one for
// each of unknown situs, with situs alone. Its note is the
// holding's name. A situs-only record leaves a holding's other records
// as they are, so each skeleton loads beside them. A holding with no
// instrument, or no value, takes no record.
func writeDeclarationSkeletons(ctx context.Context, open func() (*sql.DB, error), asOf int64, ccy string,
	w io.Writer) error {
	db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := gold.ExposureCoverage(ctx, db, asOf, ccy)
	if err != nil {
		return err
	}
	out := newNDJSONWriter(w)
	empty := json.RawMessage(`{}`)
	type written struct{ section, src, iid string }
	seen := map[written]bool{}
	for _, r := range rows {
		if (r.Section != "undeclared" && r.Section != "situs") || strOrEmpty(r.SilverSourceID) == "" ||
			strOrEmpty(r.Instrument) == "" {
			continue
		}
		k := written{r.Section, *r.SilverSourceID, *r.Instrument}
		if seen[k] {
			continue
		}
		seen[k] = true
		s := declarationSkeleton{SilverSourceID: k.src, Instrument: k.iid, Note: strOrEmpty(r.Detail)}
		if r.Section == "undeclared" {
			s.Country, s.Currency, s.Sector = empty, empty, empty
		} else {
			s.Situs = empty
		}
		if err := out.write(s); err != nil {
			return err
		}
	}
	return nil
}

// exposureReport is one view of the exposure family, the runner the CLI
// and the MCP server share: at one instant, or, with req.series, at the
// end of every period of a window, one row set per period.
func exposureReport(req request) *report {
	ccy, country := req.currency, req.country
	switch req.view {
	case "classes":
		return seriesView(req, buildExposureClassColumns(ccy), defaultExposureClassColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureClassRow, error) {
				return gold.ExposureClasses(ctx, db, at, ccy)
			})
	case "sectors":
		return seriesView(req, buildExposureSectorColumns(ccy), defaultExposureSectorColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSectorRow, error) {
				return gold.ExposureSectors(ctx, db, at, ccy)
			})
	case "sizes":
		return seriesView(req, buildExposureSizeColumns(ccy), defaultExposureSizeColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSizeRow, error) {
				return gold.ExposureSizes(ctx, db, at, ccy)
			})
	case "countries":
		return seriesView(req, buildExposureCountryColumns(ccy), defaultExposureCountryColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCountryRow, error) {
				return gold.ExposureCountries(ctx, db, at, ccy, country)
			})
	case "currencies":
		return seriesView(req, buildExposureCurrencyColumns(ccy), defaultExposureCurrencyColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCurrencyRow, error) {
				return gold.ExposureCurrencies(ctx, db, at, ccy)
			})
	case "credit", "maturities", "coupons", "cycles":
		v := exposureBucketViews[req.view]
		return seriesView(req, buildExposureBucketColumns(ccy, req.view), v.defaults,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureBucketRow, error) {
				return v.read(ctx, db, at, ccy)
			})
	case "issuers":
		kindOf, loadKinds := sourceKinds()
		issuers := typedIssuers{}
		return seriesView(req, buildExposureIssuerColumns(ccy, kindOf, issuers), defaultExposureIssuerColumns,
			loadFirstAt(loadKinds, loadFirstAt(issuers.load(kindOf),
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureIssuerRow, error) {
					return gold.ExposureIssuers(ctx, db, at, ccy)
				})))
	case "situs":
		return seriesView(req, buildExposureSitusColumns(ccy), defaultExposureSitusColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSitusRow, error) {
				return gold.ExposureSitus(ctx, db, at, ccy)
			})
	case "securities":
		kindOf, loadKinds := sourceKinds()
		return seriesView(req, buildExposureSecurityColumns(ccy, kindOf), defaultExposureSecurityColumns,
			loadFirstAt(loadKinds, func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSecurityRow, error) {
				return gold.ExposureSecurities(ctx, db, at, ccy, country)
			}))
	case "funds":
		kindOf, loadKinds := sourceKinds()
		return seriesView(req, buildExposureFundColumns(ccy, kindOf), defaultExposureFundColumns,
			loadFirstAt(loadKinds, func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureFundRow, error) {
				return gold.ExposureFunds(ctx, db, at, ccy)
			}))
	default: // coverage
		kindOf, loadKinds := sourceKinds()
		return seriesView(req, buildExposureCoverageColumns(ccy, kindOf), defaultExposureCoverageColumns,
			loadFirstAt(loadKinds, func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCoverageRow, error) {
				return gold.ExposureCoverage(ctx, db, at, ccy)
			}))
	}
}

// exposureSeriesViews are the views a series takes: the aggregates and
// coverage. A series of securities, issuers or funds would be thousands
// of rows a period.
var exposureSeriesViews = []string{"classes", "sectors", "sizes", "countries", "currencies", "credit", "maturities",
	"coupons", "cycles", "situs", "coverage"}

// ---- column registries ---------------------------------------------------
//
// value prints as value_<CCY>; a share of net worth is named share and
// prints as share_pct. -p hides every amount and every instrument id a
// holding is named by, and keeps the shares and the public keys.

// shareCols are the share columns of the aggregate views.
func shareCols[T any](get func(T) gold.ExposureShares) []columnSpec[T] {
	return []columnSpec[T]{
		pctCol("share", func(r T) *float64 { return get(r).SharePct }),
		pctCol("direct", func(r T) *float64 { return get(r).DirectPct }),
		pctCol("via_funds", func(r T) *float64 { return get(r).ViaFundsPct }),
	}
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

// exposureBucketViews are the bucket views: the name of each one's
// first column, its reader and its default columns.
var exposureBucketViews = map[string]struct {
	column   string
	read     func(context.Context, *sql.DB, int64, string) ([]gold.ExposureBucketRow, error)
	defaults []string
}{
	"credit":     {"bucket", gold.ExposureCredit, []string{"bucket", "grade", "value", "share", "direct", "via_funds", "quality"}},
	"maturities": {"bucket", gold.ExposureMaturities, []string{"bucket", "value", "share", "direct", "via_funds", "quality"}},
	"coupons":    {"coupon_kind", gold.ExposureCoupons, []string{"coupon_kind", "value", "share", "direct", "via_funds", "quality"}},
	"cycles":     {"cycle", gold.ExposureCycles, []string{"cycle", "value", "share", "direct", "via_funds", "quality"}},
}

// buildExposureBucketColumns is a bucket view's registry: the bucket,
// credit's grade, the value, the shares and the quality.
func buildExposureBucketColumns(ccy, view string) []columnSpec[gold.ExposureBucketRow] {
	type row = gold.ExposureBucketRow
	cols := []columnSpec[row]{{Name: exposureBucketViews[view].column, Align: output.AlignLeft,
		Extract: func(r row) string { return r.Bucket }}}
	if view == "credit" {
		cols = append(cols, textCol("grade", func(r row) *string { return r.Grade }))
	}
	cols = append(cols, outCcyCol("value", ccy, func(r row) *string { return r.Value }))
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols, qualityCol(func(r row) string { return r.Quality }))
}

// typedIssuers are the issuer names a typed holding's (typedHolding) own
// name gives, upper case, each with the holding's source and instrument
// id. An issuer neither GLEIF nor a stock names takes its largest leaf's
// own name, as typed or as exposure_issuer_name folds it
// (report_exposure_issuers), so such a name is the holder's words. Only
// gold knows them: the report's fetch calls load before any row renders.
type typedIssuers map[string][2]string

// load reads the names of the typed sources' instruments, once kindOf
// answers.
func (t typedIssuers) load(kindOf func(string) string) func(context.Context, *sql.DB) error {
	return func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `SELECT silver_source_id, instrument_external_id, name,
		       COALESCE(exposure_issuer_name(name), '') FROM instruments WHERE name IS NOT NULL`)
		if err != nil {
			return fmt.Errorf("typed issuers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var source, id, name, folded string
			if err := rows.Scan(&source, &id, &name, &folded); err != nil {
				return fmt.Errorf("typed issuers: %w", err)
			}
			if !typedHolding(kindOf, source) {
				continue
			}
			for _, n := range []string{name, folded} {
				if n != "" {
					t[strings.ToUpper(n)] = [2]string{source, id}
				}
			}
		}
		return rows.Err()
	}
}

// of is the typed holding an issuer's name is the words of, ok false for
// any other.
func (t typedIssuers) of(issuer string) (source, id string, ok bool) {
	h, ok := t[strings.ToUpper(issuer)]
	return h[0], h[1], ok
}

func buildExposureIssuerColumns(ccy string, kindOf func(string) string,
	issuers typedIssuers) []columnSpec[gold.ExposureIssuerRow] {
	type row = gold.ExposureIssuerRow
	return []columnSpec[row]{
		// An issuer a typed holding names prints as that holding's
		// pseudonym, as its key does in the other views.
		{Name: "issuer", Align: output.AlignLeft, Extract: func(r row) string { return r.Issuer },
			PrivacyFunc: func(r row) PrivacyClass {
				if _, _, ok := issuers.of(r.Issuer); ok {
					return PrivacyPseudonym
				}
				return PrivacyNone
			},
			PrivacyExtract: func(r row) string {
				if _, id, ok := issuers.of(r.Issuer); ok {
					return id
				}
				return r.Issuer
			},
			Pseudonym: func(r row) (string, string) {
				source, _, _ := issuers.of(r.Issuer)
				return typedPseudonym(kindOf, source)
			}},
		textCol("lei", func(r row) *string { return r.LEI }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		pctCol("cumulative_share", func(r row) *float64 { return r.CumulativeSharePct }),
		pctCol("equity", func(r row) *float64 { return r.EquityPct }),
		pctCol("debt", func(r row) *float64 { return r.DebtPct }),
		pctCol("cash", func(r row) *float64 { return r.CashPct }),
		countCol("securities", func(r row) int64 { return r.Securities }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureIssuerColumns = []string{"issuer", "value", "share", "cumulative_share", "equity", "debt", "cash",
	"quality"}

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

// buildExposureSitusColumns: no direct or via_funds, since no fund is
// looked through; funds is the share of the row held in funds.
func buildExposureSitusColumns(ccy string) []columnSpec[gold.ExposureSitusRow] {
	type row = gold.ExposureSitusRow
	return []columnSpec[row]{
		{Name: "situs", Align: output.AlignLeft, Extract: func(r row) string { return r.Situs }},
		textCol("country_name", func(r row) *string { return r.CountryName }),
		textCol("region", func(r row) *string { return r.Region }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		pctCol("funds", func(r row) *float64 { return r.FundsPct }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureSitusColumns = []string{"situs", "country_name", "region", "value", "share", "funds", "quality"}

func buildExposureSecurityColumns(ccy string, kindOf func(string) string) []columnSpec[gold.ExposureSecurityRow] {
	type row = gold.ExposureSecurityRow
	key := func(r row) string { return r.Security }
	return []columnSpec[row]{
		holdingKeyCol("security", kindOf, key),
		holdingKeyName(kindOf, key, func(r row) string { return strOrEmpty(r.Name) }),
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

func buildExposureFundColumns(ccy string, kindOf func(string) string) []columnSpec[gold.ExposureFundRow] {
	type row = gold.ExposureFundRow
	key := func(r row) string { return r.Fund }
	return []columnSpec[row]{
		holdingKeyCol("fund", kindOf, key),
		holdingKeyName(kindOf, key, func(r row) string { return strOrEmpty(r.Name) }),
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
		intPtrCol("gap_days", func(r row) *int64 { return r.GapDays }),
		intPtrCol("lines", func(r row) *int64 { return r.Lines }),
		pctCol("weight_sum", func(r row) *float64 { return r.WeightSum }),
		textCol("domicile", func(r row) *string { return r.Domicile }),
		textCol("situs", func(r row) *string { return r.Situs }),
		textCol("situs_basis", func(r row) *string { return r.SitusBasis }),
		textCol("replication", func(r row) *string { return r.Replication }),
		yesNoCol("lending", func(r row) *bool { return r.Lending }),
		textCol("hedged_currency", func(r row) *string { return r.HedgedCurrency }),
		datePtrCol("declaration_date", func(r row) *int64 { return r.DeclarationAsOf }),
		textCol("declared", func(r row) *string { return r.DeclaredDimensions }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultExposureFundColumns = []string{"fund", "name", "value", "share", "via", "proxy", "provider",
	"composition_date", "gap_days", "lines", "weight_sum", "declaration_date", "declared", "quality"}

func buildExposureCoverageColumns(ccy string, kindOf func(string) string) []columnSpec[gold.ExposureCoverageRow] {
	type row = gold.ExposureCoverageRow
	holding := func(r row) (string, string) { return strOrEmpty(r.SilverSourceID), strOrEmpty(r.Instrument) }
	return []columnSpec[row]{
		{Name: "section", Align: output.AlignLeft, Extract: func(r row) string { return r.Section }},
		// An unmatched holding no key names reads as its instrument id.
		holdingKeyCol("reason", kindOf, func(r row) string { return r.Reason }),
		countCol("items", func(r row) int64 { return r.Items }),
		outCcyCol("value", ccy, func(r row) *string { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		// A holding's line names the holding here, which -p prints as
		// its key's pseudonym where a person typed it.
		typedHoldingCol(textCol("detail", func(r row) *string { return r.Detail }), kindOf, holding),
		// What a declaration names; -p hides the instrument id whole, and
		// prints a typed holding's as its pseudonym.
		textCol("silver_source_id", func(r row) *string { return r.SilverSourceID }),
		typedHoldingCol(columnSpec[row]{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r row) string { return strOrEmpty(r.Instrument) }}, kindOf, holding),
	}
}

var defaultExposureCoverageColumns = []string{"section", "reason", "items", "value", "share", "detail",
	"silver_source_id", "instrument"}

// exposureQualityTags are the tags of the exposure quality column and
// what each means: the CLI usage and the MCP help both render them.
var exposureQualityTags = []tagNote{
	{"proxy", "another fund's composition stands in"},
	{"declared", "a declaration states it"},
	{"aged", fmt.Sprintf("the composition is older than %d days", canonical.DefaultAgedDays)},
	{"forward", "the composition is dated after the day"},
	{"partial", fmt.Sprintf("the weights leave more than %g%% unexplained, a forward states no legs, "+
		"or a fund lies deeper than three levels", canonical.DefaultPartialPct)},
	{"unmapped", "a provider's label has no place in the vocabulary"},
	{"by_name", "a receipt's or a report line's sector or country comes from a share matched by name " +
		"among all shares"},
	{"opaque", "a private equity fund nothing looks through; it reads diversified"},
	{"undeclared", "a private holding has no declaration of its exposure"},
	{"unmatched", "a listed security or fund no source describes"},
	{"no_composition", "a fund no composition and no declaration describes on the day, in the funds view"},
	{"unknown", "in the credit view: debt no rating, rating fund or known label of its fund's page describes"},
}

// exposureSitusBases are what a holding's legal situs rests on, in the
// rule's order (docs/EXPOSURE.md §13): the situs view's quality column
// gives each one's share. The CLI usage and the MCP help both render
// them.
var exposureSitusBases = []tagNote{
	{"declared", "a declaration states it"},
	{"underlying", "a depositary receipt reads as the share it stands for"},
	{"isin", "the country of the holding's ISIN prefix"},
	{"domicile", "a fund's domicile (the US for one that files N-PORT) or a security's, where it has no ISIN " +
		"of a country"},
	{"custody", "cash or metal sits in its custodian's country (exposure.custody)"},
	{"country", "the one country a declaration states for a private holding"},
	{"unknown", "none of these: a coin, cash at a custodian with no country, or a holding to declare"},
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

// andList joins words as prose: "a, b and c".
func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// flagDescription wraps a flag's description to the usage's description
// column.
func flagDescription(s string) string {
	return strings.Join(wrapWords(s, 52), "\n"+strings.Repeat(" ", 21))
}

func exposureUsage() string {
	cols := map[string]string{
		"classes":    joinColumnNames(buildExposureClassColumns("CCY")),
		"sectors":    joinColumnNames(buildExposureSectorColumns("CCY")),
		"sizes":      joinColumnNames(buildExposureSizeColumns("CCY")),
		"countries":  joinColumnNames(buildExposureCountryColumns("CCY")),
		"currencies": joinColumnNames(buildExposureCurrencyColumns("CCY")),
		"credit":     joinColumnNames(buildExposureBucketColumns("CCY", "credit")),
		"maturities": joinColumnNames(buildExposureBucketColumns("CCY", "maturities")),
		"coupons":    joinColumnNames(buildExposureBucketColumns("CCY", "coupons")),
		"cycles":     joinColumnNames(buildExposureBucketColumns("CCY", "cycles")),
		"issuers":    joinColumnNames(buildExposureIssuerColumns("CCY", nil, nil)),
		"situs":      joinColumnNames(buildExposureSitusColumns("CCY")),
		"securities": joinColumnNames(buildExposureSecurityColumns("CCY", nil)),
		"funds":      joinColumnNames(buildExposureFundColumns("CCY", nil)),
		"coverage":   joinColumnNames(buildExposureCoverageColumns("CCY", nil)),
	}
	return `usage: wealthdb exposure <view> [-d DATE] [--country risk|domicile]
                        [-f FORMAT] [-C COLS] [-x CCY] [-p]
       wealthdb exposure <view> --period ` + strings.Join(seriesPeriods, "|") + ` [FROM [TO]]

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
  credit       one row per credit bucket (AAA to B and below, unrated,
               cash), with its grade, IG or HY
  maturities   one row per time to maturity of the debt and term cash
               (under 1y to over 20y, perpetual)
  coupons      one row per coupon kind of the debt: fixed, floating,
               inflation_linked, zero
  cycles       one row per cycle of the public equity: cyclical,
               defensive, sensitive
  situs        one row per legal situs: the country whose law a holding
               is an interest under; no fund is looked through
  securities   one row per security, held directly and through every fund
  issuers      one row per issuer, its stock, bonds and deposits
               together, the largest first, with the running share
  funds        one row per fund held directly: what describes it on the
               day, and how old it is; then one row for all other
               holdings
  coverage     what the figures rest on: how each part of net worth is
               classified, how old the compositions are, the labels no
               map knows, the securities no source describes, the
               private holdings no declaration describes, how far
               each fund read from a report lies from its proxy, and
               what each situs rests on

Flags
  -d, --as-of DATE   the day (default today UTC)
  --period P         ` + flagDescription(strings.Join(seriesPeriods, " | ")+": the view at the end of every period "+
		"of the window FROM..TO (default the last twelve months; never past today), a period column first; on "+
		andList(exposureSeriesViews)) + `
  --country C        ` + flagDescription(exposureCountryBases[0]+" (default) | "+strings.Join(exposureCountryBases[1:], " | ")+
		": the country a security counts in, on "+andList(exposureCountryViews)) + `
  -f FORMAT          table | csv | csv_plain | json; on coverage also
                     ndjson: a skeleton record for each holding
                     undeclared, and a situs-only one for each of
                     unknown situs, for the declarations ledger
  -C COLS            comma-separated names, 'default', 'all', or a
                     +ADD,-REMOVE delta on the default set
  -x CCY             output currency (default: config.default_currency)
  -p                 hide every amount and every instrument id a
                     holding is named by; a manual holding's key and
                     name, and an issuer named after it, print as a
                     pseudonym, such as manual-1; shares, other names,
                     labels and public keys stay

Notes
  share is a share of net worth, so a loan is a negative row; direct
  and via_funds split it into what is held directly and what through
  funds. A share prints with a _pct suffix: share_pct.

  quality names what a figure rests on, each tag with the share of the
  row it covers (proxy=12%):` + tagNotes(exposureQualityTags) + `

  situs is where a holding legally sits, not what it is exposed to: a
  fund counts whole at its own situs. Its quality column gives the share
  of the row each basis covers (isin=80%):` + tagNotes(exposureSitusBases) + `
  funds is the share of the row held in funds.

  ` + strings.Join(wrapWords("A stock's size comes from its market cap (large from "+capBn(canonical.LargeCapUSD)+
		" US dollars, mid from "+capBn(canonical.MidCapUSD)+"), else from its rank in a broad fund the config names "+
		"(exposure.size_funds).", 70), "\n  ") + `

Available columns (per view):
  classes      ` + cols["classes"] + `
  sectors      ` + cols["sectors"] + `
  sizes        ` + cols["sizes"] + `
  countries    ` + cols["countries"] + `
  currencies   ` + cols["currencies"] + `
  credit       ` + cols["credit"] + `
  maturities   ` + cols["maturities"] + `
  coupons      ` + cols["coupons"] + `
  cycles       ` + cols["cycles"] + `
  situs        ` + cols["situs"] + `
  securities   ` + cols["securities"] + `
  issuers      ` + cols["issuers"] + `
  funds        ` + cols["funds"] + `
  coverage     ` + cols["coverage"] + `

Default column sets:
  classes      ` + strings.Join(defaultExposureClassColumns, ", ") + `
  sectors      ` + strings.Join(defaultExposureSectorColumns, ", ") + `
  sizes        ` + strings.Join(defaultExposureSizeColumns, ", ") + `
  countries    ` + strings.Join(defaultExposureCountryColumns, ", ") + `
  currencies   ` + strings.Join(defaultExposureCurrencyColumns, ", ") + `
  credit       ` + strings.Join(exposureBucketViews["credit"].defaults, ", ") + `
  maturities   ` + strings.Join(exposureBucketViews["maturities"].defaults, ", ") + `
  coupons      ` + strings.Join(exposureBucketViews["coupons"].defaults, ", ") + `
  cycles       ` + strings.Join(exposureBucketViews["cycles"].defaults, ", ") + `
  situs        ` + strings.Join(defaultExposureSitusColumns, ", ") + `
  securities   ` + strings.Join(defaultExposureSecurityColumns, ", ") + `
  issuers      ` + strings.Join(defaultExposureIssuerColumns, ", ") + `
  funds        ` + strings.Join(defaultExposureFundColumns, ", ") + `
  coverage     ` + strings.Join(defaultExposureCoverageColumns, ", ")
}

// capBn is a market cap in billions: 10bn.
func capBn(usd float64) string {
	return fmt.Sprintf("%gbn", usd/1e9)
}
