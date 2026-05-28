package ubs

import (
	"strings"

	"github.com/ptu/wealthdb/internal/canonical"
)

// kindFor maps a UBS events.kind value (with optional narrative
// for cash_movement disambiguation) to a canonical TxKind.
//
// See docs/adapters/ubs.md §5 and §6.
func kindFor(silverKind, narrative string, creditDebit string) canonical.TxKind {
	switch silverKind {
	case "trade_confirmation":
		// `side` discrimination happens in the caller; default
		// here to TxKindBuy as a safe fallback.
		return canonical.TxKindBuy

	case "corporate_action_confirmation",
		"corporate_action_notification",
		"corporate_action_narrative":
		return canonical.TxKindCorporateAction

	case "fx_confirmation":
		return canonical.TxKindFxSpot

	case "fx_option_confirmation":
		// No dedicated fx_option TxKind in the canonical
		// taxonomy; the option settlement is effectively a spot
		// trade at expiry, so we route here. Revisit if a
		// distinct kind becomes useful for analytics.
		return canonical.TxKindFxSpot

	case "loan_deposit_confirmation":
		return canonical.TxKindOther // future: money_market-related

	case "securities_movement",
		"securities_settlement_advice":
		return signedTransfer(creditDebit)

	case "precious_metal_trade":
		// Treat as buy or sell depending on credit/debit; default
		// buy when sign is unclear.
		if creditDebit == "C" || creditDebit == "CRDT" {
			return canonical.TxKindSell
		}
		return canonical.TxKindBuy

	case "charges_advice":
		return canonical.TxKindFee

	case "debit_credit_confirmation":
		return signedDepositWithdrawal(creditDebit)

	case "cash_movement":
		return cashMovementKind(narrative, creditDebit)

	default:
		return canonical.TxKindOther
	}
}

// cashMovementKind splits MT940 :86: narrative-tagged cash
// movements into the right canonical kind. Conservative parsing:
// only the prefixes we've observed are recognised; everything
// else falls through to deposit/withdrawal by sign so we don't
// invent semantics from ambiguous text.
//
// Prefix matching is case-insensitive and looks at the first
// "word" (run of letters) only.
func cashMovementKind(narrative, creditDebit string) canonical.TxKind {
	prefix := firstWord(narrative)
	switch strings.ToUpper(prefix) {
	case "INT", "INTERESTS", "INTERETS", "ZINSEN":
		return canonical.TxKindInterest
	case "COMM", "COMMISSION", "FRAIS", "GEBUEHREN", "FEE", "FEES":
		return canonical.TxKindFee
	case "IMP", "IMPOT", "STEUER", "TAX":
		return canonical.TxKindTax
	case "DIV", "DIVIDEND", "DIVIDENDE":
		return canonical.TxKindDividend
	}
	return signedDepositWithdrawal(creditDebit)
}

// signedDepositWithdrawal returns deposit for credit-side
// movements, withdrawal otherwise.
func signedDepositWithdrawal(creditDebit string) canonical.TxKind {
	switch strings.ToUpper(creditDebit) {
	case "C", "CR", "CRDT", "CREDIT":
		return canonical.TxKindDeposit
	}
	return canonical.TxKindWithdrawal
}

// signedTransfer is the equivalent for securities movements.
func signedTransfer(creditDebit string) canonical.TxKind {
	switch strings.ToUpper(creditDebit) {
	case "C", "CR", "CRDT", "CREDIT":
		return canonical.TxKindTransferIn
	}
	return canonical.TxKindTransferOut
}

// firstWord returns the leading run of letters from s, or the
// empty string if s starts with a non-letter.
func firstWord(s string) string {
	for i, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		return s[:i]
	}
	return s
}
