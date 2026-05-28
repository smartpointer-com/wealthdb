package fidelity

import "github.com/ptu/wealthdb/internal/canonical"

// kindFor maps fidelity-web-dump's `transactions.kind` (the
// first word of Fidelity's "Action" column, e.g. "BUY",
// "DIVIDEND", "CASH_SWEEP_IN") to canonical TxKind values.
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
func kindFor(raw string) canonical.TxKind {
	switch raw {
	case "BUY", "REINVESTMENT":
		// REINVESTMENT is the share-purchase leg of a reinvested
		// dividend (the cash leg is the matching DIVIDEND row);
		// canonical sign for both is negative-cash.
		return canonical.TxKindBuy
	case "SELL", "REDEMPTION":
		return canonical.TxKindSell
	case "DIVIDEND", "DISTRIBUTION":
		// DISTRIBUTION covers mutual-fund capital-gains
		// distributions, treated as dividend-class cash income
		// for single-entry purposes.
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
	}
	return canonical.TxKindOther
}
