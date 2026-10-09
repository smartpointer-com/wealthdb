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

func init() {
	register("spending", cmdSpending)
}

// spendingViews are the grains `wealthdb spending <view>` supports —
// the three reports migration 0042 defines over the spending
// population, coarsest first.
var spendingViews = map[string]bool{
	"summary": true, "categories": true, "transactions": true,
}

// reportValueFlags are the flag tokens that consume the next arg, so
// the positional [FROM [TO]] window may appear before or after flags
// (the returns / transactions convention). Shared by every report
// family: the flag idiom is the CLI's, not one command's.
var reportValueFlags = map[string]bool{
	"-f": true, "--format": true, "-C": true, "--columns": true,
	"-x": true, "--currency": true, "--period": true, "--level": true,
	"--investing": true,
}

// reportPeriods maps the CLI's bucket vocabulary onto the date_trunc
// parts the report macros bucket by. The names are the returns
// family's, extended down to daily / weekly: a spending or income
// report is read at a finer grain than a return, where a quarter is
// the coarsest interesting bucket.
var reportPeriods = map[string]string{
	"daily": "day", "weekly": "week", "monthly": "month",
	"quarterly": "quarter", "annual": "year", "total": "total",
}

// reportPeriodNames is reportPeriods' key set in display order,
// for usage text and error messages.
var reportPeriodNames = []string{"daily", "weekly", "monthly", "quarterly", "annual", "total"}

// cmdSpending routes `wealthdb spending <view> ...` to the shared
// runner, mirroring the returns dispatcher.
func cmdSpending(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		fmt.Fprintln(stderr, spendingUsage())
		return errs.Newf(2, "spending: a view subcommand is required")
	}
	view, rest := subargs[0], subargs[1:]
	switch view {
	case "-h", "--help", "help":
		fmt.Fprintln(stderr, spendingUsage())
		return nil
	}
	if !spendingViews[view] {
		fmt.Fprintln(stderr, spendingUsage())
		return errs.Newf(2, "spending: unknown view %q (want summary | categories | transactions)", view)
	}
	return runSpendingView(ctx, g, view, rest, stdout, stderr)
}

func runSpendingView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb spending "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)

	period := fs.String("period", "monthly", strings.Join(reportPeriodNames, " | "))
	level := fs.String("level", "primary", "primary | detailed — the category vocabulary (categories view)")
	rf := registerReportFlags(fs, "redact account IDs, counterparties, and monetary amounts (categories stay visible)")

	fs.Usage = func() { fmt.Fprintln(stderr, spendingUsage()) }
	reordered := reorderFlagsFirst(splitFusedColumnsFlag(args), reportValueFlags)
	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "spending: bad flags")
	}

	if _, ok := reportPeriods[*period]; !ok {
		return errs.Newf(2, "spending: invalid --period %q (want %s)",
			*period, strings.Join(reportPeriodNames, " | "))
	}
	if !oneOf(*level, "primary", "detailed") {
		return errs.Newf(2, "spending: invalid --level %q (want primary | detailed)", *level)
	}

	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), time.Now())
	if err != nil {
		fs.Usage()
		return errs.Newf(2, "spending: %s", err.Error())
	}

	fmtChoice, cfg, outCcy, err := rf.resolve(g, "spending")
	if err != nil {
		return err
	}

	rep := spendingReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch, period: *period, level: *level})
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "spending", open, *rf.privacy, fmtChoice, stdout)
}

// spendingReport is one view of the spending family, the runner the
// CLI and the MCP server share.
func spendingReport(req request) *report {
	part := reportPeriods[req.period]
	switch req.view {
	case "summary":
		return newReport(buildSpendSummaryColumnRegistry(req.currency, req.period), defaultSpendSummaryColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.SpendSummaryRow, error) {
				return gold.SpendingSummary(ctx, db, req.from, req.to, req.currency, part)
			})
	case "categories":
		return newReport(buildSpendCategoryColumnRegistry(req.currency, req.period), defaultSpendCategoryColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.SpendCategoryRow, error) {
				return gold.SpendingCategories(ctx, db, req.from, req.to, req.currency, part, req.level)
			})
	default:
		return newReport(buildSpendTransactionColumnRegistry(req.currency), defaultSpendTransactionColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.SpendTransactionRow, error) {
				return gold.SpendingTransactions(ctx, db, req.from, req.to, req.currency)
			})
	}
}

// parseTrailingYearWindow defaults a bare invocation to the trailing
// twelve months — the same day one year ago through today — otherwise
// reuses the transactions-style positional range. It does NOT default
// to since-inception the way the returns window does: these reports
// answer "what is happening lately", and a window reaching back past
// the day a source's ledger begins covers a different population, so a
// total over it would read as a different product.
func parseTrailingYearWindow(args []string, now time.Time) (int64, int64, error) {
	if len(args) == 0 {
		nowUTC := now.UTC()
		from := anchorToDay(nowUTC.AddDate(-1, 0, 0), false).Unix()
		return from, anchorToDay(nowUTC, true).Unix(), nil
	}
	return parseDateRange(args, now)
}

// periodLabel renders a bucket the way the returns family's
// `period` column does: the calendar label of the bucket the epoch
// second opens, and "total" for the single NULL bucket --period total
// emits. Daily and weekly buckets label as their opening date (DuckDB
// truncates a week to its Monday).
func periodLabel(periodStart *int64, period string) string {
	if periodStart == nil {
		return "total"
	}
	t := time.Unix(*periodStart, 0).UTC()
	switch period {
	case "daily", "weekly":
		return t.Format("2006-01-02")
	case "monthly":
		return t.Format("2006-01")
	case "annual":
		return t.Format("2006")
	default: // quarterly
		return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
	}
}

// periodStart renders the bucket's opening date, empty for the
// `total` bucket, which has no start beyond the window's own.
func periodStart(periodStart *int64) string {
	if periodStart == nil {
		return ""
	}
	return formatDate(*periodStart)
}

// ---- column registries ---------------------------------------------------

// buildSpendSummaryColumnRegistry needs `period` as well as the output
// currency: the bucket label's shape follows the bucket's size, the
// same way the money headers follow -x/--currency.
func buildSpendSummaryColumnRegistry(outCcy, period string) []columnSpec[gold.SpendSummaryRow] {
	return []columnSpec[gold.SpendSummaryRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.SpendSummaryRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.SpendSummaryRow) string { return periodStart(r.PeriodStart) }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.SpendSummaryRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "spend", Header: "spend_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendSummaryRow) string { return formatCents(r.Spend) }},
		{Name: "refunds", Header: "refunds_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendSummaryRow) string { return formatCents(r.Refunds) }},
		{Name: "net_spend", Header: "net_spend_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendSummaryRow) string { return formatCents(r.NetSpend) }},
	}
}

var defaultSpendSummaryColumns = []string{
	"period", "txn_count", "spend", "refunds", "net_spend",
}

func buildSpendCategoryColumnRegistry(outCcy, period string) []columnSpec[gold.SpendCategoryRow] {
	return []columnSpec[gold.SpendCategoryRow]{
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.SpendCategoryRow) string { return periodLabel(r.PeriodStart, period) }},
		{Name: "period_start", Align: output.AlignLeft,
			Extract: func(r gold.SpendCategoryRow) string { return periodStart(r.PeriodStart) }},
		// The category is taxonomy — a vocabulary value, not an
		// identifier — and stays legible under -p, like asset_class
		// and the other taxonomy labels. Two spellings of the same
		// thing: `category` is what a report reads as, `category_id`
		// the value it groups on. The label is the default because it
		// is what a person reads; the id is there for a caller
		// scripting against a stable key.
		{Name: "category", Align: output.AlignLeft,
			Extract: func(r gold.SpendCategoryRow) string { return r.CategoryLabel }},
		{Name: "category_id", Align: output.AlignLeft,
			Extract: func(r gold.SpendCategoryRow) string { return r.Category }},
		{Name: "txn_count", Align: output.AlignRight,
			Extract: func(r gold.SpendCategoryRow) string { return fmt.Sprintf("%d", r.TxnCount) }},
		{Name: "spend", Header: "spend_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendCategoryRow) string { return formatCents(r.Spend) }},
		{Name: "refunds", Header: "refunds_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendCategoryRow) string { return formatCents(r.Refunds) }},
		{Name: "net_spend", Header: "net_spend_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendCategoryRow) string { return formatCents(r.NetSpend) }},
		// A share is a ratio, not an amount — it stays visible under
		// -p exactly as the returns percentages do.
		{Name: "share", Header: "share_pct", Align: output.AlignRight,
			Extract: func(r gold.SpendCategoryRow) string { return formatPct(r.Share) }},
	}
}

var defaultSpendCategoryColumns = []string{
	"period", "category", "txn_count", "spend", "refunds", "net_spend", "share",
}

func buildSpendTransactionColumnRegistry(outCcy string) []columnSpec[gold.SpendTransactionRow] {
	return []columnSpec[gold.SpendTransactionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return r.SilverSourceID }},
		{Name: "date", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return formatDate(r.OccurredAt) }},
		{Name: "datetime", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return formatDateTime(r.OccurredAt) }},
		// A card's display name is "<product> ****1234" on most
		// issuers, so the account columns carry an account number and
		// redact as one — the same class the account label takes
		// everywhere else in the CLI.
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.SpendTransactionRow) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.SpendTransactionRow) string { return r.AccountExternalID }},
		{Name: "account_kind", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.AccountKind) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.AccountCategory) }},
		{Name: "kind", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return r.Kind }},
		// merchant_name redacts as free text, like the narrative it
		// was named from. The fence (spending.RowTransferShaped) gates
		// candidacy for the merchant STORE, not this column: a wire,
		// an ACH, a P2P narrative is refused a verdict, has no store
		// name, and falls back to the signature folded from that very
		// narrative (migration 0054) — so a payment to a person prints
		// its payee here. A store name carries the same exposure by a
		// slower route: the store is append-only across signature
		// revisions and across widenings of the fence itself, and the
		// enrichment lookup applies a stored verdict by signature
		// forever, so a name bought while the fence was narrower
		// outlives the fence that would now refuse it, and the model
		// wrote it from the narrative it was shown. Either way the
		// column carries no guarantee about what is in it, and -p
		// treats it as what it is: a name taken off a statement line.
		{Name: "merchant", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.MerchantName) }},
		// The signature is the opposite case: it is computed for
		// EVERY enriched row, fenced or not, as a fold of the raw
		// narrative — so it carries whatever the narrative carried.
		// Same class as counterparty.
		{Name: "merchant_signature", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.MerchantSignature) }},
		{Name: "spend_primary", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.SpendPrimary) }},
		{Name: "spend_detailed", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.SpendDetailed) }},
		// The same two values as they read (migration 0058).
		{Name: "category", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.SpendLabel) }},
		{Name: "category_primary", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.SpendPrimaryLabel) }},
		// The ISSUER's own classification of the line, mapped into our
		// vocabulary and kept beside ours (migration 0057). Off by
		// default: it is a second opinion, it disagrees with ours by
		// design, and nothing may sum the two. Empty where the issuer
		// published nothing this build translates — which is not the
		// same as the issuer filing the row under a catch-all.
		//
		// Named `issuer_*` rather than `provider_category`, which
		// everywhere else in the product — `transactions.provider_category`
		// (migration 0038), the field a config rule matches — means the
		// issuer's RAW string. These two are our translation of it.
		{Name: "issuer_category", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.ProviderSpendLabel) }},
		{Name: "issuer_category_id", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.ProviderSpendDetailed) }},
		// Which tier decided the category (matcher / rule / provider /
		// signature-only / model / manual) — vocabulary, not data.
		// Five of the six are stamped on the overlay row by the
		// enrichment pass; `model` is not stored anywhere, because the
		// model tier writes to the merchant store rather than the
		// overlay. spend_txn_categories() resolves it at the point the
		// two scopes meet (migration 0050), which is the only place the
		// store's verdict is distinguishable from the backlog.
		{Name: "provenance", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.Provenance) }},
		// The counterparty is the adapter's merchant field, published
		// unfiltered: on a card row it is a shop, on a transfer it is
		// a person. Nothing upstream separates the two, so it takes
		// the free-text class, which masks the cell whole — a wire or
		// P2P narrative is multi-word, and any shape-based rule would
		// wave exactly those through.
		{Name: "counterparty", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.Counterparty) }},
		// The statement narrative the signature was folded from —
		// same exposure as the counterparty, same class.
		{Name: "description", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r gold.SpendTransactionRow) string { return strOrEmpty(r.Description) }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.SpendTransactionRow) string { return r.Currency }},
		{Name: "net_amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendTransactionRow) string { return formatCents(r.NetAmount) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SpendTransactionRow) string { return formatCents(r.ValueOutCcy) }},
		{Name: "tx_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.SpendTransactionRow) string { return r.TransactionExternalID }},
	}
}

var defaultSpendTransactionColumns = []string{
	"silver_source", "date", "account", "merchant", "category",
	"currency", "net_amount", "value",
}

func spendingUsage() string {
	return `usage: wealthdb spending <view> [FROM [TO]] [--period P] [--level L]
                         [-f FORMAT] [-C COLS] [-x CCY] [-p]

What the tracked accounts spent. Every account counts unless the
config takes it out; what makes a row spending is its KIND, not the
kind of account it sits on. Amounts use historic FX
(nearest rate at-or-before the transaction) and are sign-split: spend
and refunds are both POSITIVE magnitudes, net_spend is their
difference; the transactions view keeps the ledger sign, so a purchase
is negative there. Own-account moves — card payments, funding wires,
mortgage payments — are not spending and appear in no view; mortgage
and loan payments are in 'wealthdb cashflow flows' (section financing).

Views (coarsest → finest):
  summary       one row per period bucket
  categories    one row per bucket and category, with its share of the bucket
  transactions  one row per spending line: merchant, category, and the tier
                that decided it

Window (positional, optional; default: the trailing twelve months):
  YYYY / YYYY-MM / YYYY-MM-DD   that calendar period
  FROM TO                       explicit range; '-' is open-ended

Flags:
  --period P        ` + strings.Join(reportPeriodNames, " | ") + `
                    (default monthly; total is one bucket for the whole window;
                    the transactions view has no buckets and ignores it)
  --level L         primary (default) | detailed — the category vocabulary
                    the categories view groups by; ignored elsewhere.
                    primary is about a dozen broad groups ("Food and
                    drink"); a category someone names — groceries,
                    restaurants, flights — is detailed
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact account IDs, amounts, and every name taken off
                    a statement line — merchant, signature, counterparty
                    (categories, provenance and shares stay visible)

There are no row-filter flags. To slice by merchant, category or
account, grep the table or take -f json and filter in jq.

Categories reconcile: for any period and level, the category rows of a
bucket sum to that bucket's summary row.

A category of '(uncategorized)' is the backlog — rows no rule, matcher,
provider or model could place. 'wealthdb categorize' works it down.

Available columns (per view):
  summary       ` + joinColumnNames(buildSpendSummaryColumnRegistry("CCY", "monthly")) + `
  categories    ` + joinColumnNames(buildSpendCategoryColumnRegistry("CCY", "monthly")) + `
  transactions  ` + joinColumnNames(buildSpendTransactionColumnRegistry("CCY")) + `

  (The money columns render as spend_<CCY> / net_spend_<CCY> /
   value_<CCY>, reflecting your -x/--currency choice.)

Default column sets:
  summary       ` + strings.Join(defaultSpendSummaryColumns, ", ") + `
  categories    ` + strings.Join(defaultSpendCategoryColumns, ", ") + `
  transactions  ` + strings.Join(defaultSpendTransactionColumns, ", ")
}
