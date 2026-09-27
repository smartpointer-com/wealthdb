package swissquote

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// kindFor maps Swissquote's `transactions.transaction_type` to
// canonical TxKind values. See docs/adapters/swissquote.md §6.
// Unrecognised values land as TxKindOther with the raw type
// preserved in payload.
func kindFor(txType string) canonical.TxKind {
	switch txType {
	case "Buy":
		return canonical.TxKindBuy
	case "Sell":
		return canonical.TxKindSell
	case "Dividend":
		return canonical.TxKindDividend
	case "Coupon":
		return canonical.TxKindCoupon
	case "Capital Gain":
		return canonical.TxKindCapitalGain
	case "Custody Fees", "Fees Tax Statement":
		return canonical.TxKindFee
	case "Interest on deposits":
		return canonical.TxKindInterest
	case "Payment":
		return canonical.TxKindDeposit
	case "Debit":
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindOther
	}
}
