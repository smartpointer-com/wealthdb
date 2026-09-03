package fidelity

import (
	"encoding/json"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// kindFor maps fidelity-web's `transactions.kind` (the
// first word of Fidelity's "Action" column, e.g. "BUY",
// "DIVIDEND", "CASH_SWEEP_IN") to canonical TxKind values.
// DISTRIBUTION is the one kind that needs row context — the
// quantity and the payload's raw Action text — because Fidelity
// overloads it (see the case below).
//
// Fidelity's signed `amount` already follows the single-entry
// convention from the account's perspective (positive = cash in,
// negative = cash out), so for kinds where the canonical sign
// helper would normally flip values, we let the source sign
// pass through unchanged on the source-dependent kinds and rely
// on ApplyCanonicalSign to enforce direction on the fixed-sign
// ones.
//
// The two CASH_SWEEP_* kinds are core-position shuffles (cash
// ↔ money-market fund); we route them as TxKindOther so the
// source sign is preserved verbatim — gold's cash_balances roll-
// up already reflects the resulting balance via the snapshot
// path, so we don't want the sign helper to second-guess these.
//
// Unrecognised values land as TxKindOther with the raw kind
// preserved in payload.
func kindFor(raw string, quantity *canonical.Decimal, payload string) canonical.TxKind {
	switch raw {
	case "BUY", "REINVESTMENT":
		// REINVESTMENT is the share-purchase leg of a reinvested
		// dividend (the cash leg is the matching DIVIDEND row);
		// canonical sign for both is negative-cash.
		return canonical.TxKindBuy
	case "SELL", "REDEMPTION":
		return canonical.TxKindSell
	case "DIVIDEND":
		return canonical.TxKindDividend
	case "DISTRIBUTION":
		// Fidelity overloads DISTRIBUTION: pooled-fund capital-
		// gain payouts are pure-cash rows, but the share legs of
		// stock splits, ADR ratio changes and spinoffs also book
		// as DISTRIBUTION — with Amount carrying the market value
		// of the shares received even though no cash moved. Only
		// the cash rows are dividend-class income; a nonzero
		// quantity (shares received) or a SPINOFF action marks a
		// corporate action instead.
		if (quantity != nil && !quantity.IsZero()) ||
			strings.Contains(payloadAction(payload), "SPINOFF") {
			return canonical.TxKindCorporateAction
		}
		return canonical.TxKindDividend
	case "INTEREST":
		return canonical.TxKindInterest
	case "FEE":
		return canonical.TxKindFee
	case "TAX":
		return canonical.TxKindTax
	case "TRANSFER", "JOURNAL":
		// Both can flow either direction; keep source sign so
		// gold doesn't lose the in/out distinction.
		return canonical.TxKindJournal
	case "CASH_SWEEP_IN", "CASH_SWEEP_OUT":
		// Cash ↔ money-market fund movements. Source-signed.
		return canonical.TxKindOther
	case "MERGER", "NAME_CHANGE", "REVERSE_SPLIT", "TENDER",
		"EXPIRATION", "CASH_IN_LIEU", "RETURN_OF_CAPITAL":
		return canonical.TxKindCorporateAction
	case "ADJUSTMENT":
		return canonical.TxKindOther
	// Donor-Advised Fund event kinds (fidelity-web DESIGN.md §12).
	// From the giving account's perspective a GRANT / Gift4Giving
	// GIFT is cash irrevocably out (external flow), a CONTRIBUTION
	// external capital in, and an EXCHANGE an internal pool shuffle.
	// CONTRIBUTION also covers the retail 529 contribution rows —
	// external capital in there too.
	case "GRANT", "GIFT":
		return canonical.TxKindWithdrawal
	case "CONTRIBUTION":
		return canonical.TxKindDeposit
	case "EXCHANGE":
		return canonical.TxKindOther
	}
	return canonical.TxKindOther
}

// payloadAction extracts the raw "Action" text from a silver
// transaction payload. Empty on absent key or malformed JSON —
// callers treat that as "no signal", never an error.
func payloadAction(payload string) string {
	var p struct {
		Action string `json:"Action"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return strings.ToUpper(p.Action)
}
