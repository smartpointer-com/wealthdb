package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// `wealthdb exposure <view>` — what is really owned, every fund looked
// through to what it holds, as of a day. docs/EXPOSURE.md defines every
// figure; the look-through is gold's (gold/exposure.go and
// derived/60_exposure.sql), and this file is the views, their columns
// and the usage text.
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
	// The usage lists the family's columns, so it joins the family here.
	exposureFamily.usage = exposureUsage
	register("exposure", routeReport(exposureFamily))
}

// exposureFamily are the views: the breakdowns coarse to fine, the
// legal situs, then the lists and the diagnostics. A series takes the
// aggregates and coverage: a series of securities, issuers or funds
// would be thousands of rows a period. --country belongs to the views
// that place a security in a country.
var exposureFamily = &family{
	name: "exposure",
	views: []view{
		exposureView("classes", gold.BenchmarkDimClass, true, func(req request, dimension string) *report {
			return benchmarkView(req, dimension, buildExposureClassColumns(req.currency), defaultExposureClassColumns,
				func(r gold.ExposureClassRow) string { return r.AssetClass },
				func(r gold.ExposureClassRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureClassRow, error) {
					return gold.ExposureClasses(ctx, db, at, req.currency)
				})
		}),
		exposureView("sectors", gold.BenchmarkDimClassSector, true, func(req request, dimension string) *report {
			return benchmarkView(req, dimension, buildExposureSectorColumns(req.currency), defaultExposureSectorColumns,
				func(r gold.ExposureSectorRow) string { return r.AssetClass + "|" + r.Sector },
				func(r gold.ExposureSectorRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSectorRow, error) {
					return gold.ExposureSectors(ctx, db, at, req.currency)
				})
		}),
		exposureView("sizes", gold.BenchmarkDimSize, true, func(req request, dimension string) *report {
			return benchmarkView(req, dimension, buildExposureSizeColumns(req.currency), defaultExposureSizeColumns,
				func(r gold.ExposureSizeRow) string { return r.Size },
				func(r gold.ExposureSizeRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSizeRow, error) {
					return gold.ExposureSizes(ctx, db, at, req.currency)
				})
		}),
		withTakes(exposureView("countries", gold.BenchmarkDimCountry, true, func(req request, dimension string) *report {
			return benchmarkView(req, dimension, buildExposureCountryColumns(req.currency), defaultExposureCountryColumns,
				func(r gold.ExposureCountryRow) string { return r.Country },
				func(r gold.ExposureCountryRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCountryRow, error) {
					return gold.ExposureCountries(ctx, db, at, req.currency, req.country)
				})
		}), "country"),
		exposureView("currencies", gold.BenchmarkDimCurrencyKind, true, func(req request, dimension string) *report {
			return benchmarkView(req, dimension, buildExposureCurrencyColumns(req.currency), defaultExposureCurrencyColumns,
				func(r gold.ExposureCurrencyRow) string { return r.Currency + "|" + r.Kind },
				func(r gold.ExposureCurrencyRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCurrencyRow, error) {
					return gold.ExposureCurrencies(ctx, db, at, req.currency)
				})
		}),
		exposureBucketView("credit", "credit", "bucket", gold.ExposureCredit,
			[]string{"bucket", "grade", "value", "share", "class_share", "direct", "via_funds", "quality"}),
		exposureBucketView("maturities", "maturity", "bucket", gold.ExposureMaturities,
			[]string{"bucket", "value", "share", "class_share", "direct", "via_funds", "quality"}),
		exposureBucketView("coupons", "coupon", "coupon_kind", gold.ExposureCoupons,
			[]string{"coupon_kind", "value", "share", "class_share", "direct", "via_funds", "quality"}),
		exposureBucketView("cycles", "cycle", "cycle", gold.ExposureCycles,
			[]string{"cycle", "value", "share", "class_share", "direct", "via_funds", "quality"}),
		exposureView("situs", "", true, func(req request, dimension string) *report {
			return seriesView(req, buildExposureSitusColumns(req.currency), defaultExposureSitusColumns,
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSitusRow, error) {
					return gold.ExposureSitus(ctx, db, at, req.currency)
				})
		}),
		withTakes(exposureView("securities", gold.BenchmarkDimSecurity, false, func(req request, dimension string) *report {
			kindOf, loadKinds := sourceKinds()
			return withLoads(benchmarkView(req, dimension, buildExposureSecurityColumns(req.currency, kindOf), defaultExposureSecurityColumns,
				func(r gold.ExposureSecurityRow) string { return r.Security },
				func(r gold.ExposureSecurityRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureSecurityRow, error) {
					return gold.ExposureSecurities(ctx, db, at, req.currency, req.country)
				}), loadKinds)
		}), "country"),
		exposureView("issuers", gold.BenchmarkDimIssuer, false, func(req request, dimension string) *report {
			kindOf, loadKinds := sourceKinds()
			issuers := typedIssuers{}
			return withLoads(benchmarkView(req, dimension, buildExposureIssuerColumns(req.currency, kindOf, issuers), defaultExposureIssuerColumns,
				issuerBenchmarkKey, func(r gold.ExposureIssuerRow) *float64 { return r.SharePct },
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureIssuerRow, error) {
					return gold.ExposureIssuers(ctx, db, at, req.currency)
				}), loadKinds, issuers.load)
		}),
		exposureView("funds", "", false, func(req request, dimension string) *report {
			kindOf, loadKinds := sourceKinds()
			return withLoads(seriesView(req, buildExposureFundColumns(req.currency, kindOf), defaultExposureFundColumns,
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureFundRow, error) {
					return gold.ExposureFunds(ctx, db, at, req.currency)
				}), loadKinds)
		}),
		withTotal(exposureView("coverage", "", true, func(req request, dimension string) *report {
			kindOf, loadKinds := sourceKinds()
			return withLoads(seriesView(req, buildExposureCoverageColumns(req.currency, kindOf), defaultExposureCoverageColumns,
				func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureCoverageRow, error) {
					return gold.ExposureCoverage(ctx, db, at, req.currency)
				}), loadKinds)
		}), noTotal),
	},
	options: []option{
		{name: "as_of", cli: []string{"d", "as-of"}},
		{name: "period", cli: []string{"period"}},
		{name: "country", cli: []string{"country"}, choices: exposureCountryBases,
			def: [2]string{exposureCountryBases[0], exposureCountryBases[0]}},
		{name: "benchmark", cli: []string{"benchmark"}},
	},
	window:    asOfDate,
	currency:  true,
	narrow:    "a coarser view",
	skeletons: writeDeclarationSkeletons,
	settle: func(fam *family, in optionSource, req *request, notes *[]string) error {
		req.country, req.benchmark = in.str("country"), in.str("benchmark")
		country := func() error {
			if !fam.view(req.view).takesOption("country") && in.has("country") {
				return errCountryView(fam, in.spell("country"))
			}
			return nil
		}
		// The CLI reads the series first and checks the country last;
		// MCP checks the country first.
		if in.cli() {
			if err := in.series("exposure", fam.seriesViews(), req, notes); err != nil {
				return err
			}
			if err := checkExposureBenchmark(fam, req.view, req.benchmark, req.country, req.series); err != nil {
				return err
			}
			if err := country(); err != nil {
				return err
			}
			_, err := choose(in, "exposure", "country")
			return err
		}
		if err := country(); err != nil {
			return err
		}
		if err := in.series("exposure", fam.seriesViews(), req, notes); err != nil {
			return err
		}
		return checkExposureBenchmark(fam, req.view, req.benchmark, req.country, req.series)
	},
	header: func(req request) []string {
		var h []string
		if req.country == "domicile" {
			h = append(h, "country by domicile")
		}
		if req.benchmark != "" {
			h = append(h, "benchmark="+req.benchmark)
		}
		return h
	},
}

// exposureView is an exposure view: at one instant, or, with req.series,
// at the end of every period of a window, one row set per period. Its
// rows add up as a balance and its main column is value; benchmark is
// the dimension of benchmark_exposure its rows read, and series says a
// series takes it.
func exposureView(name, benchmark string, series bool, build func(req request, benchmark string) *report) view {
	return view{name: name, series: series, benchmark: benchmark, total: balanceTotal, principal: fixedColumn("value"),
		build: func(_ *config.Config, req request, _ *toolArgs) *report { return build(req, benchmark) }}
}

// exposureBucketView is a bucket view: its benchmark dimension, the name
// of its first column, its reader and its default columns.
func exposureBucketView(name, benchmark, column string,
	read func(context.Context, *sql.DB, int64, string) ([]gold.ExposureBucketRow, error), defaults []string) view {
	return exposureView(name, benchmark, true, func(req request, dimension string) *report {
		return benchmarkView(req, dimension, buildExposureBucketColumns(req.currency, name, column), defaults,
			func(r gold.ExposureBucketRow) string { return r.Bucket },
			func(r gold.ExposureBucketRow) *float64 { return r.SharePct },
			func(ctx context.Context, db *sql.DB, at int64) ([]gold.ExposureBucketRow, error) {
				return read(ctx, db, at, req.currency)
			})
	})
}

// withTakes is v taking options.
func withTakes(v view, options ...string) view {
	v.takes = options
	return v
}

// withTotal is v adding up by rule.
func withTotal(v view, rule totalRule) view {
	v.total = rule
	return v
}

// exposureCountryBases are the bases a security's country is read on,
// the default first.
var exposureCountryBases = []string{"risk", "domicile"}

// errCountryView refuses the country basis, by the name its front-end
// gives it, on a view of fam that places no security in a country.
func errCountryView(fam *family, name string) error {
	return errs.Newf(errs.ExitUsage, "exposure: %s belongs to the %s views", name, strings.Join(fam.taking("country"), " and "))
}

// skeletonFormat is the -f value that turns coverage into declaration
// skeletons.
const skeletonFormat = "ndjson"

// checkSkeletonFlags refuses what cmd's -f ndjson cannot honour:
// another view, a series, a column set, a condition, and -p, since a
// record names its instrument.
func checkSkeletonFlags(cmd string, fs *flag.FlagSet, view, period string, privacy bool) error {
	switch {
	case view != "coverage":
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson belongs to the coverage view", cmd)
	case period != "":
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson prints the records still to write on one day; it takes no --period", cmd)
	case isSet(fs, "C", "columns"):
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson prints whole records; it takes no -C", cmd)
	case isSet(fs, "F", "filter"):
		return errs.Newf(errs.ExitUsage, "%s: -f ndjson prints every record still to write; it takes no -F", cmd)
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

// issuerBenchmarkKey is an issuer's bucket: by its LEI where it has one,
// else by its name.
func issuerBenchmarkKey(r gold.ExposureIssuerRow) string {
	if r.LEI != nil {
		return "lei:" + *r.LEI
	}
	return strings.ToUpper(r.Issuer)
}

// ---- column registries ---------------------------------------------------
//
// value prints as value_<CCY>; a share of net worth is named share and
// prints as share_pct. -p hides every amount and every instrument id a
// holding is named by, redacts a manual holding's key as an account id,
// masks an id typed into its name or an issuer named after it, and keeps
// the names, the shares and the public keys.

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

// buildExposureBucketColumns is a bucket view's registry: the bucket
// under column's name, credit's grade, the value, the shares, the share
// of what the dimension describes, and the quality.
func buildExposureBucketColumns(ccy, view, column string) []columnSpec[gold.ExposureBucketRow] {
	type row = gold.ExposureBucketRow
	cols := []columnSpec[row]{{Name: column, Align: output.AlignLeft,
		Extract: func(r row) string { return r.Bucket }}}
	if view == "credit" {
		cols = append(cols, textCol("grade", func(r row) *string { return r.Grade }))
	}
	cols = append(cols, outCcyCol("value", ccy, func(r row) *string { return r.Value }))
	cols = append(cols, shareCols(func(r row) gold.ExposureShares { return r.ExposureShares })...)
	return append(cols,
		pctCol("class_share", func(r row) *float64 { return r.ClassSharePct }),
		qualityCol(func(r row) string { return r.Quality }))
}

// typedIssuers are the issuer names a typed holding's (typedHolding) own
// name gives, upper case. An issuer neither GLEIF nor a stock names takes
// its largest leaf's own name, as typed or as exposure_issuer_name folds
// it (report_exposure_issuers), so such a name is the holder's words.
// Only gold knows them: the report runs load before any row renders
// (withLoads).
type typedIssuers map[string]bool

// load reads the names of the instruments of the sources whose kind is
// one of typedHoldingKinds.
func (t typedIssuers) load(ctx context.Context, db *sql.DB) error {
	kinds := slices.Sorted(maps.Keys(typedHoldingKinds))
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	rows, err := db.QueryContext(ctx, `SELECT i.name, COALESCE(exposure_issuer_name(i.name), '')
	  FROM instruments i JOIN silver_sources s ON s.silver_source_id = i.silver_source_id
	 WHERE i.name IS NOT NULL AND s.silver_kind IN (`+strings.Repeat("?, ", len(kinds)-1)+`?)`, args...)
	if err != nil {
		return fmt.Errorf("typed issuers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, folded string
		if err := rows.Scan(&name, &folded); err != nil {
			return fmt.Errorf("typed issuers: %w", err)
		}
		for _, n := range []string{name, folded} {
			if n != "" {
				t[strings.ToUpper(n)] = true
			}
		}
	}
	return rows.Err()
}

func buildExposureIssuerColumns(ccy string, kindOf func(string) string,
	issuers typedIssuers) []columnSpec[gold.ExposureIssuerRow] {
	type row = gold.ExposureIssuerRow
	// An issuer's name prints as it is under -p. One named after a typed
	// holding masks the ids in it, as the holding's own name does
	// (typedNameCol), and one that is a holding's key redacts as that key
	// does (holdingKeyCol).
	issuer := typedNameCol(holdingKeyCol("issuer", kindOf, func(r row) string { return r.Issuer }),
		func(r row) bool { return issuers[strings.ToUpper(r.Issuer)] })
	return []columnSpec[row]{
		issuer,
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
	keyCol, nameCol := holdingKeyCols("security", kindOf, func(r row) string { return r.Security },
		func(r row) string { return strOrEmpty(r.Name) })
	return []columnSpec[row]{
		keyCol,
		nameCol,
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
	keyCol, nameCol := holdingKeyCols("fund", kindOf, func(r row) string { return r.Fund },
		func(r row) string { return strOrEmpty(r.Name) })
	return []columnSpec[row]{
		keyCol,
		nameCol,
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
		// A holding's line names the holding here, whose ids -p masks
		// where a person typed it, as its name column does
		// (holdingNameCol).
		holdingNameCol(textCol("detail", func(r row) *string { return r.Detail }), kindOf, holding),
		// What a declaration names; -p hides the instrument id whole, and
		// redacts a typed holding's as an id.
		textCol("silver_source_id", func(r row) *string { return r.SilverSourceID }),
		holdingIDCol(columnSpec[row]{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
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
func andList(words []string) string { return joinProse(words, "and") }

// orList joins words as prose: "a, b or c".
func orList(words []string) string { return joinProse(words, "or") }

// joinProse joins words as prose, the last two by conj.
func joinProse(words []string, conj string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conj + " " + words[len(words)-1]
}

// flagDescription wraps a flag's description to the usage's description
// column.
func flagDescription(s string) string {
	return strings.Join(wrapWords(s, 52), "\n"+strings.Repeat(" ", 21))
}

func exposureUsage() string {
	available, defaults := columnSets(exposureFamily, 12, exposureFamily.names(), request{currency: "CCY"}, nil)
	return `usage: wealthdb exposure <view> [-d DATE] [--country risk|domicile] [--benchmark NAME]
                        [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]
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
               cash), with its grade, IG or HY, and the share within
               the debt and cash
  maturities   one row per time to maturity of the debt and cash
               (under 1y to over 20y, perpetual), with the share
               within them
  coupons      one row per coupon kind of the debt: fixed, floating,
               inflation_linked, zero, with the share within the debt
  cycles       one row per cycle of the public equity: cyclical,
               defensive, sensitive, with the share within it
  situs        one row per legal situs: the country whose law a holding
               is an interest under; no fund is looked through
  securities   one row per security, held directly and through every fund
  issuers      one row per issuer, its stock, bonds and deposits
               together, the largest first, with the running share
  funds        one row per fund held directly: what describes it on the
               day, and how old it is; then one row for all other
               holdings, a forward held directly among them (securities
               lists its legs)
  coverage     what the figures rest on: how each part of net worth is
               classified, how old the compositions are, the labels no
               map knows, the securities no source describes, the
               private holdings no declaration describes, how far
               each fund read from a report lies from its proxy, and
               what each situs rests on

Flags
  -d, --as-of DATE   the day (default today UTC)
  --benchmark NAME   the benchmark's weight of each row, and the row's
                     share less it, in percentage points. It reads today's
                     look-through as the last 'wealthdb web refresh' wrote
                     it. Not on situs, funds or coverage; no --period, and
                     no --country domicile.
                     'wealthdb help benchmarks' lists them
  --period P         ` + flagDescription(strings.Join(seriesPeriods, " | ")+": the view at the end of every period "+
		"of the window FROM..TO (default the last twelve months; never past today), a period column first; on "+
		andList(exposureFamily.seriesViews())) + `
  --country C        ` + flagDescription(exposureCountryBases[0]+" (default) | "+strings.Join(exposureCountryBases[1:], " | ")+
		": the country a security counts in, on "+andList(exposureFamily.taking("country"))) + `
  -f FORMAT          table | csv | csv_plain | json; on coverage also
                     ndjson: a skeleton record for each holding
                     undeclared, and a situs-only one for each of
                     unknown situs, for the declarations ledger
  -C COLS            comma-separated names, 'default', 'all', or a
                     +ADD,-REMOVE delta on the default set
  -F COND            keep the rows where COND holds; repeatable;
                     wealthdb help filter lists the operators
  -x CCY             output currency (default: config.default_currency)
  -p                 hide every amount and every instrument id a
                     holding is named by; a manual holding's key
                     redacts as an account id, and an id typed into
                     its name, or an issuer named after it, masks;
                     names, shares, labels and public keys stay

Notes
  share is a share of net worth, so a loan is a negative row; direct
  and via_funds split it into what is held directly and what through
  funds. class_share is a share of what the row's dimension describes:
  its asset class in sectors and sizes, every row but (none) in credit,
  maturities, coupons and cycles, so those rows sum to 100; it is blank
  where they sum to zero or less. A share prints with a _pct suffix:
  share_pct.

  quality names what a figure rests on, each tag with the share of the
  row it covers (proxy=12%):` + tagNotes(exposureQualityTags) + `

  situs is where a holding legally sits, not what it is exposed to: a
  fund counts whole at its own situs. Its quality column gives the share
  of the row each basis covers (isin=80%):` + tagNotes(exposureSitusBases) + `
  funds is the share of the row held in funds.

  ` + strings.Join(wrapWords("A stock's size comes from its market cap (large from "+capBn(canonical.LargeCapUSD)+
		" US dollars, mid from "+capBn(canonical.MidCapUSD)+"), else from its rank in a broad fund the config names "+
		"(exposure.size_funds).", 70), "\n  ") + `

Available columns (per view):` + available + `

Default column sets:` + defaults
}

// capBn is a market cap in billions: 10bn.
func capBn(usd float64) string {
	return fmt.Sprintf("%gbn", usd/1e9)
}
