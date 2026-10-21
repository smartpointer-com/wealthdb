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
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// `wealthdb yield <view>` — what the holdings earn now, and how they move
// with interest rates, as of a day. docs/YIELD.md defines every figure;
// the arithmetic is internal/yield's, the inputs gold's
// (gold.YieldInputs), and this file is the views, their columns and the
// usage text.
//
// The exposure idiom: a positional view and an as-of day (-d, default
// today); --period P turns an aggregate view or curve into a series over
// a positional window (FROM [TO]) and refuses -d. --converted collapses
// the aggregate views to one row per entity in the output currency. -f
// ndjson is coverage's alone: the ledger's records still to write.
func init() {
	register("yield", cmdYield)
}

// yieldViews are the views: the grains coarse to fine, the lists, then
// the rate sensitivity and the diagnostics.
var yieldViews = []string{"global", "sources", "portfolios", "accounts", "positions", "securities", "funds", "cash",
	"curve", "coverage"}

// yieldGrains maps the aggregate views to their grain.
var yieldGrains = map[string]yield.Grain{"global": yield.GrainAll, "sources": yield.GrainSources,
	"portfolios": yield.GrainPortfolios, "accounts": yield.GrainAccounts}

// yieldSeriesViews are the views --period takes.
var yieldSeriesViews = []string{"global", "sources", "portfolios", "accounts", "curve"}

func cmdYield(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("yield", subargs, yieldViews, yieldUsage, stderr)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("wealthdb yield "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asOf := registerAsOf(fs)
	period := fs.String("period", "", strings.Join(exposurePeriods, " | ")+" — a series over a window")
	converted := fs.Bool("converted", false, "one row per entity in the output currency, rates across currencies")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, yieldUsage()) }
	if err := parseFlags(fs, rest, "yield"); err != nil {
		return err
	}
	skeletons := *rf.format == skeletonFormat
	if skeletons {
		if err := checkYieldSkeletonFlags(fs, view, *period, *rf.privacy); err != nil {
			return err
		}
		*rf.format = string(output.FormatTable)
	}
	if _, ok := yieldGrains[view]; !ok && *converted {
		return errs.Newf(errs.ExitUsage, "yield: --converted belongs to the global, sources, portfolios and accounts views")
	}
	now := time.Now()
	var series []seriesPoint
	if *period == "" {
		if err := noPositionals(fs, "yield"); err != nil {
			return err
		}
	} else {
		if err := checkChoice("yield", "period", *period, exposurePeriods...); err != nil {
			return err
		}
		if !slices.Contains(yieldSeriesViews, view) {
			return errs.Newf(errs.ExitUsage, "yield: --period belongs to the %s views", strings.Join(yieldSeriesViews, ", "))
		}
		if isSet(fs, "d", "as-of") {
			return errs.Newf(errs.ExitUsage, "yield: -d names one day; a series takes a window instead (FROM [TO])")
		}
		from, to, err := parseTrailingYearWindow(fs.Args(), now)
		if err != nil {
			fs.Usage()
			return errs.Newf(errs.ExitUsage, "yield: %s", err.Error())
		}
		series = seriesPoints(from, min(to, anchorToDay(now.UTC(), true).Unix()), *period)
		if len(series) == 0 {
			return errs.Newf(errs.ExitUsage, "yield: the window starts after today")
		}
	}
	asOfEpoch, err := parseAsOf(*asOf, now)
	if err != nil {
		return errs.Newf(errs.ExitUsage, "yield: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "yield")
	if err != nil {
		return err
	}
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	if skeletons {
		return writeYieldSkeletons(ctx, open, asOfEpoch, outCcy, stdout)
	}
	rep := yieldReport(request{view: view, currency: outCcy, asOf: asOfEpoch, series: series, converted: *converted})
	return writeReport(ctx, rep, *rf.cols, "yield", open, *rf.privacy, fmtChoice, stdout)
}

// checkYieldSkeletonFlags refuses what -f ndjson cannot honour, as
// exposure's does.
func checkYieldSkeletonFlags(fs *flag.FlagSet, view, period string, privacy bool) error {
	switch {
	case view != "coverage":
		return errs.Newf(errs.ExitUsage, "yield: -f ndjson belongs to the coverage view")
	case period != "":
		return errs.Newf(errs.ExitUsage, "yield: -f ndjson prints the records still to write on one day; it takes no --period")
	case isSet(fs, "C", "columns"):
		return errs.Newf(errs.ExitUsage, "yield: -f ndjson prints whole records; it takes no -C")
	case privacy:
		return errs.Newf(errs.ExitUsage, "yield: -f ndjson names each holding's instrument, which -p hides")
	}
	return nil
}

// yieldBook computes the book of one instant.
func yieldBook(ctx context.Context, db *sql.DB, at int64, ccy string) (*yield.Book, error) {
	in, err := gold.YieldInputs(ctx, db, at, ccy)
	if err != nil {
		return nil, err
	}
	return yield.Compute(in), nil
}

// yieldReport is one view of the yield family, the runner the CLI and the
// MCP server share.
func yieldReport(req request) *report {
	ccy := req.currency
	if grain, ok := yieldGrains[req.view]; ok {
		kindOf, loadKinds := sourceKinds()
		return exposureView(req, buildYieldRowColumns(ccy, grain, kindOf), yieldRowDefaults[grain],
			func(ctx context.Context, db *sql.DB, at int64) ([]yield.Row, error) {
				if err := loadKinds(ctx, db); err != nil {
					return nil, err
				}
				b, err := yieldBook(ctx, db, at, ccy)
				if err != nil {
					return nil, err
				}
				return b.Aggregate(grain, req.converted), nil
			})
	}
	switch req.view {
	case "positions":
		return newReport(buildYieldPositionColumns(ccy), defaultYieldPositionColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.Position, error) {
				b, err := yieldBook(ctx, db, req.asOf, ccy)
				if err != nil {
					return nil, err
				}
				return b.Positions, nil
			})
	}
	return yieldDetailReport(req)
}

// ---- column registries ---------------------------------------------------
//
// A rate prints with a _pct suffix, a value as value_<CCY>. -p hides the
// value, dv01, every income amount, a price and a key that is a
// holding's own; rates, durations, shares, labels and public keys stay
// (docs/YIELD.md §2).

// floatMoneyCol is a money figure in the output currency, two decimals,
// redacted as money.
func floatMoneyCol[T any](name, outCcy string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Header: name + "_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
		Extract: func(r T) string {
			v := get(r)
			if v == nil {
				return ""
			}
			return fmt.Sprintf("%.2f", *v)
		}}
}

// yearsCol is a figure in years, two decimals.
func yearsCol[T any](name string, get func(T) *float64) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignRight, Extract: func(r T) string {
		v := get(r)
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%.2f", *v)
	}}
}

// yieldRateCols are the figures the aggregate and position rows share,
// in the order of docs/YIELD.md §2.
func yieldRateCols[T any](outCcy string, get func(T) yieldFigures) []columnSpec[T] {
	f := func(pick func(yieldFigures) *float64) func(T) *float64 {
		return func(r T) *float64 { return pick(get(r)) }
	}
	return []columnSpec[T]{
		pctCol("income_yield", f(func(y yieldFigures) *float64 { return y.incomeYield })),
		pctCol("stated", f(func(y yieldFigures) *float64 { return y.stated })),
		pctCol("current_yield", f(func(y yieldFigures) *float64 { return y.current })),
		pctCol("implied_yield", f(func(y yieldFigures) *float64 { return y.implied })),
		pctCol("implied_yield_annual", f(func(y yieldFigures) *float64 { return y.impliedAnnual })),
		pctCol("realized_yield", f(func(y yieldFigures) *float64 { return y.realized })),
		pctCol("earnings_yield", f(func(y yieldFigures) *float64 { return y.earnings })),
		pctCol("dividend_yield", f(func(y yieldFigures) *float64 { return y.dividend })),
		pctCol("benchmark", f(func(y yieldFigures) *float64 { return y.benchmark })),
		pctCol("gap", f(func(y yieldFigures) *float64 { return y.gap })),
		yearsCol("duration", f(func(y yieldFigures) *float64 { return y.duration })),
		yearsCol("convexity", f(func(y yieldFigures) *float64 { return y.convexity })),
		floatMoneyCol("dv01", outCcy, f(func(y yieldFigures) *float64 { return y.dv01 })),
		floatMoneyCol("income_12m", outCcy, f(func(y yieldFigures) *float64 { return y.income12 })),
		floatMoneyCol("income_100bp", outCcy, f(func(y yieldFigures) *float64 { return y.income100bp })),
		yearsCol("maturity", f(func(y yieldFigures) *float64 { return y.maturity })),
	}
}

// yieldFigures are the figures a row of either shape prints.
type yieldFigures struct {
	incomeYield, stated, current, implied, impliedAnnual, realized, earnings, dividend *float64
	benchmark, gap, duration, convexity, dv01, income12, income100bp, maturity         *float64
}

func rowFigures(r yield.Row) yieldFigures {
	return yieldFigures{r.IncomeYield, r.Stated, r.CurrentYield, r.Implied, r.ImpliedAnnual, r.Realized,
		r.EarningsYield, r.DividendYield, r.Benchmark, r.Gap, r.Duration, r.Convexity, r.DV01, r.Income12,
		r.Income100bp, r.Maturity}
}

func positionFigures(p yield.Position) yieldFigures {
	var iy *float64
	if p.Income12 != nil && p.Value != 0 {
		v := *p.Income12 / p.Value * 100
		iy = &v
	}
	return yieldFigures{iy, p.Stated, p.CurrentYield, p.Implied, p.ImpliedAnnual, p.Realized, p.EarningsYield,
		p.DividendYield, p.Benchmark, p.Gap, p.Duration, p.Convexity, p.DV01, p.Income12, p.Income100bp, p.Maturity}
}

// buildYieldRowColumns is the registry of the aggregate views: the
// grain's identifying columns, the currency, then the figures.
func buildYieldRowColumns(outCcy string, grain yield.Grain, kindOf func(string) string) []columnSpec[yield.Row] {
	type row = yield.Row
	type col = columnSpec[row]
	var ids []col
	if grain != yield.GrainAll {
		ids = append(ids, col{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string {
			if r.Total {
				return "total"
			}
			return r.Source
		}})
	}
	switch grain {
	case yield.GrainPortfolios:
		ids = append(ids,
			col{Name: "portfolio", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				PrivacyFunc: func(r row) PrivacyClass {
					return portfolioNamePrivacy(kindOf)(gold.PortfolioRow{SilverSourceID: r.Source,
						PortfolioExternalID: r.Portfolio})
				},
				Extract: func(r row) string {
					switch {
					case r.Total:
						return ""
					case r.Portfolio == "":
						return "(no portfolio)"
					}
					name := r.PortfolioName
					return accountLabel(&name, r.Portfolio)
				}},
			col{Name: "portfolio_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return r.Portfolio }})
	case yield.GrainAccounts:
		ids = append(ids,
			col{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string {
				if r.Total {
					return ""
				}
				name := r.Account.Name
				return accountLabel(&name, r.AccountID)
			}},
			col{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return r.AccountID }},
			col{Name: "account_kind", Align: output.AlignLeft, Extract: func(r row) string { return r.Account.Kind }},
			col{Name: "tax_wrapper", Align: output.AlignLeft, Extract: func(r row) string { return r.Account.TaxWrapper }},
			col{Name: "account_nickname", Align: output.AlignLeft,
				Extract: func(r row) string { return r.Account.Nickname }})
	}
	ids = append(ids,
		col{Name: "currency", Align: output.AlignLeft, Extract: func(r row) string {
			switch {
			case r.Total && grain == yield.GrainAll:
				return "total"
			case r.Currency == "":
				return "(all)"
			}
			return r.Currency
		}},
		floatMoneyCol("value", outCcy, func(r row) *float64 { v := r.Value; return &v }))
	ids = append(ids, yieldRateCols(outCcy, rowFigures)...)
	return append(ids, col{Name: "quality", Align: output.AlignLeft, Extract: func(r row) string { return r.Quality }})
}

// yieldFigureDefaults are the aggregate views' default figures
// (docs/YIELD.md §2), after the identifying columns.
var yieldFigureDefaults = []string{"currency", "value", "income_yield", "implied_yield", "realized_yield", "duration",
	"dv01", "quality"}

var yieldRowDefaults = map[yield.Grain][]string{
	yield.GrainAll:        yieldFigureDefaults,
	yield.GrainSources:    append([]string{"silver_source"}, yieldFigureDefaults...),
	yield.GrainPortfolios: append([]string{"silver_source", "portfolio"}, yieldFigureDefaults...),
	yield.GrainAccounts:   append([]string{"silver_source", "account", "tax_wrapper"}, yieldFigureDefaults...),
}

// buildYieldPositionColumns is the positions view: the keys of `holdings
// positions`, then the figures, then the holding's own terms.
func buildYieldPositionColumns(outCcy string) []columnSpec[yield.Position] {
	type row = yield.Position
	type col = columnSpec[row]
	ids := []col{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string { return r.Source }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string {
			name := r.Account.Name
			return accountLabel(&name, r.HoldingKey.Account)
		}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.HoldingKey.Account }},
		{Name: "tax_wrapper", Align: output.AlignLeft, Extract: func(r row) string { return r.Account.TaxWrapper }},
		// position_key stays legible as `holdings positions` keeps it.
		{Name: "position_key", Align: output.AlignLeft, Extract: func(r row) string { return r.PositionKey }},
		{Name: "symbol", Align: output.AlignLeft, Extract: func(r row) string { return r.Symbol }},
		{Name: "name", Align: output.AlignLeft, Extract: func(r row) string { return r.Name }},
		{Name: "asset_class", Align: output.AlignLeft, Extract: func(r row) string { return r.AssetClass }},
		{Name: "vehicle", Align: output.AlignLeft, Extract: func(r row) string { return r.Vehicle }},
		{Name: "kind", Align: output.AlignLeft, Extract: func(r row) string { return string(r.Kind) }},
		{Name: "currency", Align: output.AlignLeft, Extract: func(r row) string { return r.Currency }},
		floatMoneyCol("value", outCcy, func(r row) *float64 { v := r.Value; return &v }),
	}
	ids = append(ids, yieldRateCols(outCcy, positionFigures)...)
	return append(ids,
		pctCol("coupon", func(r row) *float64 { return r.Coupon }),
		col{Name: "maturity_date", Align: output.AlignLeft, Extract: func(r row) string { return dateOrBlank(r.MaturityDate) }},
		col{Name: "price", Align: output.AlignRight, Privacy: PrivacyMoney, Extract: func(r row) string {
			if r.Price == nil {
				return ""
			}
			return fmt.Sprintf("%.4f", *r.Price)
		}},
		col{Name: "coupon_kind", Align: output.AlignLeft, Extract: func(r row) string { return string(r.CouponKind) }},
		col{Name: "rating", Align: output.AlignLeft, Extract: func(r row) string { return r.Rating }},
		pctCol("spread", func(r row) *float64 { return r.Spread }),
		col{Name: "quality", Align: output.AlignLeft, Extract: func(r row) string { return positionQuality(r) }},
	)
}

var defaultYieldPositionColumns = []string{"silver_source", "account", "symbol", "kind", "currency", "value",
	"income_yield", "implied_yield", "realized_yield", "duration", "dv01", "quality"}

// positionQuality is a holding's tags, in the order of yield.Tags.
func positionQuality(p yield.Position) string {
	var out []string
	for _, t := range yield.Tags {
		if p.Tags[t] {
			out = append(out, string(t))
		}
	}
	return strings.Join(out, " ")
}

func dateOrBlank(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.DateOnly)
}

func yieldUsage() string {
	return `usage: wealthdb yield <view> [-d DATE] [--converted] [-f FORMAT] [-C COLS] [-x CCY] [-p]
       wealthdb yield <view> --period monthly|quarterly|annual [FROM [TO]]

What the holdings earn now, and how they move with interest rates, as of
a day (-d, default today). A rate belongs to a currency: an aggregate row
is one entity in one currency, and the total row weighs the currencies by
value (quality mixed). docs/YIELD.md defines every figure.

Views
  global       one row per currency, and a total
  sources      one row per source and currency
  portfolios   one row per portfolio and currency
  accounts     one row per account and currency
  positions    one row per holding: its figures and its own terms
  securities   one row per security, held directly and through funds
  funds        one row per fund held directly: what its lines and pages say
  cash         one row per cash balance, term deposit and money-market fund
  curve        rate sensitivity (DV01) per currency and tenor
  coverage     what the figures rest on, and what is missing

Flags
  -d DATE        the day (default today)
  --period P     a series: the aggregate views and curve at the end of each
                 monthly, quarterly or annual period of FROM [TO]
  --converted    one row per entity in the output currency
  -f ndjson      coverage only: the ledger's records still to write
  -p             hide values, dv01, income amounts, prices and account ids`
}
