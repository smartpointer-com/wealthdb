package schwab

import "github.com/ptu/wealthdb/internal/canonical"

// kindFor maps a Schwab transaction `type` (the value silver stores
// in the `kind` column) to a canonical TxKind. Sign-driven types
// (TRADE, RECEIVE_AND_DELIVER, ELECTRONIC_FUND) get further split
// by the caller using the net-amount or position-effect field.
//
// Unrecognised types fall through to TxKindOther; the raw type is
// preserved in the transaction's payload.
//
// See docs/adapters/schwab.md §5.
func kindFor(rawType string, netAmount canonical.Decimal) canonical.TxKind {
	switch rawType {
	case "TRADE":
		// Schwab convention: buy → negative cash (money out), sell
		// → positive cash. We invert because gold's TxKind is
		// instrument-centric.
		if netAmount.IsNegative() {
			return canonical.TxKindBuy
		}
		return canonical.TxKindSell

	case "JOURNAL":
		return canonical.TxKindJournal

	case "DIVIDEND_OR_INTEREST":
		// Silver collapses dividend-vs-interest into one kind on
		// the promoted column. To discriminate we'd need to
		// inspect the payload's transferItems / activityDetails
		// for the subtype Schwab assigns. Future enhancement;
		// for now we route both to `dividend` which is the
		// dominant case in the observed real-data distribution.
		return canonical.TxKindDividend

	case "WIRE_IN", "CASH_RECEIPT":
		return canonical.TxKindDeposit

	case "WIRE_OUT", "CASH_DISBURSEMENT":
		return canonical.TxKindWithdrawal

	case "ELECTRONIC_FUND":
		if netAmount.IsNegative() {
			return canonical.TxKindWithdrawal
		}
		return canonical.TxKindDeposit

	case "RECEIVE_AND_DELIVER":
		if netAmount.IsNegative() {
			return canonical.TxKindTransferOut
		}
		return canonical.TxKindTransferIn

	default:
		// Includes SMA_ADJUSTMENT, MEMORANDUM, MONEY_MARKET, and
		// anything new Schwab adds without an adapter update.
		return canonical.TxKindOther
	}
}
