// Package chase projects the chase collector's silver (a JPMorgan Chase
// retail relationship: deposit accounts and credit cards, told apart by
// `accounts.product`) into canonical change records: one account per roster
// account, its balances as cash with a card's debt negated, and the whole
// ledger of both products. It needs chase silver schema 3 or later. The
// projection is the shared bank adapter's (silver/bank); this file declares
// the source. See docs/adapters/chase.md for the mapping contract.
package chase

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

const kindName = "chase"

// productCard is the `accounts.product` discriminator for a credit card; the
// other value is 'dda' (checking / savings), which every non-card row carries.
const productCard = "card"

// spanExtrema is the MIN/MAX of every date the projection touches: account
// snapshots (accounts.snapshot_at), the transaction ledger (posted_at) — which
// carries both the per-day balance marks and the transaction events — and a
// card statement's period_end, where the statement-era closing balances land.
// It is the snapshot range Status reports.
const spanExtrema = `
SELECT MIN(t), MAX(t) FROM (
    SELECT snapshot_at AS t FROM accounts
    UNION ALL SELECT posted_at FROM transactions
    UNION ALL SELECT period_end FROM statement_balances)`

// adapter projects the source.
//
// THREE SOURCES FEED THE BALANCE SERIES, and they are mutually exclusive by
// construction so that no account ever gets two marks of one kind on one day:
//
//   - the transaction ledger's running balance, one mark per day the balance
//     moved. It is the only source for deposits, and for a card it covers the
//     export era, where the loader reconstructs the running balance neither
//     card export carries;
//   - a card statement's printed closing figure, one mark at each period_end
//     — but only for the periods the reconstruction does not reach, which is
//     what keeps the two apart;
//   - the roster, which is a CURRENT mark, not a closing one, and is stamped
//     at the source's latest roster snapshot rather than at a ledger day.
//
// SIGNS. A deposit row's silver `amount` is already signed the canonical way
// (positive = balance increase, debits negative), so the source sign is
// preserved rather than forced; summing NetAmount reproduces the account's net
// cash flow. A card row's amount is the provider's own convention — spend
// negative, anything reducing the balance owed positive — which agrees with
// the canonical convention for a liability carried as negative cash. It is
// still re-signed by its kind, because the card kinds have a fixed canonical
// direction: a purchase comes out negative and a refund / card payment
// positive whatever the row said.
//
// IDS are silver's own — content-derived and occurrence-indexed, not the
// provider's OFX FITID. Only the QFX export carries a FITID, each export
// format can fail on its own, and on a card the FITID is not even unique
// within one export (a credit reversing a charge repeats the charge's). The
// FITID rides along in `payload.fitid` for traceability.
var adapter = bank.New(bank.Source{
	Kind:            kindName,
	DefaultCurrency: "USD",
	// See returns.DepositBankPolicy for the flow-complete + hidden-plumbing
	// rationale.
	Returns: returns.DepositBankPolicy(),
	// The shared load clock (silver.LoadClock): this source has no change
	// feed of its own, so a loaded dump is the trigger and a full re-emit
	// is the response.
	Clock:          silver.LoadClock(kindName, spanExtrema, silver.Range("transactions", "posted_at")),
	NameCol:        "nickname",
	NameIsNickname: true,
	ProductCol:     "product",
	CardProduct:    productCard,
	Closings: []bank.Series{{
		// One CLOSING balance per day the balance moved, valued at that
		// day's end-of-day running balance from the transaction ledger —
		// the exact balance time series, back to the ledger's start. On a
		// deposit account this, not the statements, is the source of the
		// historic cash marks: the running balance carries the balance at
		// every date, whereas the collector records statement dates
		// without their parsed balances (and only for recent years). The
		// statement dates are a subset of these, so an as-of query at any
		// statement date returns that statement's closing balance.
		//
		// On a card the same column carries the loader's reconstruction,
		// which neither card export supplies: it rolls the posted ledger
		// between the statement closing anchors, and a span that fails to
		// land on its anchor keeps no balance at all. The statement era
		// carries none by design. Those gaps are what the statement
		// series fills, at period granularity.
		//
		// The fitid-highest row of a day stands for its end-of-day balance
		// (silver.RunningBalanceQuery). On a card that is exact — the
		// reconstruction rolls forward in (posted_at, fitid) order, so the
		// fitid-highest row of a day IS the one carrying its end-of-day
		// balance. On a deposit account the balance is the provider's own
		// column and the ledger has no intra-day order, so the pick is
		// arbitrary among that day's rows; the imprecision is bounded by
		// same-day activity and resolves by the next day.
		Label: "appendRunningBalances",
		Basis: "running_balance",
		Query: silver.RunningBalanceQuery,
	}, {
		// One CLOSING balance at a card statement's period_end, from the
		// figure the statement printed — the balance truth for the era
		// the reconstruction cannot reach.
		//
		// It is product-guarded to cards: the table is written by the
		// card statement pass only, and a deposit account's historic marks
		// come from its export's own running-balance column at day
		// density. Every row is a card's by that guard, so the printed
		// owed-positive figure is unconditionally negated.
		//
		// The NOT EXISTS clause is what keeps this series and the running
		// balances off the same day. A period contributes its anchor only
		// when NO reconstructed balance falls anywhere inside
		// [period_start, period_end] — and period_end is inside its own
		// period, so a collision is impossible. Suppressing the whole
		// period rather than just the day is also the right reading: the
		// reconstruction is anchored TO these closing figures, so where it
		// runs it already carries them, at balance-move density instead of
		// monthly.
		//
		// `transactions_covered` is deliberately not consulted. It records
		// whether the period's TRANSACTIONS reached silver; the printed
		// closing balance is asserted by the statement either way, and a
		// period whose rows were refused is precisely one that needs its
		// anchor.
		Label: "appendStatementBalances",
		Basis: "statement_closing",
		Query: `
SELECT sb.account_external_id, sb.period_end, sb.closing
  FROM statement_balances sb
 WHERE sb.closing IS NOT NULL
   AND sb.period_end BETWEEN ? AND ?
   AND sb.account_external_id IN (SELECT account_external_id FROM accounts
                                   WHERE product = ?)
   AND NOT EXISTS (SELECT 1 FROM transactions t
                    WHERE t.account_external_id = sb.account_external_id
                      AND t.balance IS NOT NULL
                      AND t.posted_at BETWEEN sb.period_start AND sb.period_end)
 ORDER BY sb.account_external_id, sb.period_end`,
		Args:  []any{productCard},
		Cards: true,
	}},
	IDCol: "fitid",
	Cols: bank.LedgerCols{
		Kind: "kind", Description: "description", Counterparty: "merchant",
		// Empty on payments, which the provider leaves uncategorised.
		Category: "category",
		TxnDate:  "txn_date",
		// Silver's per-row currency is the landing spot for a
		// foreign-currency card row; NULL means the account's own.
		Currency: "currency",
		// The cheque number, kept on an OUTFLOW only
		// (silver.CheckNumberOnOutflow). Silver's `check_number` is not
		// self-describing. The QFX path fills it from CHECKNUM, a cheque.
		// The deposit-export path fills it from a column headed "Check or
		// Slip #", which on a credit is a deposit slip.
		CheckNumber: "check_number",
	},
	TxKind: func(r bank.Row, card bool) (canonical.TxKind, bool, bool) {
		if card {
			k, known := cardTxKind(r.Kind, r.Amount)
			return k, known, true
		}
		return depositTxKind(r.Kind, r.Amount), true, false
	},
})

// init registers the kind (silver.Source). An account's display name is
// the roster's nickname where it has one.
func init() { silver.Register(adapter.Source()) }

// depositTxKind maps a Chase deposit transaction to a canonical TxKind.
// Interest and fees are recognised by their OFX TRNTYPE / CSV Type; everything
// else is a deposit or withdrawal by the (canonical) sign of the amount, which
// is the faithful classification for a cash account with no richer
// categorisation.
func depositTxKind(chaseKind string, amt canonical.Decimal) canonical.TxKind {
	k := strings.ToUpper(chaseKind)
	switch {
	case k == "INT" || strings.Contains(k, "INTEREST"):
		return canonical.TxKindInterest
	case k == "SRVCHG" || strings.Contains(k, "FEE"):
		return canonical.TxKindFee
	case amt.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

// cardTxKinds maps a card row's silver `kind` to the canonical taxonomy. Three
// vocabularies share that column: the CSV export's `Type`, the statement
// section a row was printed under, and — for a row that only ever landed in
// the QFX export, each format being fetched separately and able to fail on its
// own — the OFX TRNTYPE the loader falls back to.
var cardTxKinds = map[string]canonical.TxKind{
	"SALE":    canonical.TxKindPurchase,
	"RETURN":  canonical.TxKindRefund,
	"PAYMENT": canonical.TxKindCardPayment,
	// A QFX-only row carries the TRNTYPE, which says only which way the
	// money went. These entries make the type RECOGNISED; cardTxKind
	// then picks the direction off the sign, exactly as it does for an
	// Adjustment — a debit is spend, a credit reduces the balance owed,
	// which on a card is dominantly the monthly payment. Kinding a
	// credit `refund` instead would drop a large positive into the
	// spending base to cancel genuine purchases while leaving the
	// deposit-side bill unpaired, so the same money would count twice
	// with opposite signs; `other` would drop the row out of the
	// spending base altogether.
	//
	// THE COST IT ACCEPTS: a genuine refund that landed only in the QFX
	// is a credit, so it is kinded card_payment and leaves the spending
	// base — net spend for that period is overstated by it. And where
	// `other` was at least counted by `status -v`'s excluded-unmapped
	// line, card_payment is not, so a CSV-fetch outage shrinks net spend
	// with no drift signal. It is still the better of the two mappings:
	// the refund is a minority of card credits, and the converse
	// mapping mis-states every monthly bill instead.
	"DEBIT":  canonical.TxKindPurchase,
	"CREDIT": canonical.TxKindCardPayment,
	// An Adjustment carries the issuer's own sign and goes both ways: a
	// credit one offsets prior spend (a goodwill credit, a disputed charge
	// reversed, rewards cashed out against the balance), a debit one
	// re-bills it (a dispute decided the other way, a goodwill credit
	// withdrawn). cardTxKind keys the kind off the sign so `refund` nets
	// the credit against the spend it reverses and `purchase` keeps the
	// debit as spend; the entry here is the credit direction, which the
	// canonical-sign rule then pins. `other` in either direction would
	// drop the row out of the spending base and misstate what was spent.
	"ADJUSTMENT":    canonical.TxKindRefund,
	"FEE":           canonical.TxKindFee,
	"STMT_PURCHASE": canonical.TxKindPurchase,
	"STMT_PAYMENT":  canonical.TxKindCardPayment,
	"STMT_FEE":      canonical.TxKindFee,
	"STMT_INTEREST": canonical.TxKindInterest,
}

// cardTxKind maps a card row's kind and reports whether it was recognised. An
// unrecognised type lands as `other` with the raw string kept in the payload,
// per the global fallback rule (docs/DESIGN.md §6.8).
//
// Adjustment and the two TRNTYPE values are the types whose direction is not
// fixed by their name, so they are read off the amount the same way
// depositTxKind reads a deposit row: a credit adjustment nets as a `refund`, a
// debit one as a `purchase`, and a DEBIT / CREDIT is spend or a card payment
// by its sign. All stay inside the spending base carrying the issuer's own
// sign, where a kind pinned to one direction would flip the other and
// understate net spend by twice the amount.
//
// TxKindReward has no producer here: chase issues no reward transaction — a
// redemption against the balance surfaces as an Adjustment like any other
// credit — so the kind is left unproduced rather than guessed at from
// descriptors.
func cardTxKind(raw string, amt canonical.Decimal) (canonical.TxKind, bool) {
	norm := strings.ToUpper(strings.TrimSpace(raw))
	k, ok := cardTxKinds[norm]
	if !ok {
		return canonical.TxKindOther, false
	}
	switch norm {
	case "ADJUSTMENT":
		if amt.IsNegative() {
			return canonical.TxKindPurchase, true
		}
	case "DEBIT", "CREDIT":
		if amt.IsNegative() {
			return canonical.TxKindPurchase, true
		}
		return canonical.TxKindCardPayment, true
	}
	return k, true
}
