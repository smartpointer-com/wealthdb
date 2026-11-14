package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	// The usage lists the family's columns, so it joins the family here.
	spendingFamily.usage = spendingUsage
	register("spending", routeReport(spendingFamily))
}

// spendingFamily are the views of `wealthdb spending <view>`: the three
// grains the report_spending_* macros define over the spending
// population, coarsest first, then `fees`, what each portfolio paid
// against what it held, a side view.
var spendingFamily = &family{
	name: "spending",
	views: []view{
		{name: "summary", total: flowTotal, principal: fixedColumn("net_spend"), header: periodHeader,
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				part := reportPeriods[req.period]
				return newReport(buildSpendSummaryColumnRegistry(req.currency, req.period), defaultSpendSummaryColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.SpendSummaryRow, error) {
						return gold.SpendingSummary(ctx, db, req.from, req.to, req.currency, part)
					})
			}},
		{name: "categories", total: flowTotal, principal: fixedColumn("net_spend"), header: periodLevelHeader,
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				part := reportPeriods[req.period]
				return newReport(buildSpendCategoryColumnRegistry(req.currency, req.period), defaultSpendCategoryColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.SpendCategoryRow, error) {
						return gold.SpendingCategories(ctx, db, req.from, req.to, req.currency, part, req.level)
					})
			}},
		{name: "transactions", total: flowTotal, principal: fixedColumn("value"),
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				kindOf, loadKinds := sourceKinds()
				return withLoads(newReport(buildSpendTransactionColumnRegistry(req.currency, kindOf, req.categories),
					defaultSpendTransactionColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.SpendTransactionRow, error) {
						return gold.SpendingTransactions(ctx, db, req.from, req.to, req.currency)
					}), loadKinds)
			}},
		// On fees the entity is the portfolio or the account. A mean
		// value and a rate do not add up over entities.
		{name: "fees", side: true, principal: fixedColumn("fees"), header: periodHeader, usage: spendingFeesUsage,
			options: []option{{name: "period", cli: []string{"period"}, choices: feePeriods,
				def: [2]string{"total", "total"}}},
			refuses:  feesRefusedFlags,
			keyAlias: map[string]string{"account": "entity"},
			build: func(cfg *config.Config, req request, _ *toolArgs) *report {
				return spendingFeesReport(req, cfg)
			}},
	},
	options: []option{
		{name: "period", cli: []string{"period"}, choices: reportPeriodNames, def: [2]string{"monthly", "total"}},
		{name: "level", cli: []string{"level"}, choices: categoryLevels, def: [2]string{"primary", "primary"}},
	},
	window:   trailingYear,
	currency: true,
	narrow:   "from/to or a filter",
	settle: func(_ *family, in optionSource, req *request, notes *[]string) error {
		if req.view == "fees" {
			// The CLI refuses the level as it parses it.
			if !in.cli() {
				if err := refuseOptions(in, "spending", "fees", feesRefusedFlags, ""); err != nil {
					return err
				}
			}
			var err error
			req.period, err = choose(in, "spending", "period")
			return err
		}
		var err error
		if req.period, err = choose(in, "spending", "period"); err != nil {
			return err
		}
		if req.level, err = choose(in, "spending", "level"); err != nil {
			return err
		}
		// A category filter is a name search, and names live in the
		// detailed vocabulary: "groceries" is a detailed category under
		// "Food and drink".
		if req.view == "categories" && !in.has("level") && in.has("category") {
			req.level = "detailed"
			*notes = append(*notes, "level detailed for the category filter")
		}
		return nil
	},
}

// categoryLevels are the `--level` vocabularies of spending's and
// income's categories: the broad groups or the detailed values.
var categoryLevels = []string{"primary", "detailed"}

// ---- column registries ---------------------------------------------------

// buildSpendSummaryColumnRegistry needs `period` as well as the output
// currency: the bucket label's shape follows the bucket's size, the
// same way the money headers follow -x/--currency.
func buildSpendSummaryColumnRegistry(outCcy, period string) []columnSpec[gold.SpendSummaryRow] {
	return []columnSpec[gold.SpendSummaryRow]{
		asDate(strCol("period", func(r gold.SpendSummaryRow) string { return periodLabel(r.PeriodStart, period) })),
		asDate(strCol("period_start", func(r gold.SpendSummaryRow) string { return periodStart(r.PeriodStart) })),
		countCol("txn_count", func(r gold.SpendSummaryRow) int64 { return r.TxnCount }),
		outCcyCol("spend", outCcy, func(r gold.SpendSummaryRow) *string { return r.Spend }),
		outCcyCol("refunds", outCcy, func(r gold.SpendSummaryRow) *string { return r.Refunds }),
		outCcyCol("net_spend", outCcy, func(r gold.SpendSummaryRow) *string { return r.NetSpend }),
	}
}

var defaultSpendSummaryColumns = []string{
	"period", "txn_count", "spend", "refunds", "net_spend",
}

func buildSpendCategoryColumnRegistry(outCcy, period string) []columnSpec[gold.SpendCategoryRow] {
	return []columnSpec[gold.SpendCategoryRow]{
		asDate(strCol("period", func(r gold.SpendCategoryRow) string { return periodLabel(r.PeriodStart, period) })),
		asDate(strCol("period_start", func(r gold.SpendCategoryRow) string { return periodStart(r.PeriodStart) })),
		// The category is taxonomy — a vocabulary value, not an
		// identifier — and stays legible under -p, like asset_class
		// and the other taxonomy labels. Two spellings of the same
		// thing: `category` is what a report reads as, `category_id`
		// the value it groups on. The label is the default because it
		// is what a person reads; the id is there for a caller
		// scripting against a stable key.
		strCol("category", func(r gold.SpendCategoryRow) string { return r.CategoryLabel }),
		strCol("category_id", func(r gold.SpendCategoryRow) string { return r.Category }),
		countCol("txn_count", func(r gold.SpendCategoryRow) int64 { return r.TxnCount }),
		outCcyCol("spend", outCcy, func(r gold.SpendCategoryRow) *string { return r.Spend }),
		outCcyCol("refunds", outCcy, func(r gold.SpendCategoryRow) *string { return r.Refunds }),
		outCcyCol("net_spend", outCcy, func(r gold.SpendCategoryRow) *string { return r.NetSpend }),
		ratioCol("share", func(r gold.SpendCategoryRow) *float64 { return r.Share }),
	}
}

var defaultSpendCategoryColumns = []string{
	"period", "category", "txn_count", "spend", "refunds", "net_spend", "share",
}

func buildSpendTransactionColumnRegistry(outCcy string, kindOf func(string) string,
	categories typedCategories) []columnSpec[gold.SpendTransactionRow] {
	line := txnLineCols(outCcy, kindOf, categories,
		func(r gold.SpendTransactionRow) gold.TxnLine { return r.TxnLine },
		func(r gold.SpendTransactionRow) gold.TxnValue { return r.TxnValue })
	return []columnSpec[gold.SpendTransactionRow]{
		line("silver_source"), line("date"), line("datetime"), line("account"), line("account_id"),
		line("account_kind"), line("account_nickname"), line("account_category"), line("kind"),
		// merchant_name redacts as free text, like the narrative it
		// was named from. The fence (spending.RowTransferShaped) gates
		// candidacy for the merchant STORE, not this column: a wire,
		// an ACH, a P2P narrative is refused a verdict, has no store
		// name, and falls back to the signature folded from that very
		// narrative (spend_txn_categories) — so a payment to a person prints
		// its payee here. A store name carries the same exposure by a
		// slower route: the store is append-only across signature
		// revisions and across widenings of the fence itself, and the
		// enrichment lookup applies a stored verdict by signature
		// forever, so a name bought while the fence was narrower
		// outlives the fence that would now refuse it, and the model
		// wrote it from the narrative it was shown. Either way the
		// column carries no guarantee about what is in it, and -p
		// treats it as what it is: a name taken off a statement line.
		privateCol("merchant", PrivacyFreeText, func(r gold.SpendTransactionRow) string { return strOrEmpty(r.MerchantName) }),
		// The signature is the opposite case: it is computed for
		// EVERY enriched row, fenced or not, as a fold of the raw
		// narrative — so it carries whatever the narrative carried.
		// Same class as counterparty.
		privateCol("merchant_signature", PrivacyFreeText, func(r gold.SpendTransactionRow) string { return strOrEmpty(r.MerchantSignature) }),
		textCol("spend_primary", func(r gold.SpendTransactionRow) *string { return r.SpendPrimary }),
		textCol("spend_detailed", func(r gold.SpendTransactionRow) *string { return r.SpendDetailed }),
		// The same two values as they read (migration 0058).
		textCol("category", func(r gold.SpendTransactionRow) *string { return r.SpendLabel }),
		textCol("category_primary", func(r gold.SpendTransactionRow) *string { return r.SpendPrimaryLabel }),
		// The ISSUER's own classification of the line, mapped into the
		// wealthdb vocabulary and kept beside the wealthdb category
		// (migration 0057). Off by default: it is a second opinion, it
		// disagrees with the category by design, and nothing may sum
		// the two. Empty where the issuer
		// published nothing this build translates — which is not the
		// same as the issuer filing the row under a catch-all.
		//
		// Named `issuer_*` rather than `provider_category`, which
		// everywhere else in the product — `transactions.provider_category`
		// (migration 0038), the field a config rule matches — means the
		// issuer's RAW string. These two are its translation.
		textCol("issuer_category", func(r gold.SpendTransactionRow) *string { return r.ProviderSpendLabel }),
		textCol("issuer_category_id", func(r gold.SpendTransactionRow) *string { return r.ProviderSpendDetailed }),
		// Which tier decided the category (matcher / rule / provider /
		// signature-only / model / manual) — vocabulary, not data.
		// Five of the six are stamped on the overlay row by the
		// enrichment pass; `model` is not stored anywhere, because the
		// model tier writes to the merchant store rather than the
		// overlay. spend_txn_categories() resolves it at the point the
		// two scopes meet, which is the only place the store's verdict
		// is distinguishable from the backlog.
		textCol("provenance", func(r gold.SpendTransactionRow) *string { return r.Provenance }),
		// The description is the statement narrative the signature was
		// folded from.
		line("counterparty"), line("description"), line("currency"), line("net_amount"), line("value"), line("tx_id"),
	}
}

var defaultSpendTransactionColumns = []string{
	"silver_source", "date", "account", "merchant", "category",
	"currency", "net_amount", "value",
}

func spendingUsage() string {
	// fees, the side view, has a usage text of its own.
	available, defaults := columnSets(spendingFamily, 13, spendingFamily.ladder(), request{currency: "CCY", period: "monthly"}, nil)
	return `usage: wealthdb spending <view> [FROM [TO]] [--period P] [--level L]
                         [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

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

Also:
  fees          what each portfolio cost, against what it held and returned
                ('wealthdb spending fees -h')

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
  -F, --filter COND keep the rows where COND holds; repeatable;
                    wealthdb help filter lists the operators
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact account IDs, amounts, and every name taken off
                    a statement line — merchant, signature, counterparty;
                    every name a person typed about an account prints as
                    a pseudonym, such as manual-1, or as a redacted ID
                    (categories, provenance and shares stay visible)

There are no row-filter flags. To slice by merchant, category or
account, grep the table or take -f json and filter in jq.

Categories reconcile: for any period and level, the category rows of a
bucket sum to that bucket's summary row.

A category of '(uncategorized)' is the backlog — rows no rule, matcher,
provider or model could place. 'wealthdb categorize' works it down.

Available columns (per view):` + available + `

  (The money columns render as spend_<CCY> / net_spend_<CCY> /
   value_<CCY>, reflecting your -x/--currency choice.)

Default column sets:` + defaults
}
