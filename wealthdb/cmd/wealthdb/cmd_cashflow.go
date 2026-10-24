package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// `wealthdb cashflow <view>` — where the household's cash came from and
// where it went.
//
// cmd_income.go's shape, and the two families' idiom unchanged: the grain in a positional view, everything
// else in a flag, the window positional with the trailing twelve months
// as its default, `share_pct` rendered by the same percentage formatter.
// What is its own is one flag — `--investing` — and three refusals.
//
// `coverage` is the odd one out: it does not aggregate, and it reports
// each account against that account's OWN balances in that account's
// own currency. So it carries a currency COLUMN where its siblings
// carry a currency FLAG.
//
// THE THREE REFUSALS are `sankey --period`, `sankey --level section` and
// `coverage -x`, and all three are usage errors rather than silent
// ignores. A period on an edge list would mean one list per bucket,
// which is a loop's job; a section level would draw a diagram with no
// inner column, which is not the diagram; and an output currency on
// coverage would put a rate error on top of the one number that view
// exists to make trustworthy. Ignoring any of them would hand back a
// plausible answer to a question the caller did not ask.
// `transactions` DOES ignore --period, as the families' transactions
// views do: a list of lines means the same thing however it is
// bucketed.
func init() {
	register("cashflow", cmdCashflow)
}

// cashflowViews are the grains `wealthdb cashflow <view>` supports: the
// statement's, coarsest first, then the plan.
var cashflowViews = []string{"summary", "flows", "sankey", "transactions", "coverage", "projection"}

// cashflowInvestingGrains is the `--investing` vocabulary: net the
// section as one movement, or per asset class.
var cashflowInvestingGrains = []string{"whole", "class"}

// cmdCashflow routes `wealthdb cashflow <view> ...` to the view's run.
func cmdCashflow(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("cashflow", subargs, cashflowViews, cashflowUsage, stderr)
	if err != nil {
		return err
	}
	return runCashflowView(ctx, g, view, rest, stdout, stderr)
}

func runCashflowView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb cashflow "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	level := fs.String("level", "group", "section | class | group — the grain a node is netted at (flows, sankey)")
	investing := fs.String("investing", "whole", strings.Join(cashflowInvestingGrains, " | ")+" — net investing as one node or per asset class")
	ff := registerForwardFlags(fs)
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, cashflowUsage()) }
	if err := parseFlags(fs, args, "cashflow"); err != nil {
		return err
	}
	// The refusals have to be asked before the values are validated:
	// `coverage -x` is refused for the same reason the report has no
	// output currency: a per-account gap belongs in the account's own
	// money, and converting it would add a rate error to the very
	// number the report exists to make trustworthy.
	if view == "coverage" && isSet(fs, "x", "currency") {
		return errs.Newf(errs.ExitUsage, "cashflow: coverage takes no -x/--currency — "+
			"each account is measured against its own balances, in its own currency")
	}
	// `sankey --period annual` is refused for what it means, not for
	// being misspelled.
	if view == "sankey" {
		if isSet(fs, "period") {
			return errs.Newf(errs.ExitUsage, "cashflow: sankey takes no --period — a diagram is a window, not a series; "+
				"loop over the years instead")
		}
		if *level == "section" {
			return errs.Newf(errs.ExitUsage, "cashflow: sankey --level section would draw no inner column (want class | group)")
		}
	}

	if err := checkCashflowPlanOptions(view, *period, func(name string) bool { return isSet(fs, name) },
		func(name string) string { return "--" + name }); err != nil {
		return err
	}
	if err := ff.checkChoices("cashflow"); err != nil {
		return err
	}
	if err := checkChoice("cashflow", "period", *period, reportPeriodNames...); err != nil {
		return err
	}
	if err := checkChoice("cashflow", "level", *level, "section", "class", "group"); err != nil {
		return err
	}
	if err := checkChoice("cashflow", "investing", *investing, cashflowInvestingGrains...); err != nil {
		return err
	}

	fmtChoice, cfg, outCcy, err := rf.resolve(g, "cashflow")
	if err != nil {
		return err
	}
	now := g.clock()
	var window windowParser
	if view == "projection" {
		window = parsePlanWindow
	}
	fromEpoch, toEpoch, err := reportWindow(fs, "cashflow", now, cfg.YieldHorizon(), window)
	if err != nil {
		return err
	}

	rep := cashflowReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch,
		period: *period, level: *level, investing: *investing, categories: overrideCategories(cfg),
		asOf: anchorToDay(now.UTC(), true).Unix(), assumptions: ff.assumptions(), lookback: cfg.CashflowLookback()})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "cashflow", open, *rf.privacy, fmtChoice, stdout)
}

// cashflowReport is one view of the cash flow family, the runner the
// CLI and the MCP server share. The refusals are the front-ends' to
// make before they get here.
func cashflowReport(req request) *report {
	part := reportPeriods[req.period]
	switch req.view {
	case "projection":
		return newReport(buildCashflowPlanColumns(req.currency, req.period), defaultCashflowPlanColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.PlanRow, error) { return cashflowPlan(ctx, db, req) })
	case "summary":
		return newReport(buildCashflowSummaryColumnRegistry(req.currency, req.period), defaultCashflowSummaryColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.CashflowSummaryRow, error) {
				return gold.CashflowSummary(ctx, db, req.from, req.to, req.currency, part)
			})
	case "flows":
		return newReport(buildCashflowFlowColumnRegistry(req.currency, req.period), defaultCashflowFlowColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.CashflowFlowRow, error) {
				return gold.CashflowFlows(ctx, db, req.from, req.to, req.currency, part, req.level, req.investing)
			})
	case "sankey":
		return newReport(buildCashflowSankeyColumnRegistry(req.currency), defaultCashflowSankeyColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.CashflowSankeyRow, error) {
				return gold.CashflowSankey(ctx, db, req.from, req.to, req.currency, req.level, req.investing)
			})
	case "coverage":
		kindOf, loadKinds := sourceKinds()
		nicknames := accountNicknames{}
		return withLoads(newReport(buildCashflowCoverageColumnRegistry(req.period, kindOf, nicknames),
			defaultCashflowCoverageColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.CashflowCoverageRow, error) {
				return gold.CashflowCoverage(ctx, db, req.from, req.to, part)
			}), loadKinds, nicknames.load)
	default:
		kindOf, loadKinds := sourceKinds()
		return withLoads(newReport(buildCashflowTransactionColumnRegistry(req.currency, kindOf, req.categories),
			defaultCashflowTransactionColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.CashflowTransactionRow, error) {
				return gold.CashflowTransactions(ctx, db, req.from, req.to, req.currency)
			}), loadKinds)
	}
}

// checkCashflowPlanOptions refuses the options a view does not take, the
// CLI and the MCP server alike: the projection's assumptions off it; on
// it the level, the investing grain, and a period that is none of
// seriesPeriods. has says an option is given; flag spells an option's
// name as the front-end does.
func checkCashflowPlanOptions(view, period string, has func(string) bool, flag func(string) string) error {
	if view != "projection" {
		return refuseAssumptions("cashflow", "the projection view", has, flag)
	}
	if has("level") || has("investing") {
		return errs.Newf(errs.ExitUsage, "cashflow: projection takes no %s or %s", flag("level"), flag("investing"))
	}
	return checkSeriesPeriod("cashflow", "projection takes", period, flag)
}

// parsePlanWindow is the projection's window: the forward window of the
// income projection (parseForwardWindow), held to checkPlanWindow.
func parsePlanWindow(from, to string, now time.Time, horizon int) (int64, int64, error) {
	f, t, err := parseForwardWindow(from, to, now, horizon)
	if err != nil {
		return 0, 0, err
	}
	return f, t, checkPlanWindow(f, t, now)
}

// checkPlanWindow refuses a plan's window [from, to] that starts before
// next month or cuts a month, the CLI and the MCP server alike: the plan
// sets whole months of the projection against the look-back's monthly
// averages, so a part of a month would carry a whole month of outflows.
func checkPlanWindow(from, to int64, now time.Time) error {
	n := now.UTC()
	next := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	monthStart := func(at int64) bool { d := time.Unix(at, 0).UTC(); return d.Day() == 1 && d.Unix()%86400 == 0 }
	switch {
	case from < next.Unix():
		return fmt.Errorf("projection plans the months after this one: it starts in %s or later",
			next.Format("2006-01"))
	case !monthStart(from) || !monthStart(to+1):
		return fmt.Errorf("projection plans whole months: its window starts on a month's first day and ends on a " +
			"month's last")
	}
	return nil
}

// cashflowPlan is the projection view's rows: today's income projection
// for the window's months, split by the pool (gold.PlanMonths), set
// against the look-back's averages (gold.CashflowPlanLookback), the
// mortgages that today's book holds continued to their maturity.
func cashflowPlan(ctx context.Context, db *sql.DB, req request) ([]gold.PlanRow, error) {
	b, w, err := forwardBook(ctx, db, req, "cashflow")
	if err != nil {
		return nil, err
	}
	pool, err := gold.CashflowPool(ctx, db)
	if err != nil {
		return nil, err
	}
	months := gold.PlanMonths(b, b.Amounts(nil, w, req.assumptions), w, pool)
	from, to := gold.PlanWindow(time.Unix(req.asOf, 0), req.lookback)
	lb, err := gold.CashflowPlanLookback(ctx, db, from, to, req.currency)
	if err != nil {
		return nil, err
	}
	return gold.CashflowPlan(lb, gold.PlanStanding(b), months, seriesPeriodMonths(req.period),
		req.assumptions.Rule == yield.RuleRoll), nil
}

// ---- column registries ---------------------------------------------------

// buildCashflowPlanColumns are the projection view's columns: each part
// in the output currency, the operating-out classes behind -C, and what
// the parts rest on (docs/CASHFLOW.md §12).
func buildCashflowPlanColumns(outCcy, period string) []columnSpec[gold.PlanRow] {
	type row = gold.PlanRow
	cols := []columnSpec[row]{
		{Name: "period", Align: output.AlignLeft, Extract: func(r row) string {
			switch {
			case r.Total:
				return "total"
			case r.Average:
				return "monthly_average"
			}
			at := r.Period.Unix()
			return periodLabel(&at, period)
		}},
		floatValCol("investment_income", outCcy, func(r row) float64 { return r.InvestmentIncome }),
		floatValCol("principal_returned", outCcy, func(r row) float64 { return r.PrincipalReturned }),
		floatValCol("vehicles_income", outCcy, func(r row) float64 { return r.VehiclesIncome }),
		floatValCol("other_income", outCcy, func(r row) float64 { return r.OtherIncome }),
		floatValCol("operating_out", outCcy, func(r row) float64 { return r.OperatingOut }),
		floatValCol("debt_service", outCcy, func(r row) float64 { return r.DebtService }),
		floatValCol("vehicles", outCcy, func(r row) float64 { return r.Vehicles }),
		floatValCol("net", outCcy, func(r row) float64 { return r.Net }),
	}
	for _, c := range gold.PlanOutClasses {
		cols = append(cols, floatValCol(planClassColumn(c), outCcy, func(r row) float64 { return r.OutClasses[c] }))
	}
	return append(cols, qualityCol(func(r row) string { return r.Quality }))
}

// planClassColumn names an operating-out class's column: its id, the
// unplaced class as uncategorized.
func planClassColumn(class string) string { return strings.Trim(class, "()") }

var defaultCashflowPlanColumns = []string{"period", "investment_income", "principal_returned", "vehicles_income",
	"other_income", "operating_out", "debt_service", "vehicles", "net", "quality"}

// cashflowPlanTags are the tags of the plan's quality column and what
// each means (docs/CASHFLOW.md §12), in the order gold.CashflowPlan
// lists them: the CLI usage and the MCP help both render them.
var cashflowPlanTags = []tagNote{
	{gold.PlanProjected, "investment_income, principal_returned and vehicles_income are the income projection's"},
	{gold.PlanTrailing, "the other parts are monthly averages of the look-back"},
	{gold.PlanTerms, "a mortgage's instalments end at its terms' maturity"},
	{gold.PlanThin, "the look-back holds under " + strconv.Itoa(gold.PlanThinMonths) + " months"},
}

// planClassColumns are the -C delta that adds every operating-out class
// column of the plan (gold.PlanOutClasses).
func planClassColumns() string {
	cols := make([]string, len(gold.PlanOutClasses))
	for i, c := range gold.PlanOutClasses {
		cols[i] = "+" + planClassColumn(c)
	}
	return strings.Join(cols, ",")
}

func buildCashflowSummaryColumnRegistry(outCcy, period string) []columnSpec[gold.CashflowSummaryRow] {
	return []columnSpec[gold.CashflowSummaryRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSummaryRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSummaryRow) string { return periodStart(r.PeriodStart) }},
		countCol("txn_count", func(r gold.CashflowSummaryRow) int64 { return r.TxnCount }),
		// The two halves of operating. NOT named `income` and
		// `spending`: those are two other reports whose numbers differ
		// from these by construction, and a column a reader cannot
		// safely compare to the report it is named after is a trap.
		// The flows view already spells them this way.
		outCcyCol("operating_in", outCcy, func(r gold.CashflowSummaryRow) *string { return r.OperatingIn }),
		outCcyCol("operating_out", outCcy, func(r gold.CashflowSummaryRow) *string { return r.OperatingOut }),
		outCcyCol("operating", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Operating }),
		outCcyCol("investing", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Investing }),
		outCcyCol("financing", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Financing }),
		outCcyCol("vehicles", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Vehicles }),
		outCcyCol("net_cash_flow", outCcy, func(r gold.CashflowSummaryRow) *string { return r.NetCashFlow }),
		// The yield class alone: for a household whose money comes from
		// wealth rather than wages, the figure the report is opened for.
		outCcyCol("yield", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Yield }),
		outCcyCol("taxes", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Taxes }),
		outCcyCol("fees", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Fees }),
		outCcyCol("giving", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Giving }),
		ratioCol("savings_rate", func(r gold.CashflowSummaryRow) *float64 { return r.SavingsRate }),
		// The reconciliation memo. It is the one check that can catch a
		// wrong POPULATION rather than wrong arithmetic — the pool's
		// balances are observed, and the statement's cash section is
		// not — and it is never read by net_cash_flow.
		outCcyCol("cash_measured", outCcy, func(r gold.CashflowSummaryRow) *string { return r.CashMeasured }),
		outCcyCol("fx_effect", outCcy, func(r gold.CashflowSummaryRow) *string { return r.FXEffect }),
		// The flow term the residual is taken against, so the four
		// memo columns close on their face. It is the flow over the
		// MEASURED slice, pool-internal moves included, and so is not
		// net_cash_flow — printing the residual without it left a
		// reader's own arithmetic short.
		outCcyCol("cash_flow_measured", outCcy, func(r gold.CashflowSummaryRow) *string { return r.CashFlowMeasured }),
		outCcyCol("unexplained", outCcy, func(r gold.CashflowSummaryRow) *string { return r.Unexplained }),
	}
}

var defaultCashflowSummaryColumns = []string{
	"period", "operating_in", "operating_out", "operating",
	"investing", "financing", "vehicles", "net_cash_flow",
}

func buildCashflowFlowColumnRegistry(outCcy, period string) []columnSpec[gold.CashflowFlowRow] {
	return []columnSpec[gold.CashflowFlowRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return periodStart(r.PeriodStart) }},
		{Name: "section", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return r.Section }},
		// `class` and `group` are what a reader sees; the _id columns
		// are the keys behind them, and they carry the whole node key
		// rather than the leaf alone — a gift given and a gift received
		// are two nodes, and only the full key tells them apart.
		textCol("class", func(r gold.CashflowFlowRow) *string { return r.ClassLabel }),
		textCol("group", func(r gold.CashflowFlowRow) *string { return r.GroupLabel }),
		{Name: "section_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return r.Section }},
		{Name: "class_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return nodeKey(r.Section, r.Class, nil) }},
		{Name: "group_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return nodeKey(r.Section, r.Class, r.Group) }},
		countCol("txn_count", func(r gold.CashflowFlowRow) int64 { return r.TxnCount }),
		outCcyCol("inflow", outCcy, func(r gold.CashflowFlowRow) *string { return r.Inflow }),
		outCcyCol("outflow", outCcy, func(r gold.CashflowFlowRow) *string { return r.Outflow }),
		outCcyCol("net", outCcy, func(r gold.CashflowFlowRow) *string { return r.Net }),
		ratioCol("share", func(r gold.CashflowFlowRow) *float64 { return r.Share }),
	}
}

var defaultCashflowFlowColumns = []string{
	"period", "section", "class", "group", "txn_count",
	"inflow", "outflow", "net", "share",
}

// nodeKey renders a node's identity as `section.class.group`, stopping
// at whatever the row reaches. A node IS the whole key: three values of
// the shared vocabulary — a gift, an other, an unplaced row — exist on
// both sides of the household, and keying by the leaf alone would
// collide the two.
func nodeKey(section string, class, group *string) string {
	key := section
	if class != nil {
		key += "." + *class
	}
	if group != nil {
		key += "." + *group
	}
	return key
}

func buildCashflowSankeyColumnRegistry(outCcy string) []columnSpec[gold.CashflowSankeyRow] {
	return []columnSpec[gold.CashflowSankeyRow]{
		countCol("stage", func(r gold.CashflowSankeyRow) int64 { return r.Stage }),
		// Node names are vocabulary, never a merchant, a payer, an
		// account or an instrument — which is why the privacy twin of
		// this view needs normalisation alone and no redaction.
		{Name: "source", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSankeyRow) string { return r.Source }},
		{Name: "target", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSankeyRow) string { return r.Target }},
		{Name: "source_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSankeyRow) string { return r.SourceID }},
		{Name: "target_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSankeyRow) string { return r.TargetID }},
		textCol("section", func(r gold.CashflowSankeyRow) *string { return r.Section }),
		outCcyCol("value", outCcy, func(r gold.CashflowSankeyRow) *string { return r.Value }),
		ratioCol("share", func(r gold.CashflowSankeyRow) *float64 { return r.Share }),
	}
}

var defaultCashflowSankeyColumns = []string{"stage", "source", "target", "value", "share"}

var defaultCashflowCoverageColumns = []string{
	"period", "silver_source", "account", "currency", "ledger", "measured", "gap", "status"}

// The coverage registry carries no output currency: every amount is in
// the account's own. A column header naming a currency the row is not in
// would be worse than no header at all.
//
// A coverage row names its account by one label: the nickname, else
// the display name, else the id. Under -p a label that is one of its
// source's nicknames, or that may be typed (typedAccountLabel,
// typedAccountID), prints as a pseudonym: the row has no id to show in
// its place.
func buildCashflowCoverageColumnRegistry(period string, kindOf func(string) string,
	nicknames accountNicknames) []columnSpec[gold.CashflowCoverageRow] {
	return []columnSpec[gold.CashflowCoverageRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return periodStart(r.PeriodStart) }},
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return r.SourceID }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			PrivacyFunc: func(r gold.CashflowCoverageRow) PrivacyClass {
				if typedAccountLabel(kindOf, r.SourceID) || typedAccountID(kindOf, r.SourceID) ||
					nicknames.has(r.SourceID, r.Account) {
					return PrivacyPseudonym
				}
				return PrivacyAccountID
			},
			Pseudonym: func(r gold.CashflowCoverageRow) (string, string) {
				if nicknames.has(r.SourceID, r.Account) {
					return "nickname", ""
				}
				return typedPseudonym(kindOf, r.SourceID)
			},
			Extract: func(r gold.CashflowCoverageRow) string { return r.Account }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return r.AccountKind }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return r.Currency }},
		countCol("txn_count", func(r gold.CashflowCoverageRow) int64 { return r.TxnCount }),
		{Name: "ledger", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowCoverageRow) string { return strOrEmpty(r.Ledger) }},
		{Name: "unsigned", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowCoverageRow) string { return strOrEmpty(r.UnsignedVolume) }},
		{Name: "measured", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowCoverageRow) string { return strOrEmpty(r.Measured) }},
		{Name: "gap", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowCoverageRow) string { return strOrEmpty(r.Gap) }},
		{Name: "status", Align: output.AlignLeft,
			Extract: func(r gold.CashflowCoverageRow) string { return r.Status }},
	}
}

func buildCashflowTransactionColumnRegistry(outCcy string, kindOf func(string) string,
	categories typedCategories) []columnSpec[gold.CashflowTransactionRow] {
	ref := func(r gold.CashflowTransactionRow) accountRef {
		return accountRef{source: r.SilverSourceID, name: r.DisplayName, nickname: r.Nickname, id: r.AccountExternalID}
	}
	return []columnSpec[gold.CashflowTransactionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return formatDate(r.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return formatDateTime(r.OccurredAt) }},
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		textCol("account_kind", func(r gold.CashflowTransactionRow) *string { return r.AccountKind }),
		nicknameCol("account_nickname", func(r gold.CashflowTransactionRow) *string { return r.Nickname }),
		accountCategoryCol(categories, func(r gold.CashflowTransactionRow) *string { return r.AccountCategory }),
		{Name: "kind", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.Kind }},
		{Name: "section", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.Section }},
		{Name: "class", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.ClassLabel }},
		{Name: "group", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.GroupLabel }},
		{Name: "class_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.Section + "." + r.Class }},
		{Name: "group_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string {
				return r.Section + "." + r.Class + "." + r.Group
			}},
		// The instrument on an investing line, the payer or merchant on
		// an operating one. It takes the FREE-TEXT class for the reason
		// the families' payer column does: a column takes the class of
		// the worst thing it can hold, and on an inbound wire that is a
		// person. A typed holding's line names what its holder typed,
		// which prints as a pseudonym.
		typedHoldingCol(columnSpec[gold.CashflowTransactionRow]{Name: "name", Align: output.AlignLeft,
			Privacy: PrivacyFreeText, Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Name) }},
			kindOf, func(r gold.CashflowTransactionRow) (string, string) { return r.SilverSourceID, strOrEmpty(r.Name) }),
		// The family's own value behind the node, and the two overlays
		// it could have come from.
		textCol("detailed", func(r gold.CashflowTransactionRow) *string { return r.Verdict }),
		textCol("spend_detailed", func(r gold.CashflowTransactionRow) *string { return r.SpendDetailed }),
		textCol("income_detailed", func(r gold.CashflowTransactionRow) *string { return r.IncomeDetailed }),
		textCol("provenance", func(r gold.CashflowTransactionRow) *string { return r.Provenance }),
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.Currency }},
		moneyCol("net_amount", "", func(r gold.CashflowTransactionRow) *string { return r.NetAmount }),
		outCcyCol("value", outCcy, func(r gold.CashflowTransactionRow) *string { return r.ValueOutCcy }),
		{Name: "counterparty", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Counterparty) }},
		{Name: "description", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Description) }},
		typedIDCol("tx_id", kindOf, func(r gold.CashflowTransactionRow) (string, string) {
			return r.SilverSourceID, r.TransactionExternalID
		}),
	}
}

var defaultCashflowTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "section", "class", "group",
	"name", "currency", "net_amount", "value",
}

func cashflowUsage() string {
	return "usage: wealthdb cashflow <view> [FROM [TO]] [--period P] [--level L] [--investing G]\n" +
		"                            [--rates R] [--maturing M] [--shift BP] [-f FORMAT] [-C COLS] [-x CCY] [-p]\n" +
		`
What the household's cash did: where it came from and where it went,
as a cash flow statement and as the edge list of a Sankey. The
household is the accounts in its own tax wrappers; retirement plans,
trusts and charitable vehicles are vehicles it pays into and draws
on. Moves between the household's own accounts are invisible; buying
and selling is shown NET per period, never as two gross bands.
Positive is cash arriving, negative is cash leaving.

Views (the statement coarsest → finest, then the plan):
  summary       one row per period bucket: operating_in, operating_out,
                operating, investing, financing, vehicles, net_cash_flow
  flows         one row per bucket and node at --level, netted at that level
  sankey        the window's diagram as edges: stage, source, target, value
                (period-less; --level class for the inner two stages only)
  transactions  one row per line: section, class, group, and the family's
                own verdict behind it
  coverage      per account and period: the cash delta its transactions
                imply against the delta its own balances show, in the
                ACCOUNT'S currency. 'status' is the column to read first:
                'measured' is a real gap (zero when the account
                reconciles), 'obscured' means the account carries at
                least as much unsigned volume (FX, in-kind transfers)
                as the gap so nothing can be concluded,
                'opening' means the balance series
                began mid-period, 'ended' that it stopped before the
                period, 'unmeasurable' means no balances at all. Sort by
                gap where status is 'measured'.
  projection    the plan, one row per month ahead; see Notes.
                --period ` + strings.Join(seriesPeriods, " | ") + `

Window (positional, optional; default: the trailing twelve months):
  YYYY / YYYY-MM / YYYY-MM-DD   that calendar period
  FROM TO                       explicit range; '-' is open-ended
  A household reads this per year: wealthdb cashflow sankey 2025
  projection takes whole months from next month on. Its default is the
  projection's horizon (yield.horizon_months, default ` + strconv.Itoa(canonical.DefaultHorizonMonths) + `).

Flags beyond the ones every report shares:
  --level L     flows: section | class | group (default group)
                sankey: class | group (default group); section is refused
                summary and transactions ignore it
  --investing G whole (default) | class — net investing as one
                Investments node, or per asset class with one node each
                (flows and sankey; the summary is always whole)
  --rates R     projection: ` + strings.Join(pathNames(), " | ") + ` (default ` + string(yield.DefaultAssumptions.Path) +
		`) — how
                floating rates move, as for 'wealthdb income projection'
  --maturing M  projection: ` + strings.Join(ruleNames(), " | ") + ` (default ` + string(yield.DefaultAssumptions.Rule) +
		`) — what
                a maturing principal earns; under roll it buys its
                successor
  --shift BP    projection: a parallel shift of the floating rates
  -f FORMAT     table | csv | csv_plain | json
  -C COLS       comma-separated names, 'default', 'all', or a
                +ADD,-REMOVE delta on the default set
  -x CCY        output currency (default: config.default_currency);
                coverage refuses it - every account is reported in its own
  -p            redact account ids, statement names and amounts;
                every name a person typed prints as a pseudonym,
                such as manual-1, or as a redacted id; sections,
                classes, groups and shares stay legible

Notes
  vehicles is every plan together — retirement, education and health;
  for one of them read 'flows --level class' and take its row. The
  mortgage is under financing.

  The four sections sum to net_cash_flow, and a bucket's flows rows —
  the Cash row included — sum to zero. Both identities are structural
  and guard arithmetic rather than population: the reconciliation that
  can catch a population hole is
  -C +cash_measured,+fx_effect,+cash_flow_measured,+unexplained on the
  summary. Those four close on their face — unexplained is
  cash_measured minus fx_effect minus cash_flow_measured — and
  cash_flow_measured is deliberately NOT net_cash_flow: the memo can
  only compare observed balances against the flow over the accounts
  and dates those balances cover, which counts moves inside the pool
  and starts each account at its own first snapshot.

  A NODE IS A NET, and the level decides what nets. A class with a
  large gross and a small net is one node, not two bands; inflow and
  outflow beside it are the line-level gross under it. share_pct is over
  the hub at the level drawn, so a finer level can have a larger hub
  than a coarser one — that is what netting means.

  operating_in and operating_out are NOT what 'wealthdb income' and
  'wealthdb spending' report, which is why they are not named after
  them. Totals here are smaller than those reports', by four terms:
  the vehicles' own income and spending, the reimbursements cashflow
  keeps and income drops, the verdicts cashflow re-homes, and any
  account the families' own scopes exclude but the pool keeps.

  Tax a vehicle withheld before the household saw the money is on no
  node at all, so the Taxes node is the tax the household paid from its
  own accounts.

  projection is the plan. investment_income and principal_returned
  are what 'wealthdb income projection' expects on the pool's
  accounts. vehicles_income is its income and principal on every
  account outside the pool: a vehicle's, a giving account's, or one
  cashflow.accounts excludes. The three sum to the projection's
  expected and principal. other_income, operating_out and vehicles
  are monthly averages of the months before this one, as the summary
  counts them.
  cashflow.projection.lookback_months sets how many (default ` + strconv.Itoa(config.DefaultCashflowLookbackMonths) +
		`, at
  most ` + strconv.Itoa(config.MaxCashflowLookbackMonths) + `). debt_service runs each mortgage's average instalment to
  its maturity. Other financing outflows stay at their average.

  net is investment_income + other_income - operating_out -
  debt_service + vehicles + principal_returned: the pool's, without
  vehicles_income. Under --maturing roll, net leaves principal_returned
  out. --level, --investing and --period total are refused on
  projection.
  -C ` + planClassColumns() + ` splits
  operating_out by class. quality lists what a row rests on:` + tagNotes(cashflowPlanTags) + `

Available columns (per view):
  summary       ` + joinColumnNames(buildCashflowSummaryColumnRegistry("CCY", "monthly")) + `
  flows         ` + joinColumnNames(buildCashflowFlowColumnRegistry("CCY", "monthly")) + `
  sankey        ` + joinColumnNames(buildCashflowSankeyColumnRegistry("CCY")) + `
  transactions  ` + joinColumnNames(buildCashflowTransactionColumnRegistry("CCY", nil, nil)) + `
  coverage      ` + joinColumnNames(buildCashflowCoverageColumnRegistry("monthly", nil, nil)) + `
  projection    ` + joinColumnNames(buildCashflowPlanColumns("CCY", "monthly")) + `

  (The money columns render as operating_in_<CCY> / net_<CCY> /
   value_<CCY>, reflecting your -x/--currency choice. Coverage's do
   not: every one of its amounts is in the account's own currency.)

Default column sets:
  summary       ` + strings.Join(defaultCashflowSummaryColumns, ", ") + `
  flows         ` + strings.Join(defaultCashflowFlowColumns, ", ") + `
  sankey        ` + strings.Join(defaultCashflowSankeyColumns, ", ") + `
  transactions  ` + strings.Join(defaultCashflowTransactionColumns, ", ") + `
  coverage      ` + strings.Join(defaultCashflowCoverageColumns, ", ") + `
  projection    ` + strings.Join(defaultCashflowPlanColumns, ", ")
}
