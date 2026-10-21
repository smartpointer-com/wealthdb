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

// `wealthdb income <view>` — the read surface for what the tracked
// accounts received.
//
// cmd_spending.go read in the other direction, and deliberately not a
// generalisation of it: the two commands share the flag idiom, the
// period vocabulary, the window default and the column machinery
// (reportPeriods, periodLabel, parseTrailingYearWindow, the report
// seam), and differ in the three
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

// incomeViews are the grains `wealthdb income <view>` supports: what was
// received, coarsest first, then the forward views, which take a window
// that may reach into the future (cmd_income_projection.go).
var incomeViews = []string{"summary", "types", "transactions", "projection", "calendar", "sources", "portfolios",
	"accounts", "positions"}

// cmdIncome routes `wealthdb income <view> ...` to the view's run.
func cmdIncome(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("income", subargs, incomeViews, incomeUsage, stderr)
	if err != nil {
		return err
	}
	return runIncomeView(ctx, g, view, rest, stdout, stderr)
}

func runIncomeView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb income "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	level := fs.String("level", "detailed", "primary | detailed — the type vocabulary (types view)")
	ff := registerForwardFlags(fs)
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, incomeUsage()) }
	if err := parseFlags(fs, args, "income"); err != nil {
		return err
	}
	if err := checkChoice("income", "period", *period, reportPeriodNames...); err != nil {
		return err
	}
	if err := checkChoice("income", "level", *level, "primary", "detailed"); err != nil {
		return err
	}
	forward := isForwardView(view)
	if err := ff.check(fs, forward); err != nil {
		return err
	}
	if forward && view != "projection" && isSet(fs, "period") {
		return errs.Newf(errs.ExitUsage, "income: of the forward views, --period belongs to projection")
	}
	if forward && !slices.Contains(projectionPeriods, *period) {
		return errs.Newf(errs.ExitUsage, "income: the forward views take --period %s", strings.Join(projectionPeriods, " | "))
	}

	fmtChoice, cfg, outCcy, err := rf.resolve(g, "income")
	if err != nil {
		return err
	}
	now := time.Now()
	var fromEpoch, toEpoch int64
	switch {
	case !forward:
		fromEpoch, toEpoch, err = parseTrailingYearWindow(fs.Args(), now)
	case fs.NArg() > 2:
		err = fmt.Errorf("a window is FROM [TO]")
	default:
		fromEpoch, toEpoch, err = parseForwardWindow(fs.Arg(0), fs.Arg(1), now, cfg.YieldHorizon())
	}
	if err != nil {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "income: %s", err.Error())
	}

	rep := incomeReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch, period: *period, level: *level,
		asOf: anchorToDay(now.UTC(), true).Unix(), assumptions: ff.assumptions()})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "income", open, *rf.privacy, fmtChoice, stdout)
}

// incomeReport is one view of the income family, the runner the CLI
// and the MCP server share.
func incomeReport(req request) *report {
	part := reportPeriods[req.period]
	if isForwardView(req.view) {
		return incomeForwardReport(req)
	}
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
		countCol("txn_count", func(r gold.IncomeSummaryRow) int64 { return r.TxnCount }),
		outCcyCol("income", outCcy, func(r gold.IncomeSummaryRow) *string { return r.Income }),
		outCcyCol("reversals", outCcy, func(r gold.IncomeSummaryRow) *string { return r.Reversals }),
		outCcyCol("net_income", outCcy, func(r gold.IncomeSummaryRow) *string { return r.NetIncome }),
		// A memo, off by default. It is tax the household never saw,
		// shown beside the income it was withheld from — and it is
		// never subtracted from net_income, which is what "gross as
		// booked" means (docs/INCOME.md §5).
		outCcyCol("withheld", outCcy, func(r gold.IncomeSummaryRow) *string { return r.Withheld }),
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
		countCol("txn_count", func(r gold.IncomeTypeRow) int64 { return r.TxnCount }),
		outCcyCol("income", outCcy, func(r gold.IncomeTypeRow) *string { return r.Income }),
		outCcyCol("reversals", outCcy, func(r gold.IncomeTypeRow) *string { return r.Reversals }),
		outCcyCol("net_income", outCcy, func(r gold.IncomeTypeRow) *string { return r.NetIncome }),
		ratioCol("share", func(r gold.IncomeTypeRow) *float64 { return r.Share }),
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
		textCol("account_kind", func(r gold.IncomeTransactionRow) *string { return r.AccountKind }),
		textCol("account_nickname", func(r gold.IncomeTransactionRow) *string { return r.Nickname }),
		textCol("account_category", func(r gold.IncomeTransactionRow) *string { return r.AccountCategory }),
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
		textCol("income_type", func(r gold.IncomeTransactionRow) *string { return r.IncomeLabel }),
		textCol("income_type_id", func(r gold.IncomeTransactionRow) *string { return r.IncomeDetailed }),
		textCol("income_primary", func(r gold.IncomeTransactionRow) *string { return r.IncomePrimaryLabel }),
		textCol("income_primary_id", func(r gold.IncomeTransactionRow) *string { return r.IncomePrimary }),
		textCol("provenance", func(r gold.IncomeTransactionRow) *string { return r.Provenance }),
		// The source's own filing, beside ours and never summed with
		// it.
		textCol("provider_income_type", func(r gold.IncomeTransactionRow) *string { return r.ProviderIncomeLabel }),
		textCol("provider_income_type_id", func(r gold.IncomeTransactionRow) *string { return r.ProviderIncomeDetailed }),
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.IncomeTransactionRow) string { return r.Currency }},
		moneyCol("net_amount", "", func(r gold.IncomeTransactionRow) *string { return r.NetAmount }),
		outCcyCol("value", outCcy, func(r gold.IncomeTransactionRow) *string { return r.ValueOutCcy }),
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
		"                          [--rates R] [--maturing M] [--shift BP]\n" +
		"                          [-f FORMAT] [-C COLS] [-x CCY] [-p]\n" +
		`
What the tracked accounts received: wages, interest, dividends, staking
rewards, rent, gifts — consolidated across every source, typed and
attributed to a payer. The forward views add what the holdings are
expected to pay (docs/INCOME.md §12).

Views
  summary       one row per period bucket
  types         one row per (bucket, income type), with its share
  transactions  the income lines themselves, oldest first
  projection    one row per month, quarter or year: received to today,
                expected after it, per type
  calendar      each expected payment and each principal a maturity
                returns, in date order
  sources, portfolios, accounts, positions
                received, expected, last year's and the next twelve
                months' per entity; positions lists every holding

Window
  FROM and TO are ISO dates, a year, or the usual shorthands; a bare
  invocation reports the trailing twelve months. Income is often read
  per calendar year: wealthdb income types 2025 --period annual
  A forward view's window may reach into the future: a lone FROM is its
  own span, and a bare invocation the projection's horizon after the
  current month (yield.horizon_months, default 12, at most 36). It
  takes no open bound (-), and of the forward views only projection
  takes --period:
  wealthdb income projection 2027 --period quarterly

Flags
  --period P    ` + strings.Join(reportPeriodNames, " | ") + ` (default monthly)
  --level L     primary | detailed (default detailed) — the type
                vocabulary the types view groups by. The income
                taxonomy has one vendored primary, so the primary level
                folds every earned and yielded type into INCOME beside
                the deltas.
  --rates R     ` + strings.Join(pathNames(), " | ") + ` (default constant) — how
                floating rates move, on the forward views
  --maturing M  ` + strings.Join(ruleNames(), " | ") + ` (default cash) — what a
                maturing principal earns, on the forward views
  --shift BP    a parallel shift of the floating rates in basis points,
                on the forward views
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

  On the forward views, withheld is what the yield.withholding table
  withholds at each holding's situs, received and expected alike; it is
  blank where the table names no rate.

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
  projection    ` + joinColumnNames(buildProjectionColumns("CCY", "monthly")) + `
  calendar      ` + joinColumnNames(buildCalendarColumns("CCY")) + `
  sources       ` + joinColumnNames(buildIncomeEntityColumns("CCY", yield.GrainSources, nil)) + `
  portfolios    ` + joinColumnNames(buildIncomeEntityColumns("CCY", yield.GrainPortfolios, nil)) + `
  accounts      ` + joinColumnNames(buildIncomeEntityColumns("CCY", yield.GrainAccounts, nil)) + `
  positions     ` + joinColumnNames(buildIncomeEntityColumns("CCY", yield.GrainPositions, nil)) + `

  (The money columns render as income_<CCY> / net_income_<CCY> /
   value_<CCY>, reflecting your -x/--currency choice.)

Default column sets:
  summary       ` + strings.Join(defaultIncomeSummaryColumns, ", ") + `
  types         ` + strings.Join(defaultIncomeTypeColumns, ", ") + `
  transactions  ` + strings.Join(defaultIncomeTransactionColumns, ", ") + `
  projection    ` + strings.Join(defaultProjectionColumns, ", ") + `
  calendar      ` + strings.Join(defaultCalendarColumns, ", ") + `
  sources       ` + strings.Join(incomeEntityDefaults[yield.GrainSources], ", ") + `
  portfolios    ` + strings.Join(incomeEntityDefaults[yield.GrainPortfolios], ", ") + `
  accounts      ` + strings.Join(incomeEntityDefaults[yield.GrainAccounts], ", ") + `
  positions     ` + strings.Join(incomeEntityDefaults[yield.GrainPositions], ", ")
}
