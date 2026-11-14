package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("transactions", routeReport(transactionsFamily))
}

// transactionsFamily is the ledger, a tool of one shape.
var transactionsFamily = &family{
	name: "transactions",
	views: []view{{total: flowTotal, principal: fixedColumn("value"),
		build: func(_ *config.Config, req request, _ *toolArgs) *report { return transactionsReport(req) }}},
	options:  []option{{name: "newest_first", cli: []string{"r", "reverse"}, kind: optFlag}},
	window:   pastMonth,
	currency: true,
	narrow:   "from/to or a filter",
	usage:    transactionsUsage,
	settle: func(_ *family, in optionSource, req *request, _ *[]string) error {
		req.newestFirst = in.on("newest_first")
		return nil
	},
	header: func(req request) []string {
		if req.newestFirst {
			return []string{"newest first"}
		}
		return nil
	},
}

// transactionsReport is the ledger over a window, the runner the CLI
// and the MCP server share.
func transactionsReport(req request) *report {
	order := gold.SortAscending
	if req.newestFirst {
		order = gold.SortDescending
	}
	kindOf, loadKinds := sourceKinds()
	return withLoads(newReport(buildTransactionColumnRegistry(req.currency, kindOf, req.categories),
		defaultTransactionColumns,
		func(ctx context.Context, db *sql.DB) ([]gold.TransactionRow, error) {
			return gold.TransactionsBetween(ctx, db, req.from, req.to, req.currency, order)
		}), loadKinds)
}

// ---- column registry -----------------------------------------------------

// txnLineCols are the columns of a gold.TxnLine and a gold.TxnValue,
// which every transaction-grain view shares, by name: each registry
// lists them in its own order. line and value read the two off a row.
// A name the two do not hold panics: the registry misspells it.
func txnLineCols[T any](outCcy string, kindOf func(string) string, categories typedCategories,
	line func(T) gold.TxnLine, value func(T) gold.TxnValue) func(name string) columnSpec[T] {
	ref := func(r T) accountRef {
		l := line(r)
		return accountRef{source: l.SilverSourceID, name: l.DisplayName, nickname: l.Nickname, id: l.AccountExternalID}
	}
	specs := []columnSpec[T]{
		strCol("silver_source", func(r T) string { return line(r).SilverSourceID }),
		asDate(strCol("date", func(r T) string { return formatDate(line(r).OccurredAt) })),
		asDate(strCol("datetime", func(r T) string { return formatDateTime(line(r).OccurredAt) })),
		// A card's display name is "<product> ****1234" on most
		// issuers, so the account columns carry an account number and
		// redact as one — the same class the account label takes
		// everywhere else in the CLI.
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		textCol("account_kind", func(r T) *string { return line(r).AccountKind }),
		nicknameCol("account_nickname", func(r T) *string { return line(r).Nickname }),
		accountCategoryCol(categories, func(r T) *string { return line(r).AccountCategory }),
		strCol("kind", func(r T) string { return line(r).Kind }),
		strCol("currency", func(r T) string { return value(r).Currency }),
		moneyCol("net_amount", func(r T) *string { return value(r).NetAmount }),
		outCcyCol("value", outCcy, func(r T) *string { return value(r).ValueOutCcy }),
		// The counterparty is the adapter's merchant field, published
		// unfiltered: on a card row it is a shop, on a transfer it is
		// a person. Nothing upstream separates the two, so it takes
		// the free-text class, which masks the cell whole — a wire or
		// P2P narrative is multi-word, and any shape-based rule would
		// wave exactly those through.
		privateCol("counterparty", PrivacyFreeText, func(r T) string { return strOrEmpty(value(r).Counterparty) }),
		// The statement narrative, verbatim: a bank line carries a
		// counterparty's name, address and reference text, and gold
		// appends the payer's memo behind the separator. Same exposure
		// as the counterparty, same class: the cell masks whole,
		// because there is no safe slice of a narrative to expose.
		privateCol("description", PrivacyFreeText, func(r T) string { return strOrEmpty(value(r).Description) }),
		typedIDCol("tx_id", kindOf, func(r T) (string, string) {
			l := line(r)
			return l.SilverSourceID, l.TransactionExternalID
		}),
	}
	return func(name string) columnSpec[T] {
		for _, c := range specs {
			if c.Name == name {
				return c
			}
		}
		panic("txnLineCols: no column " + name)
	}
}

func buildTransactionColumnRegistry(outCcy string, kindOf func(string) string,
	categories typedCategories) []columnSpec[gold.TransactionRow] {
	line := txnLineCols(outCcy, kindOf, categories,
		func(r gold.TransactionRow) gold.TxnLine { return r.TxnLine },
		func(r gold.TransactionRow) gold.TxnValue { return r.TxnValue })
	holding := func(r gold.TransactionRow) (string, string) {
		return r.SilverSourceID, strOrEmpty(r.InstrumentExternalID)
	}
	return []columnSpec[gold.TransactionRow]{
		line("silver_source"), line("date"), line("datetime"), line("account"), line("account_id"), line("kind"),
		textCol("symbol", func(r gold.TransactionRow) *string { return r.Symbol }),
		holdingNameCol(columnSpec[gold.TransactionRow]{Name: "name",
			Extract: func(r gold.TransactionRow) string {
				// Prefer the joined instruments.name; fall back to the
				// adapter-supplied transactions.description (Schwab
				// dividends, UBS web cash_movement captions) so
				// instrument-related rows still surface a label.
				if r.Name != nil && *r.Name != "" {
					return *r.Name
				}
				return strOrEmpty(r.Description)
			},
			// The class follows which of the two the cell holds. A
			// joined instrument name is a holding's name and stays
			// legible, a typed holding's with its ids masked
			// (holdingNameCol); the description fallback is a statement
			// narrative and takes the free-text class, exactly as the
			// `description` column below does, whoever holds the
			// instrument.
			PrivacyFunc: func(r gold.TransactionRow) PrivacyClass {
				if r.Name == nil || *r.Name == "" {
					return PrivacyFreeText
				}
				return PrivacyNone
			}}, kindOf, func(r gold.TransactionRow) (string, string) { return r.SilverSourceID, strOrEmpty(r.Name) }),
		holdingIDCol(textCol("instrument_id", func(r gold.TransactionRow) *string { return r.InstrumentExternalID }),
			kindOf, holding),
		// What the row's instrument was looked up BY, where that lookup
		// found nothing. `instrument_id` empty beside a value here is
		// exactly a row a `transaction_instruments` entry can close,
		// and this is the value such an entry is keyed by.
		holdingIDCol(textCol("instrument_hint", func(r gold.TransactionRow) *string { return r.InstrumentHint }),
			kindOf, func(r gold.TransactionRow) (string, string) { return r.SilverSourceID, strOrEmpty(r.InstrumentHint) }),
		line("description"),
		// The spending overlay's verdict on the row (spend_txn_categories).
		// Empty for everything the enrichment pass does not reach:
		// investment rows, and anything outside the spending account
		// scope. The merchant is the store's name for the row's
		// signature, or that signature itself where the store holds
		// none, and is empty on a delta row (an own-account move,
		// capital deployed, a gift), which carries its issuer label or
		// nothing.
		// Free-text class, as on the spending view: the transfer fence
		// gates what may acquire a STORE name, not what this column
		// prints, so a narrative the fence refused surfaces here as
		// its own fold. The spend_* categories and asset_class stay
		// legible as taxonomy.
		privateCol("merchant", PrivacyFreeText, func(r gold.TransactionRow) string { return strOrEmpty(r.MerchantName) }),
		textCol("spend_primary", func(r gold.TransactionRow) *string { return r.SpendPrimary }),
		textCol("spend_detailed", func(r gold.TransactionRow) *string { return r.SpendDetailed }),
		// The INCOME overlay's verdict on the same row
		// (income_txn_categories), and a row can carry both: a deposit the
		// matcher paired is `internal_transfer` on each side, and this is
		// the one surface that shows a transaction from both at once.
		// `payer` takes the free-text class for the reason `merchant`
		// does — on a dividend it is the instrument, but on a deposit
		// it is the store's name for the signature or the fold of the
		// narrative itself, and a column takes the class of the worst
		// thing it can hold.
		privateCol("payer", PrivacyFreeText, func(r gold.TransactionRow) string { return strOrEmpty(r.PayerName) }),
		textCol("income_primary", func(r gold.TransactionRow) *string { return r.IncomePrimary }),
		textCol("income_detailed", func(r gold.TransactionRow) *string { return r.IncomeDetailed }),
		// The cheque number of an outgoing paper cheque, for matching a
		// row against the holder's own paper records. It names no third
		// party, so it is not free text — but it is an identifier tied
		// to the holder's own account, which is what PrivacyAccountID
		// covers.
		privateCol("check_no", PrivacyAccountID, func(r gold.TransactionRow) string { return strOrEmpty(r.CheckNumber) }),
		// The node the cash flow statement resolved the row to
		// (cashflow_txn_nodes), beside the two families' trios: which
		// section of the statement, which inner node, which leaf. The
		// three are vocabulary and stay legible under -p, as the
		// families' categories do. Blank where the resolution reached
		// no node — a kind with no canonical sign, an unpaired card
		// bill — and on a pool-internal move, which is a movement the
		// statement deliberately does not draw.
		textCol("cashflow_section", func(r gold.TransactionRow) *string { return r.CashflowSection }),
		textCol("cashflow_class", func(r gold.TransactionRow) *string { return r.CashflowClass }),
		textCol("cashflow_group", func(r gold.TransactionRow) *string { return r.CashflowGroup }),
		textCol("asset_class", func(r gold.TransactionRow) *string { return r.AssetClass }),
		line("currency"),
		moneyCol("gross_amount", func(r gold.TransactionRow) *string { return r.GrossAmount }),
		line("net_amount"),
		quantityCol("quantity", func(r gold.TransactionRow) *string { return r.Quantity }),
		kindCol("price", MoneyColumn, func(r gold.TransactionRow) string { return strOrEmpty(r.Price) }),
		line("value"), line("tx_id"),
		privateCol("relationship_id", PrivacyAccountID, func(r gold.TransactionRow) string { return strOrEmpty(r.RelationshipID) }),
		line("account_nickname"), line("account_category"), line("account_kind"),
	}
}

var defaultTransactionColumns = []string{
	"silver_source", "date", "account", "kind", "symbol",
	"instrument_id", "currency", "net_amount", "value",
}

func transactionsUsage() string {
	registry := buildTransactionColumnRegistry("CCY", nil, nil)
	return `usage: wealthdb transactions [FROM [TO]] [-r] [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

Print transactions over a date range: trades, dividends, interest,
fees, deposits, withdrawals and transfers, one line each. Money
leaving an account is negative, money arriving positive. Default:
past 30 days, table format, oldest first, default column set,
output currency from config.default_currency, historic FX (nearest
rate at-or-before occurred_at).

Date arguments (positional, optional; may appear before or after
flags):
  (no args)              past 30 days
  YYYY                   full calendar year
  YYYY-MM                full calendar month
  YYYY-MM-DD             single day
  YYYY-MM-DD YYYY-MM-DD  explicit range, inclusive
  YYYY-MM-DD -           open end (FROM date to today)
  - YYYY-MM-DD           open start (epoch to TO date)
  - today                all time (synonym of "- -")
  today                  accepted in any position as a synonym

Flags:
  -r, --reverse            reverse-time order (newest first); default is oldest first
  -f, --format FORMAT      output format: table | csv | csv_plain | json
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+description-account)
  -F, --filter COND        keep the rows where COND holds; repeatable;
                           wealthdb help filter lists the operators
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
  -p, --privacy            redact account / tx IDs, quantities, prices, and monetary amounts;
                           every name a person typed about an account prints as a pseudonym,
                           such as manual-1, or as a redacted ID; holding names stay, and an
                           ID typed into a manual holding's name masks; statement narratives
                           (description, and merchant / payer — the store's name or the
                           line's own signature — and the name column where it falls back to
                           one) redact as free text — the cell masks whole
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value' column renders as 'value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

Default column set:
  ` + strings.Join(defaultTransactionColumns, ", ")
}
