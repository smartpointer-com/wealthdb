// Package amex projects the amex collector's silver — an American Express
// credit- and charge-card relationship — into canonical change records: one
// card account per card, its outstanding balance as negative cash, and the
// whole card ledger. Nothing reaches the returns engine; the source earns its
// keep in spending. The projection is the shared bank adapter's
// (silver/bank); this file declares the source. docs/adapters/amex.md
// documents the mapping, the one sign flip and the kind rules.
package amex

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

const kindName = "amex"

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the transaction ledger (posted_at), and
// the statement periods (period_end), which carry the historic balance
// marks. It is the snapshot range Status reports.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT period_end FROM statement_balances)`

// adapter projects the source.
//
// Every account is a card, so every balance is negated. The CURRENT balance
// is each card's roster figure, stamped at the source's latest roster
// snapshot. The CLOSING series is the statement periods'.
//
// SIGNS. Silver already holds the fleet's card convention — spend negative,
// anything reducing the balance owed positive — the collector having negated
// Amex's own spend-positive figures at load. That agrees with the canonical
// convention for a liability carried as negative cash. Each row is still
// re-signed by its kind, because the card kinds have a fixed canonical
// direction: a purchase comes out negative and a refund or bill payment
// positive whatever the row said.
//
// IDS on the modern era are the provider's own stable reference, which is also
// the QFX FITID and (unquoted) the CSV Reference. Unlike chase, no
// content-derived id is needed: every channel agrees on it, and the loader
// reads only one. Statement-sourced rows carry no such reference and the
// loader keys them positionally within their period instead (the collector's
// migration 0001 records the shape), which is stable as long as a period's
// parse is.
//
// PENDING rows are emitted like any other. Their ids are provisional — they
// change when the charge posts — but silver replaces the pending set wholesale
// on each load, and gold re-emits the full window on every load, so the
// provisional row is deleted by the same deleteWindow that re-applies the
// posted one. What a pending row buys is that today's spending report sees
// today's spending.
var adapter = bank.New(bank.Source{
	Kind:            kindName,
	DefaultCurrency: "USD",
	// See returns.CardIssuerPolicy for why a card contributes nothing to
	// returns at any grain, and gold.returnsInvisibleKind for where they
	// are dropped.
	Returns: returns.CardIssuerPolicy(),
	// The shared load clock (silver.LoadClock): this source has no change
	// feed of its own, so a loaded dump is the trigger and a full re-emit
	// is the response.
	Clock:       silver.LoadClock(kindName, spanExtrema, silver.Range("transactions", "posted_at")),
	NameCol:     "display_name",
	AccountKind: canonical.AccountKindCard,
	Closings: []bank.Series{{
		// One CLOSING balance at each statement period's end, from the
		// figure the period stated — the only historic balance truth a
		// card has, since no Amex channel carries a running balance per
		// row.
		//
		// Silver records a period from whichever channel stated it (the
		// activity JSON for the ~24 months it reaches, the statement PDFs
		// for the deeper archive), keyed on (account, period_end) so the
		// two converge on one row per period rather than competing.
		// `transactions_covered` is deliberately not consulted: it
		// records whether the period's ROWS reached silver, and the
		// closing balance is asserted either way — a period whose rows
		// were refused is precisely one that needs its anchor.
		//
		// A period's end can fall on the day of the CURRENT mark — the
		// activity channel's period ends at the window a run fetched.
		Label: "appendStatementBalances",
		Basis: "statement_closing",
		Query: `
SELECT account_external_id, period_end, closing
  FROM statement_balances
 WHERE closing IS NOT NULL
   AND period_end BETWEEN ? AND ?
 ORDER BY account_external_id, period_end`,
	}},
	IDCol: "txn_id",
	Cols: bank.LedgerCols{
		Kind: "kind", Description: "description", Counterparty: "merchant", Category: "category",
		TxnDate: "txn_date", Currency: "currency", Pending: "is_pending",
	},
	TxKind: func(r bank.Row, _ bool) (canonical.TxKind, bool, bool) {
		k, known := cardTxKind(r.Kind, r.Category, r.Amount)
		return k, known, true
	},
})

// init registers the kind (silver.Source).
func init() { silver.Register(adapter.Source()) }

// feesCategory is the provider's own category for the charges that are not
// purchases — an annual fee, a late fee, interest, and the adjustments that
// reverse them. Matched as a prefix so a spelling variant of the same bucket
// still lands.
const feesCategory = "fees"

// statementSectionKinds maps the DEEP era's kinds — the section a row was
// printed under on its statement — to the canonical taxonomy.
//
// The deep era carries no spend category, so the section is the only thing
// that separates a bill payment from a statement credit from a purchase; the
// collector stamps it for exactly that reason. The mapping is finer than the
// modern era's in one respect: a statement states interest as its own section,
// where the activity JSON bills it into the fees category and cannot be
// separated.
var statementSectionKinds = map[string]canonical.TxKind{
	"STMT_PAYMENT":  canonical.TxKindCardPayment,
	"STMT_CREDIT":   canonical.TxKindRefund,
	"STMT_PURCHASE": canonical.TxKindPurchase,
	"STMT_FEE":      canonical.TxKindFee,
	"STMT_INTEREST": canonical.TxKindInterest,
}

// cardTxKind maps a row's direction and spend category — or, before the
// structured horizon, its statement section (`STMT_*`) — to the canonical
// kind, and reports whether the direction was recognised. The category is
// what separates the two credits Amex issues: an uncategorised CREDIT is the
// monthly bill (`card_payment`), a categorised one a refund; a DEBIT under
// fees is a `fee`, any other a `purchase`. An unrecognised direction keeps
// the raw string in the payload and falls back to the sign silver already
// normalised, rather than to `other`, which reaches neither the spending base
// nor the matcher. No reward kind is produced: Amex marks the purchases that
// earn cash back, and a redemption is an ordinary categorised credit.
// docs/adapters/amex.md §5 argues each rule and the cost it accepts.
func cardTxKind(rawKind, category string, amt canonical.Decimal) (canonical.TxKind, bool) {
	norm := strings.ToUpper(strings.TrimSpace(rawKind))
	if k, ok := statementSectionKinds[norm]; ok {
		return k, true
	}
	switch norm {
	case "CREDIT":
		if strings.TrimSpace(category) == "" {
			return canonical.TxKindCardPayment, true
		}
		return canonical.TxKindRefund, true
	case "DEBIT":
		// The fees test lives inside the DEBIT case, not above the
		// switch: a fee REVERSAL carries the same category, and `fee`
		// forces a negative canonical sign, which would state the
		// charge twice.
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(category)),
			feesCategory) {
			return canonical.TxKindFee, true
		}
		return canonical.TxKindPurchase, true
	}
	// No usable direction: fall back to the sign silver already normalised,
	// so the row still lands on the right side of the ledger.
	if amt.IsNegative() {
		return canonical.TxKindPurchase, false
	}
	return canonical.TxKindCardPayment, false
}
