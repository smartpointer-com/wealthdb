package erstebankat

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/bank"
)

// checkingTypes are the George transaction types (`txType`) of a checking
// row whose direction is its sign: a card payment, an ATM withdrawal, a
// direct debit (CASHCOLLECTION), a standing order, a transfer entered by
// hand or by the bank (MANUAL, OTHER).
var checkingTypes = map[string]bool{
	"ATM": true, "CASHCOLLECTION": true, "MANUAL": true, "OTHER": true,
	"POSPAYMENT": true, "STANDINGORDER": true,
}

// txKind kinds a ledger row. Money out is negative on both ledgers, and a
// card row takes its kind's canonical sign. A type the rules do not know
// falls back by the sign of the amount, so the row still counts in cash
// flow and spending, and the raw type stays in the payload.
func txKind(r bank.Row, card bool) (kind canonical.TxKind, known, resign bool) {
	if card {
		kind, known = cardTxKind(r.Kind, merchant(r.Payload), r.Amount)
		return kind, known, true
	}
	kind, known = checkingTxKind(r.Kind, r.Description+" "+memo(r.Payload), r.Amount)
	return kind, known, false
}

// checkingTxKind maps a checking row and reports whether its type was
// recognised. An unrecognised type is a withdrawal or a deposit by its
// sign.
//
// BANKFEE is the account-closing entry (Abschluss), which carries the
// account fee, the interest either way, and the capital-gains tax withheld
// on credit interest. Its text tells them apart: KESt (Kapitalertragsteuer)
// marks the tax, Zins the interest, and anything else is a fee.
func checkingTxKind(txType, text string, amt canonical.Decimal) (canonical.TxKind, bool) {
	if txType == "BANKFEE" {
		t := strings.ToUpper(text)
		switch {
		case strings.Contains(t, "KEST"):
			return canonical.TxKindTax, true
		case strings.Contains(t, "ZINS"):
			return canonical.TxKindInterest, true
		default:
			return canonical.TxKindFee, true
		}
	}
	known := checkingTypes[txType]
	if amt.IsNegative() {
		return canonical.TxKindWithdrawal, known
	}
	return canonical.TxKindDeposit, known
}

// cardTxKind maps a card row and reports whether its type was recognised.
// George books every card row as CREDITCARD, so the kind comes from the
// row's shape: a charge is a purchase, a credit from a merchant (it carries
// the merchant category code) a refund, and a credit with no merchant the
// repayment of the card. A row of another type is a purchase or a card
// payment by its sign.
func cardTxKind(txType string, merchant bool, amt canonical.Decimal) (canonical.TxKind, bool) {
	known := txType == "CREDITCARD"
	switch {
	case amt.IsNegative():
		return canonical.TxKindPurchase, known
	case merchant && known:
		return canonical.TxKindRefund, true
	default:
		return canonical.TxKindCardPayment, known
	}
}
