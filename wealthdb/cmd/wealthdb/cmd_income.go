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

// `wealthdb income <view>` — the read surface for what the tracked
// accounts received.
//
// cmd_spending.go read in the other direction, and deliberately not a
// generalisation of it: the two commands share the flag idiom, the
// period vocabulary, the window default and the column machinery
// (reportValueFlags, reportPeriods, periodLabel,
// parseTrailingYearWindow, resolveColumns), and differ in the three
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
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	privacy := fs.Bool("p", false, "redact account IDs, payers, and monetary amounts (types stay visible)")
	fs.BoolVar(privacy, "privacy", false, "redact account IDs, payers, and monetary amounts (types stay visible)")

	fs.Usage = func() { fmt.Fprintln(stderr, incomeUsage()) }
	reordered := reorderFlagsFirst(splitFusedColumnsFlag(args), reportValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "income: bad flags")
	}

	part, ok := reportPeriods[*period]
	if !ok {
		return errs.Newf(2, "income: invalid --period %q (want %s)",
			*period, strings.Join(reportPeriodNames, " | "))
	}
	if !oneOf(*level, "primary", "detailed") {
		return errs.Newf(2, "income: invalid --level %q (want primary | detailed)", *level)
	}
	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "income: %s", err.Error())
	}

	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "income: %s", err.Error())
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
		return errs.Newf(2, "income: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	switch view {
	case "summary":
		colSet, err := resolveIncomeSummaryColumns(*cols, outCcy, *period)
		if err != nil {
			return errs.Newf(2, "income: %s", err.Error())
		}
		rows, err := gold.IncomeSummary(ctx, db, fromEpoch, toEpoch, outCcy, part)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	case "types":
		colSet, err := resolveIncomeTypeColumns(*cols, outCcy, *period)
		if err != nil {
			return errs.Newf(2, "income: %s", err.Error())
		}
		rows, err := gold.IncomeTypes(ctx, db, fromEpoch, toEpoch, outCcy, part, *level)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
	default:
		colSet, err := resolveIncomeTransactionColumns(*cols, outCcy)
		if err != nil {
			return errs.Newf(2, "income: %s", err.Error())
		}
		rows, err := gold.IncomeTransactions(ctx, db, fromEpoch, toEpoch, outCcy)
		if err != nil {
			return err
		}
		return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
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
		// booked" means (docs/INCOME.md §3).
		{Name: "withheld", Header: "withheld_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.IncomeSummaryRow) string { return formatCents(r.Withheld) }},
	}
}

var defaultIncomeSummaryColumns = []string{
	"period", "txn_count", "income", "reversals", "net_income",
}

func resolveIncomeSummaryColumns(flagValue, outCcy, period string) ([]columnSpec[gold.IncomeSummaryRow], error) {
	return resolveColumns(flagValue, defaultIncomeSummaryColumns, buildIncomeSummaryColumnRegistry(outCcy, period))
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
		{Name: "share", Align: output.AlignRight,
			Extract: func(r gold.IncomeTypeRow) string { return formatPct(r.Share) }},
	}
}

var defaultIncomeTypeColumns = []string{
	"period", "type", "txn_count", "income", "reversals", "net_income", "share",
}

func resolveIncomeTypeColumns(flagValue, outCcy, period string) ([]columnSpec[gold.IncomeTypeRow], error) {
	return resolveColumns(flagValue, defaultIncomeTypeColumns, buildIncomeTypeColumnRegistry(outCcy, period))
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
			Extract: func(r gold.IncomeTransactionRow) string {
				if r.DisplayName != nil && *r.DisplayName != "" {
					return *r.DisplayName
				}
				return r.AccountExternalID
			}},
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
	}
}

var defaultIncomeTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "payer", "income_type",
	"provenance", "currency", "net_amount", "value",
}

func resolveIncomeTransactionColumns(flagValue, outCcy string) ([]columnSpec[gold.IncomeTransactionRow], error) {
	return resolveColumns(flagValue, defaultIncomeTransactionColumns, buildIncomeTransactionColumnRegistry(outCcy))
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
  -C COLS       comma-separated column names, or 'default' / 'all'
  -x CCY        output currency (default: config.default_currency)
  -p            redact account ids, payers and amounts; types, shares
                and provenance stay legible

Notes
  Income is GROSS as booked. Tax withheld at source is the spending
  side's, and -C +withheld shows it beside the income it was taken
  from without ever subtracting it.

  income and reversals are positive magnitudes; net_income is the
  difference. A negative row of an income kind — a dividend clawed
  back, a credit reversed — nets inside its own type.

  A receipt no tier could place reads (uncategorized) rather than being
  guessed at; 'wealthdb categorize income' is what works that backlog
  down.`
}
