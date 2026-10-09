package main

import (
	"context"
	"database/sql"
	"errors"
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

// `wealthdb gains <view>` — what was gained or lost, realized and
// unrealized, over a window. docs/GAINS.md defines every figure; the
// sums are gold's (migration 0116), and this file is the views, their
// columns and the usage text.
//
// The families' idiom: the grain in a positional view, the window
// positional with the trailing twelve months as its default, --period
// on the four aggregate views and ignored by the others, as the
// families' transactions views ignore it. Two flags belong to the
// `realized` view alone, --documents and -r, and are refused on every
// other view rather than ignored: on another view they would ask a
// question it cannot answer.
func init() {
	register("gains", cmdGains)
}

// gainsViews are the views, coarse to fine.
var gainsViews = []string{"summary", "sources", "portfolios", "accounts", "positions", "realized", "lots", "coverage"}

// gainsGrains maps the four aggregate views to their grain.
var gainsGrains = map[string]gold.GainsGrain{
	"summary": gold.GainsAll, "sources": gold.GainsSources,
	"portfolios": gold.GainsPortfolios, "accounts": gold.GainsAccounts,
}

// gainsValueFlags are reportValueFlags plus --documents.
var gainsValueFlags = func() map[string]bool {
	m := map[string]bool{"--documents": true}
	for k, v := range reportValueFlags {
		m[k] = v
	}
	return m
}()

func cmdGains(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		fmt.Fprintln(stderr, gainsUsage())
		return errs.Newf(2, "gains: a view subcommand is required")
	}
	view, rest := subargs[0], subargs[1:]
	switch view {
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, gainsUsage())
		return nil
	}
	if !slices.Contains(gainsViews, view) {
		fmt.Fprintln(stderr, gainsUsage())
		return errs.Newf(2, "gains: unknown view %q (want %s)", view, strings.Join(gainsViews, " | "))
	}
	return runGainsView(ctx, g, view, rest, stdout, stderr)
}

func runGainsView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb gains "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)

	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	documents := fs.String("documents", "primary", "primary | all — which copies of a sale the realized view lists")
	reverse := fs.Bool("r", false, "realized only: newest first")
	fs.BoolVar(reverse, "reverse", false, "realized only: newest first")
	rf := registerReportFlags(fs, "redact account ids, quantities and monetary amounts (percentages, stamps, dates and security names stay visible)")

	fs.Usage = func() { fmt.Fprintln(stderr, gainsUsage()) }
	if err := fs.Parse(reorderFlagsFirst(splitFusedColumnsFlag(args), gainsValueFlags)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "gains: bad flags")
	}
	if view != "realized" && isSet(fs, "documents", "r", "reverse") {
		return errs.Newf(2, "gains: --documents and -r belong to the realized view")
	}
	if _, ok := reportPeriods[*period]; !ok {
		return errs.Newf(2, "gains: invalid --period %q (want %s)", *period, strings.Join(reportPeriodNames, " | "))
	}
	if !oneOf(*documents, "primary", "all") {
		return errs.Newf(2, "gains: invalid --documents %q (want primary | all)", *documents)
	}
	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "gains: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "gains")
	if err != nil {
		return err
	}

	rep := gainsReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch,
		period: *period, allDocuments: *documents == "all", newestFirst: *reverse})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "gains", open, *rf.privacy, fmtChoice, stdout)
}

// gainsReport is one view of the gains family, the runner the CLI and
// the MCP server share.
func gainsReport(req request) *report {
	ccy := req.currency
	if grain, ok := gainsGrains[req.view]; ok {
		kindOf, loadKinds := sourceKinds()
		return newReport(buildGainsBucketColumnRegistry(ccy, req.period, grain, kindOf), gainsBucketDefaults[grain],
			func(ctx context.Context, db *sql.DB) ([]gold.GainsBucketRow, error) {
				if grain == gold.GainsPortfolios {
					if err := loadKinds(ctx, db); err != nil {
						return nil, err
					}
				}
				return gold.GainsBuckets(ctx, db, req.from, req.to, ccy, reportPeriods[req.period], grain)
			})
	}
	switch req.view {
	case "positions":
		return newReport(buildGainsPositionColumnRegistry(ccy), defaultGainsPositionColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.GainsPositionRow, error) {
				return gold.GainsPositions(ctx, db, req.from, req.to, ccy)
			})
	case "realized":
		order := gold.SortAscending
		if req.newestFirst {
			order = gold.SortDescending
		}
		return newReport(buildRealizedLotColumnRegistry(ccy), defaultRealizedLotColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.RealizedLotRow, error) {
				return gold.RealizedLotsBetween(ctx, db, req.from, req.to, ccy, req.allDocuments, order)
			})
	case "lots":
		return newReport(buildOpenLotColumnRegistry(ccy), defaultOpenLotColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.OpenLotRow, error) {
				return gold.OpenLotsAsOf(ctx, db, req.to, ccy)
			})
	default: // coverage
		return newReport(buildGainsCoverageColumnRegistry(ccy), defaultGainsCoverageColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.GainsCoverageRow, error) {
				return gold.GainsCoverage(ctx, db, req.from, req.to, ccy)
			})
	}
}

// ---- column registries ---------------------------------------------------
//
// A money column in the holding's own currency has a twin in the output
// currency named <name>_outccy, its header <name>_<CCY>; value is
// market_value's twin, as on holdings. A figure only the output
// currency carries (the window's gains) has no twin and keeps its own
// name, its header taking the currency suffix.

// yesNo renders a stated flag, empty where the source states nothing.
func yesNo(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "yes"
	default:
		return "no"
	}
}

func intOrEmpty(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%d", *p)
}

// buildGainsBucketColumnRegistry is the registry of the four aggregate
// views: the grain's identifying columns, then the figures they share.
func buildGainsBucketColumnRegistry(outCcy, period string, grain gold.GainsGrain, kindOf func(string) string) []columnSpec[gold.GainsBucketRow] {
	type row = gold.GainsBucketRow
	type col = columnSpec[row]
	var ids []col
	if grain != gold.GainsAll {
		ids = append(ids, col{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r row) string { return strOrEmpty(r.SilverSourceID) }})
	}
	switch grain {
	case gold.GainsPortfolios:
		ids = append(ids,
			col{Name: "portfolio", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				PrivacyFunc: func(r row) PrivacyClass {
					return portfolioNamePrivacy(kindOf)(gold.PortfolioRow{
						SilverSourceID: strOrEmpty(r.SilverSourceID), PortfolioExternalID: strOrEmpty(r.PortfolioExternalID)})
				},
				Extract: func(r row) string {
					if strOrEmpty(r.PortfolioExternalID) == "" {
						return "(no portfolio)"
					}
					return accountLabel(r.PortfolioDisplayName, *r.PortfolioExternalID)
				}},
			col{Name: "portfolio_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return strOrEmpty(r.PortfolioExternalID) }})
	case gold.GainsAccounts:
		ids = append(ids,
			col{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return accountLabel(r.DisplayName, strOrEmpty(r.AccountExternalID)) }},
			col{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return strOrEmpty(r.AccountExternalID) }},
			textCol("account_kind", func(r row) *string { return r.AccountKind }),
			textCol("tax_wrapper", func(r row) *string { return r.TaxWrapper }),
			textCol("account_nickname", func(r row) *string { return r.Nickname }),
			textCol("account_category", func(r row) *string { return r.AccountCategory }),
			col{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return strOrEmpty(r.RelationshipID) }})
	}
	return append(ids,
		col{Name: "period", Align: output.AlignLeft,
			Extract: func(r row) string { return periodLabel(r.PeriodStart, period) }},
		col{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r row) string { return periodStart(r.PeriodStart) }},
		outCcyCol("realized", outCcy, func(r row) *string { return r.Realized }),
		outCcyCol("realized_short", outCcy, func(r row) *string { return r.RealizedShort }),
		outCcyCol("realized_long", outCcy, func(r row) *string { return r.RealizedLong }),
		outCcyCol("realized_other", outCcy, func(r row) *string { return r.RealizedOther }),
		outCcyCol("unrealized_start", outCcy, func(r row) *string { return r.UnrealizedStart }),
		outCcyCol("unrealized_end", outCcy, func(r row) *string { return r.UnrealizedEnd }),
		outCcyCol("unrealized_change", outCcy, func(r row) *string { return r.UnrealizedChange }),
		outCcyCol("gain", outCcy, func(r row) *string { return r.Gain }),
		outCcyCol("proceeds", outCcy, func(r row) *string { return r.Proceeds }),
		outCcyCol("wash_disallowed", outCcy, func(r row) *string { return r.WashDisallowed }),
		countCol("realized_lots", func(r row) int64 { return r.RealizedLots }),
		countCol("sells", func(r row) int64 { return r.Sells }),
		countCol("positions", func(r row) int64 { return r.Positions }),
		countCol("positions_without_basis", func(r row) int64 { return r.PositionsWithoutBasis }),
		col{Name: "basis_coverage", Header: "basis_coverage_pct", Align: output.AlignRight,
			Extract: func(r row) string { return formatPctOrBlank(r.BasisCoverage) }},
		col{Name: "quality", Align: output.AlignLeft, Extract: func(r row) string { return r.Quality }},
	)
}

// gainsBucketFigures are the aggregate views' default figures, after
// the grain's identifying columns.
var gainsBucketFigures = []string{"period", "realized", "unrealized_start", "unrealized_end",
	"unrealized_change", "gain", "basis_coverage", "quality"}

var gainsBucketDefaults = map[gold.GainsGrain][]string{
	gold.GainsAll:        gainsBucketFigures,
	gold.GainsSources:    append([]string{"silver_source"}, gainsBucketFigures...),
	gold.GainsPortfolios: append([]string{"silver_source", "portfolio"}, gainsBucketFigures...),
	gold.GainsAccounts: {"silver_source", "account", "tax_wrapper", "period", "realized",
		"unrealized_end", "unrealized_change", "gain", "basis_coverage", "quality"},
}

func buildGainsPositionColumnRegistry(outCcy string) []columnSpec[gold.GainsPositionRow] {
	type row = gold.GainsPositionRow
	type col = columnSpec[row]
	return []col{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string { return r.SilverSourceID }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.AccountExternalID }},
		textCol("account_nickname", func(r row) *string { return r.Nickname }),
		textCol("symbol", func(r row) *string { return r.Symbol }),
		textCol("name", func(r row) *string { return r.Name }),
		// A row the realized lots or events alone make carries their own
		// key, since no position line names its instrument.
		{Name: "position_key", Align: output.AlignLeft,
			Extract: func(r row) string {
				if r.PositionKey != nil {
					return *r.PositionKey
				}
				return strOrEmpty(r.LotKey)
			}},
		textCol("asset_class", func(r row) *string { return r.AssetClass }),
		textCol("vehicle", func(r row) *string { return r.Vehicle }),
		textCol("currency", func(r row) *string { return r.Currency }),
		quantityCol("quantity_start", func(r row) *string { return r.QuantityStart }),
		quantityCol("quantity_end", func(r row) *string { return r.QuantityEnd }),
		moneyCol("cost_basis", "", func(r row) *string { return r.BookValue }),
		outCcyTwin("cost_basis", outCcy, func(r row) *string { return r.BookValueOutCcy }),
		moneyCol("market_value", "", func(r row) *string { return r.MarketValue }),
		outCcyCol("value", outCcy, func(r row) *string { return r.ValueOutCcy }),
		outCcyCol("unrealized_start", outCcy, func(r row) *string { return r.UnrealizedStart }),
		outCcyCol("unrealized_end", outCcy, func(r row) *string { return r.UnrealizedEnd }),
		outCcyCol("unrealized_change", outCcy, func(r row) *string { return r.UnrealizedChange }),
		outCcyCol("realized", outCcy, func(r row) *string { return r.Realized }),
		outCcyCol("gain", outCcy, func(r row) *string { return r.Gain }),
		{Name: "unrealized_pct", Align: output.AlignRight,
			Extract: func(r row) string { return formatPctOrBlank(r.UnrealizedRatio) }},
		textCol("basis_stamp", func(r row) *string { return r.BasisStamp }),
		textCol("acquisition_date", func(r row) *string { return r.AcquisitionDate }),
		countCol("lots", func(r row) int64 { return r.OpenLots }),
		{Name: "quality", Align: output.AlignLeft, Extract: func(r row) string { return r.Quality }},
	}
}

var defaultGainsPositionColumns = []string{
	"silver_source", "account", "symbol", "asset_class", "currency",
	"cost_basis_outccy", "value", "unrealized_end", "realized", "gain", "basis_stamp",
}

func buildRealizedLotColumnRegistry(outCcy string) []columnSpec[gold.RealizedLotRow] {
	type row = gold.RealizedLotRow
	type col = columnSpec[row]
	return []col{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft, Extract: func(r row) string { return r.EffectiveDate }},
		// The date is the tax year's last day when the document states
		// no disposal or settlement date.
		{Name: "undated", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(&r.Undated) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.AccountExternalID }},
		textCol("account_nickname", func(r row) *string { return r.Nickname }),
		textCol("symbol", func(r row) *string { return r.Symbol }),
		// The document's own name for the security: a public name,
		// legible like the holdings' name column.
		textCol("description", func(r row) *string { return r.Description }),
		textCol("instrument_id", func(r row) *string { return r.InstrumentExternalID }),
		quantityCol("quantity", func(r row) *string { return r.Quantity }),
		{Name: "acquired", Align: output.AlignLeft,
			Extract: func(r row) string {
				if r.AcquiredVarious {
					return "various"
				}
				return strOrEmpty(r.AcquisitionDate)
			}},
		textCol("term", func(r row) *string { return r.Term }),
		{Name: "covered", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(r.Covered) }},
		textCol("form_8949_box", func(r row) *string { return r.Form8949Box }),
		{Name: "held_days", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.HeldDays) }},
		moneyCol("proceeds", "", func(r row) *string { return r.Proceeds }),
		moneyCol("cost_basis", "", func(r row) *string { return r.BookValue }),
		moneyCol("gain", "", func(r row) *string { return r.Gain }),
		moneyCol("wash_disallowed", "", func(r row) *string { return r.WashDisallowed }),
		moneyCol("accrued_market_discount", "", func(r row) *string { return r.AccruedMarketDiscount }),
		{Name: "gain_origin", Align: output.AlignLeft, Extract: func(r row) string { return r.GainOrigin }},
		{Name: "currency", Align: output.AlignLeft, Extract: func(r row) string { return r.Currency }},
		outCcyTwin("proceeds", outCcy, func(r row) *string { return r.ProceedsOutCcy }),
		outCcyTwin("cost_basis", outCcy, func(r row) *string { return r.BookValueOutCcy }),
		outCcyTwin("gain", outCcy, func(r row) *string { return r.GainOutCcy }),
		{Name: "document", Align: output.AlignLeft, Extract: func(r row) string { return r.DocumentKind }},
		{Name: "tax_year", Align: output.AlignLeft, Extract: func(r row) string { return fmt.Sprintf("%d", r.TaxYear) }},
		{Name: "primary", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(&r.IsPrimary) }},
		textCol("basis_stamp", func(r row) *string { return r.BasisStamp }),
		{Name: "lot_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.RealizedLotExternalID }},
		{Name: "source_document", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return strOrEmpty(r.SourceDocument) }},
	}
}

var defaultRealizedLotColumns = []string{
	"silver_source", "date", "account", "symbol", "quantity", "acquired", "term",
	"currency", "proceeds", "cost_basis", "gain", "gain_outccy",
}

func buildOpenLotColumnRegistry(outCcy string) []columnSpec[gold.OpenLotRow] {
	type row = gold.OpenLotRow
	type col = columnSpec[row]
	return []col{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string { return r.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(r row) string { return formatDate(r.SnapshotAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.AccountExternalID }},
		textCol("account_nickname", func(r row) *string { return r.Nickname }),
		textCol("symbol", func(r row) *string { return r.Symbol }),
		textCol("name", func(r row) *string { return r.Name }),
		{Name: "position_key", Align: output.AlignLeft, Extract: func(r row) string { return r.PositionKey }},
		{Name: "lot_id", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string { return r.LotKey }},
		textCol("acquisition_date", func(r row) *string { return r.AcquisitionDate }),
		{Name: "held_days", Align: output.AlignRight, Extract: func(r row) string { return intOrEmpty(r.HeldDays) }},
		textCol("term", func(r row) *string { return r.Term }),
		{Name: "covered", Align: output.AlignLeft, Extract: func(r row) string { return yesNo(r.Covered) }},
		quantityCol("quantity", func(r row) *string { return r.Quantity }),
		moneyCol("cost_basis", "", func(r row) *string { return r.BookValue }),
		moneyCol("market_value", "", func(r row) *string { return r.MarketValue }),
		textCol("value_origin", func(r row) *string { return r.ValueOrigin }),
		moneyCol("unrealized_gain", "", func(r row) *string { return r.UnrealizedGain }),
		{Name: "unrealized_pct", Align: output.AlignRight,
			Extract: func(r row) string { return formatPctOrBlank(r.UnrealizedRatio) }},
		{Name: "currency", Align: output.AlignLeft, Extract: func(r row) string { return r.Currency }},
		outCcyTwin("cost_basis", outCcy, func(r row) *string { return r.BookValueOutCcy }),
		outCcyCol("value", outCcy, func(r row) *string { return r.ValueOutCcy }),
		outCcyTwin("unrealized_gain", outCcy, func(r row) *string { return r.UnrealizedOutCcy }),
		textCol("basis_origin", func(r row) *string { return r.BasisOrigin }),
		{Name: "source_document", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return strOrEmpty(r.SourceDocument) }},
	}
}

var defaultOpenLotColumns = []string{
	"silver_source", "account", "symbol", "acquisition_date", "held_days", "term",
	"quantity", "cost_basis", "market_value", "unrealized_gain", "currency",
}

func buildGainsCoverageColumnRegistry(outCcy string) []columnSpec[gold.GainsCoverageRow] {
	type row = gold.GainsCoverageRow
	type col = columnSpec[row]
	return []col{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r row) string { return r.SilverSourceID }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.AccountExternalID }},
		textCol("account_nickname", func(r row) *string { return r.Nickname }),
		textCol("tax_wrapper", func(r row) *string { return r.TaxWrapper }),
		outCcyCol("value", outCcy, func(r row) *string { return r.Value }),
		outCcyCol("value_with_basis", outCcy, func(r row) *string { return r.ValueWithBasis }),
		{Name: "basis_coverage", Header: "basis_coverage_pct", Align: output.AlignRight,
			Extract: func(r row) string { return formatPctOrBlank(r.BasisCoverage) }},
		textCol("basis_stamps", func(r row) *string { return r.BasisStamps }),
		countCol("open_lots", func(r row) int64 { return r.OpenLots }),
		countCol("sells", func(r row) int64 { return r.Sells }),
		countCol("realized_lots", func(r row) int64 { return r.RealizedLots }),
		textCol("documents", func(r row) *string { return r.Documents }),
		{Name: "verdict", Align: output.AlignLeft, Extract: func(r row) string { return r.Verdict }},
	}
}

var defaultGainsCoverageColumns = []string{
	"silver_source", "account", "tax_wrapper", "value", "value_with_basis", "basis_coverage",
	"basis_stamps", "open_lots", "sells", "realized_lots", "documents", "verdict",
}

func gainsUsage() string {
	cols := func(grain gold.GainsGrain) string {
		return joinColumnNames(buildGainsBucketColumnRegistry("CCY", "monthly", grain, nil))
	}
	return "usage: wealthdb gains <view> [FROM [TO]] [--period P] [--documents D] [-r]\n" +
		"                         [-f FORMAT] [-C COLS] [-x CCY] [-p]\n" +
		`
What was gained or lost (P&L) on what is held, realized and unrealized,
over a window. A realized gain is what a sale's tax document or
statement states. An unrealized gain is a holding's clean value (market
value less accrued interest) less its cost basis. The figures are only
as complete as the sources: read the quality column, and the coverage
view, before trusting a total. docs/GAINS.md defines every figure.

Views (coarse to fine)
  summary       one row per period bucket, the whole portfolio
  sources       one row per bucket and source
  portfolios    one row per bucket and portfolio, plus one per source for
                its accounts outside any portfolio
  accounts      one row per bucket and account, with its tax wrapper
  positions     one row per account and instrument over the window
  realized      one row per realized lot sold in the window, oldest first
  lots          one row per open lot, as of the window's end
  coverage      one row per account: where the figures are blind

  The four aggregate views reconcile: summary == Σ sources ==
  Σ portfolios == Σ accounts, bucket by bucket.

Window
  FROM and TO are ISO dates, a year, or the usual shorthands; a bare
  invocation reports the trailing twelve months. Gains are often read
  per calendar year: wealthdb gains realized 2025

Flags
  --period P      ` + strings.Join(reportPeriodNames, " | ") + ` (default monthly), on the
                  four aggregate views; the others read the whole window
  --documents D   primary (default) | all, realized only: all lists every
                  copy of a sale (a 1099-B, its correction, a year-end
                  summary) with the document and primary columns
  -r              realized only: newest first
  -f FORMAT       table | csv | csv_plain | json
  -C COLS         comma-separated names, 'default', 'all', or a
                  +ADD,-REMOVE delta on the default set
  -x CCY          output currency (default: config.default_currency)
  -p              redact account ids, quantities and amounts; percentages,
                  stamps, dates and security names stay legible

Notes
  gain = realized + unrealized_change. A sale moves gain from unrealized
  to realized; a purchase adds none.

  The cost basis is what each source states, and basis_stamp says which
  notion it is (origin/method/fees): a brokerage lot's tax basis, a
  fund's capital paid in, an exercised option's value at exercise.

  quality names each way a figure can be incomplete:
  sells_without_documents=N, lots_without_gain=N, undated_lots=N,
  unmatched_lots=N, in_kind_moves=N, corporate_actions=N,
  basis_changed=N, accounts_unobserved=N, paid_in_basis,
  onboarded_in_window=<source>, fx_missing=N. docs/GAINS.md §6 says
  what each means.

Available columns (per view):
  summary       ` + cols(gold.GainsAll) + `
  sources       silver_source, then summary's
  portfolios    silver_source, portfolio, portfolio_id, then summary's
  accounts      ` + cols(gold.GainsAccounts) + `
  positions     ` + joinColumnNames(buildGainsPositionColumnRegistry("CCY")) + `
  realized      ` + joinColumnNames(buildRealizedLotColumnRegistry("CCY")) + `
  lots          ` + joinColumnNames(buildOpenLotColumnRegistry("CCY")) + `
  coverage      ` + joinColumnNames(buildGainsCoverageColumnRegistry("CCY")) + `

  A money column in the holding's own currency has a twin in the output
  currency, <name>_outccy, which prints as <name>_<CCY> (value is
  market_value's twin). The window's figures exist only in the output
  currency and print as <name>_<CCY>.

Default column sets:
  summary       ` + strings.Join(gainsBucketDefaults[gold.GainsAll], ", ") + `
  sources       ` + strings.Join(gainsBucketDefaults[gold.GainsSources], ", ") + `
  portfolios    ` + strings.Join(gainsBucketDefaults[gold.GainsPortfolios], ", ") + `
  accounts      ` + strings.Join(gainsBucketDefaults[gold.GainsAccounts], ", ") + `
  positions     ` + strings.Join(defaultGainsPositionColumns, ", ") + `
  realized      ` + strings.Join(defaultRealizedLotColumns, ", ") + `
  lots          ` + strings.Join(defaultOpenLotColumns, ", ") + `
  coverage      ` + strings.Join(defaultGainsCoverageColumns, ", ")
}
