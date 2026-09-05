package ubs

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestCashMovementStampDuty guards the returns-flow fix: Swiss transfer stamp
// duty must map to a TAX (a cost inside the return), never to deposit/withdrawal
// (a capital-movement kind a returns calc would strip out as external capital).
// All narratives below are synthetic, standard-form stamp-duty wordings.
func TestCashMovementStampDuty(t *testing.T) {
	stampDuty := []string{
		"DROIT DE TIMBRE",
		"DROIT DE TIMBRE DE NEGOCIATION",
		"UMSATZABGABE",
		"STEMPELSTEUER",
		"STAMP DUTY",
		"timbre", // case-insensitive
	}
	for _, n := range stampDuty {
		// Both credit and debit sides must route to tax, not deposit/withdrawal.
		for _, cd := range []string{"C", "D"} {
			if got := cashMovementKind(n, cd); got != canonical.TxKindTax {
				t.Errorf("cashMovementKind(%q, %q) = %q, want tax", n, cd, got)
			}
		}
	}
}

// TestCashMovementUnchanged confirms the existing prefix mapping and the
// deposit/withdrawal fall-through for genuine cash movements are untouched.
func TestCashMovementUnchanged(t *testing.T) {
	cases := []struct {
		narrative, creditDebit string
		want                   canonical.TxKind
	}{
		{"INTERETS CREDITEURS", "C", canonical.TxKindInterest},
		{"FRAIS BANCAIRES", "D", canonical.TxKindFee},
		{"IMPOT ANTICIPE", "D", canonical.TxKindTax},
		{"DIVIDENDE", "C", canonical.TxKindDividend},
		// Unrecognised narrative still falls through to deposit/withdrawal by
		// sign — genuine wires must remain external-capital kinds.
		{"VIREMENT RECU", "C", canonical.TxKindDeposit},
		{"PAIEMENT", "D", canonical.TxKindWithdrawal},
		// A TWINT narrative in the MT940 era carries no prefix the map
		// knows, so it lands by the :61: credit/debit flag — money moving,
		// as in the web eras, whatever the case of the type.
		{"PAYMENT UBS TWINT\nEXAMPLE CHOCOLATIER AG", "D", canonical.TxKindWithdrawal},
		{"Credit UBS TWINT\nEXAMPLE, PERSON", "C", canonical.TxKindDeposit},
		{"REVERSAL UBS TWINT", "C", canonical.TxKindDeposit},
	}
	for _, c := range cases {
		if got := cashMovementKind(c.narrative, c.creditDebit); got != c.want {
			t.Errorf("cashMovementKind(%q,%q) = %q, want %q", c.narrative, c.creditDebit, got, c.want)
		}
	}
}
