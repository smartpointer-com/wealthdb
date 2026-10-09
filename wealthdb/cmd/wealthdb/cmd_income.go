package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// `wealthdb income <view>` — the read surface for what the tracked
// accounts received.
//
// cmd_spending.go read in the other direction, and deliberately not a
// generalisation of it: the two commands share the flag idiom, the
// period vocabulary, the window default and the column machinery
// (reportValueFlags, reportPeriods, periodLabel,
// parseTrailingYearWindow, the report seam), and differ in the three
// places a reader should be able to see at a glance — the views, the
// registries, and the usage text.
//
// One default differs from spending's, and it is the one decision in
// this file: `--level` is `detailed` rather than `primary`. The income
// vocabulary has ONE vendored primary, so at the primary level every
// vendored type and extension folds into INCOME beside `gift`,
// `inheritance` and `cash_deposit`. That view has a use — what was
// earned or yielded, against what was given — but it is not the one a
// reader opens the report for.
func init() {
	register("income", cmdIncome)
}

var incomeViews = map[string]bool{
	"summary": true, "types": true, "transactions": true,
}

// cmdIncome routes `wealthdb income <view> ...` to the shared runner.
func cmdIncome(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		fmt.Fprintln(stderr, incomeUsage())
		return errs.Newf(2, "income: a view subcommand is required")
	}
	view, rest := subargs[0], subargs[1:]
	switch view {
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, incomeUsage())
		return nil
	}
	if !incomeViews[view] {
		fmt.Fprintln(stderr, incomeUsage())
		return errs.Newf(2, "income: unknown view %q (want summary | types | transactions)", view)
	}
	return runIncomeView(ctx, g, view, rest, stdout, stderr)
}

func runIncomeView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb income "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)

	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	level := fs.String("level", "detailed", "primary | detailed — the type vocabulary (types view)")
	rf := registerReportFlags(fs, "redact account IDs, payers, and monetary amounts (types stay visible)")

	fs.Usage = func() { fmt.Fprintln(stderr, incomeUsage()) }
	reordered := reorderFlagsFirst(splitFusedColumnsFlag(args), reportValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "income: bad flags")
	}

	if _, ok := reportPeriods[*period]; !ok {
		return errs.Newf(2, "income: invalid --period %q (want %s)",
			*period, strings.Join(reportPeriodNames, " | "))
	}
	if !oneOf(*level, "primary", "detailed") {
		return errs.Newf(2, "income: invalid --level %q (want primary | detailed)", *level)
	}

	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "income: %s", err.Error())
	}

	fmtChoice, cfg, outCcy, err := rf.resolve(g, "income")
	if err != nil {
		return err
	}

	rep := incomeReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch, period: *period, level: *level})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "income", open, *rf.privacy, fmtChoice, stdout)
}

// incomeReport is one view of the income family, the runner the CLI
// and the MCP server share.
func incomeReport(req request) *report {
	part := reportPeriods[req.period]
	switch req.view {
	case "summary":
		return newReport(buildIncomeSummaryColumnRegistry(req.currency, req.period), defaultIncomeSummaryColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.IncomeSummaryRow, error) {
				return gold.IncomeSummary(ctx, db, req.from, req.to, req.currency, part)
			})
	case "types":
		return newReport(buildIncomeTypeColumnRegistry(req.currency, req.period), defaultIncomeTypeColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.IncomeTypeRow, error) {
				return gold.IncomeTypes(ctx, db, req.from, req.to, req.currency, part, req.level)
			})
	default:
		return newReport(buildIncomeTransactionColumnRegistry(req.currency), defaultIncomeTransactionColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.IncomeTransactionRow, error) {
				return gold.IncomeTransactions(ctx, db, req.from, req.to, req.currency)
			})
	}
}

// ---- column registries ---------------------------------------------------

func buildIncomeSummaryColumnRegistry(outCcy, period string) []columnSpec[gold.IncomeSummaryRow] {
	return []columnSpec[gold.IncomeSummaryRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.IncomeSummaryRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.IncomeSummaryRow) string { return periodStart(r.PeriodStart) }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.IncomeSummaryRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "income", Header: "income_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeSummaryRow) string { return formatCents(r.Income) }},
		{Name: "reversals", Header: "reversals_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeSummaryRow) string { return formatCents(r.Reversals) }},
		{Name: "net_income", Header: "net_income_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeSummaryRow) string { return formatCents(r.NetIncome) }},
		// A memo, off by default. It is tax the household never saw,
		// shown beside the income it was withheld from — and it is
		// never subtracted from net_income, which is what "gross as
		// booked" means (docs/INCOME.md §5).
		{Name: "withheld", Header: "withheld_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeSummaryRow) string { return formatCents(r.Withheld) }},
	}
}

var defaultIncomeSummaryColumns = []string{
	"period", "txn_count", "income", "reversals", "net_income",
}

func buildIncomeTypeColumnRegistry(outCcy, period string) []columnSpec[gold.IncomeTypeRow] {
	return []columnSpec[gold.IncomeTypeRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTypeRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTypeRow) string { return periodStart(r.PeriodStart) }},
		// `type` is what a reader sees and `type_id` is the key they
		// would group or join on — the same split the spending
		// categories view makes between its label and its value.
		{Name: "type", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTypeRow) string { return r.TypeLabel }},
		{Name: "type_id", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTypeRow) string { return r.Type }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.IncomeTypeRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "income", Header: "income_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeTypeRow) string { return formatCents(r.Income) }},
		{Name: "reversals", Header: "reversals_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeTypeRow) string { return formatCents(r.Reversals) }},
		{Name: "net_income", Header: "net_income_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeTypeRow) string { return formatCents(r.NetIncome) }},
		// A share is a proportion, not an amount: it survives -p, which
		// is what makes the privacy twin of this report readable.
		{Name: "share", Header: "share_pct", Align: output.AlignRight,
			Extract: func(r gold.IncomeTypeRow) string { return formatPct(r.Share) }},
	}
}

var defaultIncomeTypeColumns = []string{
	"period", "type", "txn_count", "income", "reversals", "net_income", "share",
}

func buildIncomeTransactionColumnRegistry(outCcy string) []columnSpec[gold.IncomeTransactionRow] {
	return []columnSpec[gold.IncomeTransactionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return formatDate(r.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return formatDateTime(r.OccurredAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.IncomeTransactionRow) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.IncomeTransactionRow) string { return r.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.AccountKind) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.AccountCategory) }},
		{Name: "kind", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return r.Kind }},
		// The payer takes the FREE-TEXT class, and that most of the
		// base fills it with an instrument name does not change it: a
		// column takes the class of the worst thing it can hold. On a
		// deposit the column is the payer store's name for the
		// signature or, where the store has none, the fold of the
		// narrative itself — which on an inbound wire is a person.
		{Name: "payer", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.PayerName) }},
		{Name: "payer_signature", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.PayerSignature) }},
		{Name: "income_type", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.IncomeLabel) }},
		{Name: "income_type_id", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.IncomeDetailed) }},
		{Name: "income_primary", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.IncomePrimaryLabel) }},
		{Name: "income_primary_id", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.IncomePrimary) }},
		{Name: "provenance", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.Provenance) }},
		// The source's own filing, beside ours and never summed with
		// it.
		{Name: "provider_income_type", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.ProviderIncomeLabel) }},
		{Name: "provider_income_type_id", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.ProviderIncomeDetailed) }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return r.Currency }},
		{Name: "net_amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeTransactionRow) string { return formatCents(r.NetAmount) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeTransactionRow) string { return formatCents(r.ValueOutCcy) }},
		// The raw narrative the signature was folded from. Free text on
		// both counts: it is what a bank printed, and on a deposit that
		// is whoever sent the money.
		{Name: "counterparty", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.Counterparty) }},
		{Name: "description", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.IncomeTransactionRow) string { return strOrEmpty(r.Description) }},
		// The source's own id for the line. INCOME.md §8 sends a reader
		// to `wealthdb transactions` to see both families on one row,
		// and without this there is no key to join the two listings on.
		// Spending's transactions view and `wealthdb transactions` both
		// offer it.
		{Name: "tx_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.IncomeTransactionRow) string { return r.TransactionExternalID }},
	}
}

var defaultIncomeTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "payer", "income_type",
	"provenance", "currency", "net_amount", "value",
}

func incomeUsage() string {
	return "usage: wealthdb income <view> [FROM [TO]] [--period P] [--level L]\n" +
		"                          [-f FORMAT] [-C COLS] [-x CCY] [-p]\n" +
		`
What the tracked accounts received: wages, interest, dividends, staking
rewards, rent, gifts — consolidated across every source, typed and
attributed to a payer.

Views
  summary       one row per period bucket
  types         one row per (bucket, income type), with its share
  transactions  the income lines themselves, oldest first

Window
  FROM and TO are ISO dates, a year, or the usual shorthands; a bare
  invocation reports the trailing twelve months. Income is often read
  per calendar year: wealthdb income types 2025 --period annual

Flags
  --period P    ` + strings.Join(reportPeriodNames, " | ") + ` (default monthly)
  --level L     primary | detailed (default detailed) — the type
                vocabulary the types view groups by. The income
                taxonomy has one vendored primary, so the primary level
                folds every earned and yielded type into INCOME beside
                the deltas.
  -f FORMAT     table | csv | csv_plain | json
  -C COLS       comma-separated names, 'default', 'all', or a
                +ADD,-REMOVE delta on the default set
  -x CCY        output currency (default: config.default_currency)
  -p            redact account ids, payers and amounts; types, shares
                and provenance stay legible

Notes
  Income is GROSS as booked. Tax withheld at source is the spending
  side's, and -C +withheld shows it beside the income it was taken
  from without ever subtracting it. The memo is every NEGATIVE tax-kind
  row of the window on these accounts, negated to read positive — what a
  brokerage books withholding as, and what UBS also books a transaction
  tax as.

  A summary bucket can show txn_count 0 and blank money columns: that
  is a period in which tax was withheld and no income arrived. -C
  +withheld shows what put it there.

  income and reversals are positive magnitudes; net_income is the
  difference. A negative row of an income kind — a dividend clawed
  back, a credit reversed — nets inside its own type.

  A receipt no tier could place reads (uncategorized) rather than being
  guessed at; 'wealthdb categorize income' is what works that backlog
  down.

Available columns (per view):
  summary       ` + joinColumnNames(buildIncomeSummaryColumnRegistry("CCY", "monthly")) + `
  types         ` + joinColumnNames(buildIncomeTypeColumnRegistry("CCY", "monthly")) + `
  transactions  ` + joinColumnNames(buildIncomeTransactionColumnRegistry("CCY")) + `

  (The money columns render as income_<CCY> / net_income_<CCY> /
   value_<CCY>, reflecting your -x/--currency choice.)

Default column sets:
  summary       ` + strings.Join(defaultIncomeSummaryColumns, ", ") + `
  types         ` + strings.Join(defaultIncomeTypeColumns, ", ") + `
  transactions  ` + strings.Join(defaultIncomeTransactionColumns, ", ")
}
