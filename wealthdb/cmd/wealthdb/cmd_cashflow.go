package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// `wealthdb cashflow <view>` — where the household's cash came from and
// where it went.
//
// cmd_income.go's shape with four views instead of three, and the two
// families' idiom unchanged: the grain in a positional view, everything
// else in a flag, the window positional with the trailing twelve months
// as its default, `share_%` rendered by the same percentage formatter.
// What is its own is one flag — `--investing` — and two refusals.
//
// THE TWO REFUSALS are `sankey --period` and `sankey --level section`,
// and both are usage errors rather than silent ignores. A period on an
// edge list would mean one list per bucket, which is a loop's job; a
// section level would draw a diagram with no inner column, which is not
// the diagram. Ignoring either would hand back a plausible answer to a
// question the caller did not ask. `transactions` DOES ignore --period,
// as the families' transactions views do: a list of lines means the
// same thing however it is bucketed.
func init() {
	register("cashflow", cmdCashflow)
}

var cashflowViews = map[string]bool{
	"summary": true, "flows": true, "sankey": true, "transactions": true,
}

// cashflowInvestingGrains is the `--investing` vocabulary: net the
// section as one movement, or per asset class.
var cashflowInvestingGrains = []string{"whole", "class"}

// cmdCashflow routes `wealthdb cashflow <view> ...` to the shared
// runner.
func cmdCashflow(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		fmt.Fprintln(stderr, cashflowUsage())
		return errs.Newf(2, "cashflow: a view subcommand is required")
	}
	view, rest := subargs[0], subargs[1:]
	switch view {
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, cashflowUsage())
		return nil
	}
	if !cashflowViews[view] {
		fmt.Fprintln(stderr, cashflowUsage())
		return errs.Newf(2, "cashflow: unknown view %q (want summary | flows | sankey | transactions)", view)
	}
	return runCashflowView(ctx, g, view, rest, stdout, stderr)
}

func runCashflowView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb cashflow "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)

	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	level := fs.String("level", "group", "section | class | group — the grain a node is netted at (flows, sankey)")
	investing := fs.String("investing", "whole", strings.Join(cashflowInvestingGrains, " | ")+" — net investing as one node or per asset class")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	privacy := fs.Bool("p", false, "redact account IDs, names, and monetary amounts (the nodes stay visible)")
	fs.BoolVar(privacy, "privacy", false, "redact account IDs, names, and monetary amounts (the nodes stay visible)")

	fs.Usage = func() { fmt.Fprintln(stderr, cashflowUsage()) }
	reordered := reorderFlagsFirst(splitFusedColumnsFlag(args), reportValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "cashflow: bad flags")
	}
	// The refusals have to be asked before the values are validated:
	// `sankey --period annual` is refused for what it means, not for
	// being misspelled.
	if view == "sankey" {
		if isSet(fs, "period") {
			return errs.Newf(2, "cashflow: sankey takes no --period — a diagram is a window, not a series; "+
				"loop over the years instead")
		}
		if *level == "section" {
			return errs.Newf(2, "cashflow: sankey --level section would draw no inner column (want class | group)")
		}
	}

	part, ok := reportPeriods[*period]
	if !ok {
		return errs.Newf(2, "cashflow: invalid --period %q (want %s)",
			*period, strings.Join(reportPeriodNames, " | "))
	}
	if !oneOf(*level, "section", "class", "group") {
		return errs.Newf(2, "cashflow: invalid --level %q (want section | class | group)", *level)
	}
	if !oneOf(*investing, cashflowInvestingGrains...) {
		return errs.Newf(2, "cashflow: invalid --investing %q (want %s)",
			*investing, strings.Join(cashflowInvestingGrains, " | "))
	}
	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "cashflow: %s", err.Error())
	}

	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "cashflow: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	outCcy := strings.ToUpper(*currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return errs.Newf(2, "cashflow: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	switch view {
	case "summary":
		colSet, err := resolveCashflowSummaryColumns(*cols, outCcy, *period)
		if err != nil {
			return errs.Newf(2, "cashflow: %s", err.Error())
		}
		rows, err := gold.CashflowSummary(ctx, db, fromEpoch, toEpoch, outCcy, part)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	case "flows":
		colSet, err := resolveCashflowFlowColumns(*cols, outCcy, *period)
		if err != nil {
			return errs.Newf(2, "cashflow: %s", err.Error())
		}
		rows, err := gold.CashflowFlows(ctx, db, fromEpoch, toEpoch, outCcy, part, *level, *investing)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	case "sankey":
		colSet, err := resolveCashflowSankeyColumns(*cols, outCcy)
		if err != nil {
			return errs.Newf(2, "cashflow: %s", err.Error())
		}
		rows, err := gold.CashflowSankey(ctx, db, fromEpoch, toEpoch, outCcy, *level, *investing)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	default:
		colSet, err := resolveCashflowTransactionColumns(*cols, outCcy)
		if err != nil {
			return errs.Newf(2, "cashflow: %s", err.Error())
		}
		rows, err := gold.CashflowTransactions(ctx, db, fromEpoch, toEpoch, outCcy)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	}
}

// isSet reports whether a flag was given on the command line, as
// opposed to sitting at its default. The refusals need it: `--period
// monthly` is the default value AND a thing a caller can type at a
// sankey, and only one of the two is an error.
func isSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// ---- column registries ---------------------------------------------------

func buildCashflowSummaryColumnRegistry(outCcy, period string) []columnSpec[gold.CashflowSummaryRow] {
	return []columnSpec[gold.CashflowSummaryRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSummaryRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSummaryRow) string { return periodStart(r.PeriodStart) }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.CashflowSummaryRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "income", Header: "income_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Income) }},
		{Name: "spending", Header: "spending_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Spending) }},
		{Name: "operating", Header: "operating_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Operating) }},
		{Name: "investing", Header: "investing_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Investing) }},
		{Name: "financing", Header: "financing_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Financing) }},
		{Name: "vehicles", Header: "vehicles_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Vehicles) }},
		{Name: "net_cash_flow", Header: "net_cash_flow_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.NetCashFlow) }},
		// The yield class alone: for a household whose money comes from
		// wealth rather than wages, the figure the report is opened for.
		{Name: "yield", Header: "yield_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Yield) }},
		{Name: "taxes", Header: "taxes_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Taxes) }},
		{Name: "fees", Header: "fees_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Fees) }},
		{Name: "giving", Header: "giving_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Giving) }},
		// A rate is a proportion, not an amount: it survives -p, which
		// is what makes the privacy twin of this report readable.
		{Name: "savings_rate", Header: "savings_rate_%", Align: output.AlignRight,
			Extract: func(r gold.CashflowSummaryRow) string { return formatPct(r.SavingsRate) }},
		// The reconciliation memo. It is the one check that can catch a
		// wrong POPULATION rather than wrong arithmetic — the pool's
		// balances are observed, and the statement's cash section is
		// not — and it is never read by net_cash_flow.
		{Name: "cash_measured", Header: "cash_measured_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.CashMeasured) }},
		{Name: "fx_effect", Header: "fx_effect_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.FXEffect) }},
		{Name: "unexplained", Header: "unexplained_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSummaryRow) string { return formatCents(r.Unexplained) }},
	}
}

var defaultCashflowSummaryColumns = []string{
	"period", "income", "spending", "operating",
	"investing", "financing", "vehicles", "net_cash_flow",
}

func resolveCashflowSummaryColumns(flagValue, outCcy, period string) ([]columnSpec[gold.CashflowSummaryRow], error) {
	return resolveColumns(flagValue, defaultCashflowSummaryColumns, buildCashflowSummaryColumnRegistry(outCcy, period))
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
		{Name: "class", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return strOrEmpty(r.ClassLabel) }},
		{Name: "group", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return strOrEmpty(r.GroupLabel) }},
		{Name: "section_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return r.Section }},
		{Name: "class_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return nodeKey(r.Section, r.Class, nil) }},
		{Name: "group_id", Align: output.AlignLeft,
			Extract: func(r gold.CashflowFlowRow) string { return nodeKey(r.Section, r.Class, r.Group) }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.CashflowFlowRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "inflow", Header: "inflow_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowFlowRow) string { return formatCents(r.Inflow) }},
		{Name: "outflow", Header: "outflow_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowFlowRow) string { return formatCents(r.Outflow) }},
		{Name: "net", Header: "net_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowFlowRow) string { return formatCents(r.Net) }},
		{Name: "share", Header: "share_%", Align: output.AlignRight,
			Extract: func(r gold.CashflowFlowRow) string { return formatPct(r.Share) }},
	}
}

var defaultCashflowFlowColumns = []string{
	"period", "section", "class", "group", "txn_count",
	"inflow", "outflow", "net", "share",
}

func resolveCashflowFlowColumns(flagValue, outCcy, period string) ([]columnSpec[gold.CashflowFlowRow], error) {
	return resolveColumns(flagValue, defaultCashflowFlowColumns, buildCashflowFlowColumnRegistry(outCcy, period))
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
		{Name: "stage", Align: output.AlignRight,
			Extract: func(r gold.CashflowSankeyRow) string { return fmt.Sprintf("%d", r.Stage) }},
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
		{Name: "section", Align: output.AlignLeft,
			Extract: func(r gold.CashflowSankeyRow) string { return strOrEmpty(r.Section) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowSankeyRow) string { return formatCents(r.Value) }},
		{Name: "share", Header: "share_%", Align: output.AlignRight,
			Extract: func(r gold.CashflowSankeyRow) string { return formatPct(r.Share) }},
	}
}

var defaultCashflowSankeyColumns = []string{"stage", "source", "target", "value", "share"}

func resolveCashflowSankeyColumns(flagValue, outCcy string) ([]columnSpec[gold.CashflowSankeyRow], error) {
	return resolveColumns(flagValue, defaultCashflowSankeyColumns, buildCashflowSankeyColumnRegistry(outCcy))
}

func buildCashflowTransactionColumnRegistry(outCcy string) []columnSpec[gold.CashflowTransactionRow] {
	return []columnSpec[gold.CashflowTransactionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return formatDate(r.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return formatDateTime(r.OccurredAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.CashflowTransactionRow) string {
				if r.DisplayName != nil && *r.DisplayName != "" {
					return *r.DisplayName
				}
				return r.AccountExternalID
			}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.CashflowTransactionRow) string { return r.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.AccountKind) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.AccountCategory) }},
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
		// person.
		{Name: "name", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Name) }},
		// The family's own value behind the node, and the two overlays
		// it could have come from.
		{Name: "detailed", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Verdict) }},
		{Name: "spend_detailed", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.SpendDetailed) }},
		{Name: "income_detailed", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.IncomeDetailed) }},
		{Name: "provenance", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Provenance) }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.CashflowTransactionRow) string { return r.Currency }},
		{Name: "net_amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowTransactionRow) string { return formatCents(r.NetAmount) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.CashflowTransactionRow) string { return formatCents(r.ValueOutCcy) }},
		{Name: "counterparty", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Counterparty) }},
		{Name: "description", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.CashflowTransactionRow) string { return strOrEmpty(r.Description) }},
		{Name: "tx_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.CashflowTransactionRow) string { return r.TransactionExternalID }},
	}
}

var defaultCashflowTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "section", "class", "group",
	"name", "currency", "net_amount", "value",
}

func resolveCashflowTransactionColumns(flagValue, outCcy string) ([]columnSpec[gold.CashflowTransactionRow], error) {
	return resolveColumns(flagValue, defaultCashflowTransactionColumns, buildCashflowTransactionColumnRegistry(outCcy))
}

func cashflowUsage() string {
	return "usage: wealthdb cashflow <view> [FROM [TO]] [--period P] [--level L] [--investing G]\n" +
		"                            [-f FORMAT] [-C COLS] [-x CCY] [-p]\n" +
		`
What the household's cash did: where it came from and where it went,
as a cash flow statement and as the edge list of a Sankey. The
household is the accounts in its own tax wrappers; retirement plans,
trusts and charitable vehicles are vehicles it pays into and draws
on. Moves between the household's own accounts are invisible; buying
and selling is shown NET per period, never as two gross bands.
Positive is cash arriving, negative is cash leaving.

Views (coarsest → finest):
  summary       one row per period bucket: income, spending, operating,
                investing, financing, vehicles, net_cash_flow
  flows         one row per bucket and node at --level, netted at that level
  sankey        the window's diagram as edges: stage, source, target, value
                (period-less; --level class for the inner two stages only)
  transactions  one row per line: section, class, group, and the family's
                own verdict behind it

Window (positional, optional; default: the trailing twelve months):
  YYYY / YYYY-MM / YYYY-MM-DD   that calendar period
  FROM TO                       explicit range; '-' is open-ended
  A household reads this per year: wealthdb cashflow sankey 2025

Flags beyond the ones every report shares:
  --level L     flows: section | class | group (default group)
                sankey: class | group (default group); section is refused
                summary and transactions ignore it
  --investing G whole (default) | class — net investing as one
                Investments node, or per asset class with one node each
                (flows and sankey; the summary is always whole)
  -f FORMAT     table | csv | csv_plain | json
  -C COLS       comma-separated names, 'default', 'all', or a
                +ADD,-REMOVE delta on the default set
  -x CCY        output currency (default: config.default_currency)
  -p            redact account ids, names and amounts; sections, classes,
                groups and shares stay legible

Notes
  The four sections sum to net_cash_flow, and a bucket's flows rows —
  the Cash row included — sum to zero. Both identities are structural
  and guard arithmetic rather than population: the reconciliation that
  can catch a population hole is -C +cash_measured,+fx_effect,+unexplained
  on the summary.

  A NODE IS A NET, and the level decides what nets. A class with a
  large gross and a small net is one node, not two bands; inflow and
  outflow beside it are the line-level gross under it. share_% is over
  the hub at the level drawn, so a finer level can have a larger hub
  than a coarser one — that is what netting means.

  Totals here are smaller than 'wealthdb income' and 'wealthdb
  spending' report, by four terms: the vehicles' own income and
  spending, the reimbursements cashflow keeps and income drops, the
  verdicts cashflow re-homes, and any account the families' own scopes
  exclude but the pool keeps.

  Tax a vehicle withheld before the household saw the money is on no
  node at all, so the Taxes node is the tax the household paid from its
  own accounts.

Available columns (per view):
  summary       ` + joinColumnNames(buildCashflowSummaryColumnRegistry("CCY", "monthly")) + `
  flows         ` + joinColumnNames(buildCashflowFlowColumnRegistry("CCY", "monthly")) + `
  sankey        ` + joinColumnNames(buildCashflowSankeyColumnRegistry("CCY")) + `
  transactions  ` + joinColumnNames(buildCashflowTransactionColumnRegistry("CCY")) + `

  (The money columns render as income_<CCY> / net_<CCY> / value_<CCY>,
   reflecting your -x/--currency choice.)

Default column sets:
  summary       ` + strings.Join(defaultCashflowSummaryColumns, ", ") + `
  flows         ` + strings.Join(defaultCashflowFlowColumns, ", ") + `
  sankey        ` + strings.Join(defaultCashflowSankeyColumns, ", ") + `
  transactions  ` + strings.Join(defaultCashflowTransactionColumns, ", ")
}
