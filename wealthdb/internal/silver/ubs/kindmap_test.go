package ubs

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
			if got := cashMovementKind(n, cd, ""); got != canonical.TxKindTax {
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
		if got := cashMovementKind(c.narrative, c.creditDebit, ""); got != c.want {
			t.Errorf("cashMovementKind(%q,%q) = %q, want %q", c.narrative, c.creditDebit, got, c.want)
		}
	}
}

// TestTheTypeCodeClassifiesWhatTheNarrativeCannot pins the :61: floor.
//
// An MT940 entry the bank wrote no narrative for used to fall straight
// through to the credit/debit direction, so a trade settling on the
// cash account became a plain withdrawal or deposit. Spending then read
// it as money leaving the household and returns read it as capital —
// deposits and withdrawals are external and are never netted — when it
// was neither: the money moved between the portfolio's own pockets.
func TestTheTypeCodeClassifiesWhatTheNarrativeCannot(t *testing.T) {
	cases := []struct {
		txnType, creditDebit string
		want                 canonical.TxKind
	}{
		// A purchase debits the cash account, a sale credits it —
		// the same kinds the account-statement feed books for the
		// settlement leg of a trade.
		{"NSEC", "D", canonical.TxKindBuy},
		{"NSEC", "C", canonical.TxKindSell},
		{"NFEX", "D", canonical.TxKindFx},
		{"NFEX", "C", canonical.TxKindFx},
		{"NDIV", "C", canonical.TxKindDividend},
		{"NINT", "C", canonical.TxKindInterest},
		{"NCHG", "D", canonical.TxKindFee},
		{"NTAX", "D", canonical.TxKindTax},
		// Not a code the feed carries: the direction still decides.
		{"NTRF", "D", canonical.TxKindWithdrawal},
		{"", "C", canonical.TxKindDeposit},
	}
	for _, c := range cases {
		// The narrative a bare booking code leaves behind.
		if got := cashMovementKind("B37?", c.creditDebit, c.txnType); got != c.want {
			t.Errorf("cashMovementKind(code-only, %q, %q) = %q, want %q",
				c.creditDebit, c.txnType, got, c.want)
		}
	}
}

// TestTheNarrativeStillOutranksTheTypeCode: the floor is a floor. A
// narrative that names the entry is the better witness — it separates a
// stamp duty from a custody price, which no type code does — so it must
// keep deciding wherever it says anything at all.
func TestTheNarrativeStillOutranksTheTypeCode(t *testing.T) {
	if got := cashMovementKind("UMSATZABGABE", "D", "NSEC"); got != canonical.TxKindTax {
		t.Errorf("a stamp duty on a securities entry = %q, want tax", got)
	}
	if got := cashMovementKind("DIVIDENDE", "C", "NSEC"); got != canonical.TxKindDividend {
		t.Errorf("a named dividend = %q, want dividend", got)
	}
}

// TestAReversalIsReadByDirectionAlone: MT940 marks a reversal in the
// credit/debit field (`RC`, `RD`), and the caller pre-negates only a
// plain `D` — so a reversal arrives with a positive amount and a
// direction that means the opposite of what it spells. Reading a type
// code off that states a kind confidently against an unflipped sign,
// which turned a dividend clawback into interest income.
func TestAReversalIsReadByDirectionAlone(t *testing.T) {
	for _, txnType := range []string{"NRTI", "NSEC", "NDIV", "NFEX", "NCHG"} {
		if got := cashMovementKind("W09?", "RC", txnType); got != canonical.TxKindWithdrawal {
			t.Errorf("a reversed credit marked %q = %q, want withdrawal — the direction is the only safe reading",
				txnType, got)
		}
	}
}

// TestAReturnedItemIsNotInterest guards the SWIFT name trap: NRTI is
// RTI, a returned item, and resembles NINT only in spelling.
func TestAReturnedItemIsNotInterest(t *testing.T) {
	if got := cashMovementKind("W09?", "C", "NRTI"); got == canonical.TxKindInterest {
		t.Error("NRTI classified as interest; it is a returned item")
	}
	if got := cashMovementKind("X01?", "C", "NINT"); got != canonical.TxKindInterest {
		t.Errorf("NINT = %q, want interest", got)
	}
}
