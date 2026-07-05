package schwab

import (
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// kindFor maps a Schwab transaction `type` (the value silver stores
// in the `kind` column) to a canonical TxKind. Sign-driven types
// (TRADE, RECEIVE_AND_DELIVER, ELECTRONIC_FUND) get further split
// by the caller using the net-amount or position-effect field.
// DIVIDEND_OR_INTEREST is split via the payload's `description`
// because the Schwab API ships only the cash leg on those rows —
// the transferItems' assetType is always CURRENCY, so the security
// kind (ETF dividend vs treasury coupon vs cash sweep) is only
// recoverable from the free-text description.
//
// Unrecognised types fall through to TxKindOther; the raw type is
// preserved in the transaction's payload.
//
// See docs/adapters/schwab.md §5.
func kindFor(rawType string, netAmount canonical.Decimal, description string) canonical.TxKind {
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
		if isInterestDescription(description) {
			return canonical.TxKindInterest
		}
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

// isInterestDescription returns true when a DIVIDEND_OR_INTEREST
// payload's description identifies it as interest rather than a
// security dividend. Two families to catch:
//
//   - Cash-sweep interest: "BANK INT 011625-021525 SCHWAB BANK",
//     "SCHWAB1 INT 03/28-04/28", "INTEREST 12/30THRU 01/29",
//     "MARGIN INTEREST ...". These have stable, distinctive
//     prefixes — the leading token is what Schwab assigns and
//     never overlaps with a security name.
//
//   - Treasury coupon payments: descriptions like
//     "US TREASU NT 0.625%05/30UST NOTE DUE 05/15/30" or
//     "US TREASURY 1.25%05/50UST BOND DUE 05/15/50". The Schwab
//     API drops the instrument leg for DIVIDEND_OR_INTEREST so
//     assetType=TREASURY isn't available; we fall back to
//     description substrings ("UST NOTE", "UST BOND") that only
//     appear in Treasury holdings.
//
// Anything else (ETF / equity dividends) returns false.
func isInterestDescription(d string) bool {
	if d == "" {
		return false
	}
	u := strings.ToUpper(d)

	switch {
	case strings.HasPrefix(u, "BANK INT "):
		return true
	case strings.HasPrefix(u, "SCHWAB1 INT "):
		return true
	case strings.HasPrefix(u, "MARGIN INTEREST "):
		return true
	case strings.HasPrefix(u, "INTEREST "):
		// Plain "INTEREST <date-range>" — covers margin-interest
		// rows that Schwab labels without the "MARGIN" prefix.
		return true
	case strings.HasPrefix(u, "CREDIT INTEREST "):
		return true
	case strings.HasPrefix(u, "BOND INTEREST "):
		return true
	}

	// Treasury coupons: the description splices the security name
	// twice, with "UST NOTE DUE" or "UST BOND DUE" appearing as a
	// substring. Distinctive enough that an equity dividend won't
	// trip it.
	if strings.Contains(u, "UST NOTE") || strings.Contains(u, "UST BOND") {
		return true
	}
	return false
}
