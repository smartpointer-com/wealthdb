package schwab

import (
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// kindFor maps a Schwab transaction `type` (the value silver stores
// in the `kind` column) to a canonical TxKind. Sign-driven types
// (TRADE, ELECTRONIC_FUND) split on the net amount.
//
// RECEIVE_AND_DELIVER carries a zero net amount, so it splits on the
// description and on the security leg's quantity (nil when the row
// has no security leg); settleDeliveries then refines it against the
// row's siblings.
//
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
func kindFor(rawType string, netAmount canonical.Decimal, quantity *canonical.Decimal, description string) canonical.TxKind {
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
		if isCorporateActionDescription(description) {
			return canonical.TxKindCorporateAction
		}
		if quantity != nil && quantity.IsNegative() {
			return canonical.TxKindTransferOut
		}
		return canonical.TxKindTransferIn

	default:
		// Includes SMA_ADJUSTMENT, MEMORANDUM, MONEY_MARKET, and
		// anything new Schwab adds without an adapter update.
		return canonical.TxKindOther
	}
}

// corporateActionMarker matches the wording Schwab gives a
// RECEIVE_AND_DELIVER row that books a corporate action rather than a
// delivery: "REVERSE SPLIT EFF", "FORWARD SPLIT WITH STOCK SPLIT
// SHARES", "MANDATORY MERGER EFF", "Removed due to Expiration", and
// the like. Schwab splices the marker onto the security name, often
// with no space ("…INC XXXREVERSE SPLIT EFF"), so it is matched as a
// trailing word rather than a whole one.
var corporateActionMarker = regexp.MustCompile(
	`(?i)(SPLIT|MERGER|EXPIRATION|SPIN[ -]?OFF|NAME CHANGE)\b`)

func isCorporateActionDescription(d string) bool {
	return corporateActionMarker.MatchString(d)
}

// settleDeliveries refines the kinds of the RECEIVE_AND_DELIVER rows
// at idx against their siblings: the rows of the same account booked
// at the same instant, which is how Schwab books the legs of one
// event.
//
//   - A corporate action removes the old shares on a row carrying the
//     marker and books the new shares on a row carrying only the
//     security name. Every row of a group that holds a marked row is
//     therefore a corporate action.
//   - Otherwise, legs of one instrument whose quantities cancel move
//     shares between the account's cash and margin sub-accounts. They
//     are a journal, not a delivery.
//
// What remains keeps the per-row kind: a delivery in or out by the
// sign of its quantity.
func settleDeliveries(txs []canonical.TransactionChange, idx []int) {
	type groupKey struct {
		account string
		at      int64
	}
	groups := map[groupKey][]int{}
	for _, i := range idx {
		k := groupKey{txs[i].AccountExternalID, txs[i].OccurredAt}
		groups[k] = append(groups[k], i)
	}
	for _, g := range groups {
		corporate := false
		for _, i := range g {
			corporate = corporate || txs[i].Kind == canonical.TxKindCorporateAction
		}
		if corporate {
			for _, i := range g {
				setKind(&txs[i], canonical.TxKindCorporateAction)
			}
			continue
		}
		net := map[string]canonical.Decimal{}
		for _, i := range g {
			if t := txs[i]; t.InstrumentExternalID != nil && t.Quantity != nil {
				net[*t.InstrumentExternalID] = net[*t.InstrumentExternalID].Add(*t.Quantity)
			}
		}
		for _, i := range g {
			t := txs[i]
			if t.InstrumentExternalID == nil || t.Quantity == nil {
				continue
			}
			if n, ok := net[*t.InstrumentExternalID]; ok && n.IsZero() {
				setKind(&txs[i], canonical.TxKindJournal)
			}
		}
	}
}

// setKind re-types a transaction and re-signs its gross amount for the
// new kind, as buildTransaction signs it for the first.
func setKind(t *canonical.TransactionChange, k canonical.TxKind) {
	t.Kind = k
	t.GrossAmount = canonical.ApplyCanonicalSign(k, t.GrossAmount)
}

// isInterestDescription returns true when a DIVIDEND_OR_INTEREST
// payload's description identifies it as interest rather than a
// security dividend. Two families to catch:
//
//   - Cash-sweep interest: "BANK INT 010100-020100 SCHWAB BANK",
//     "SCHWAB1 INT 01/01-02/01", "INTEREST 01/01THRU 02/01",
//     "MARGIN INTEREST ...". These have stable, distinctive
//     prefixes — the leading token is what Schwab assigns and
//     never overlaps with a security name.
//
//   - Treasury coupon payments: descriptions like
//     "US TREASU NT 9.999%01/99UST NOTE DUE 01/15/99" or
//     "US TREASURY 9.999%01/99UST BOND DUE 01/15/99". The Schwab
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
