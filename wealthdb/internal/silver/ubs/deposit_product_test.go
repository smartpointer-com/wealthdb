package ubs

import "testing"

// A deposit product — a call deposit, a fixed-term deposit, a notice
// account — is the one thing the bank moves money into that it never
// lists as an account. Every movement of one is booked on the account
// that FUNDS it, so these rows are the only trace the product leaves,
// and the booking type is the only thing that tells one of them from
// another.
//
// The export era hid that. Its `Description1` names the product and
// nothing else, and the caption path returned it verbatim, so a
// principal movement and the interest the product paid reached gold
// with byte-identical narratives — not ambiguous, IDENTICAL, which no
// downstream tier can undo.
//
// Every value below is synthetic (CLAUDE.md §4).

const depositCaption = "Example Call Deposit; Serial no. 00000"

// TestADepositProductsBookingTypeReachesTheNarrative is the fix: the
// type leads, the product name follows, and the composed shape is the
// one the statement era already produces — so one spelling reaches
// both eras.
func TestADepositProductsBookingTypeReachesTheNarrative(t *testing.T) {
	caption := depositCaption
	for _, tc := range []struct {
		name, booking, want string
	}{
		{"principal out", "Call Deposit Increase",
			"Call Deposit Increase; " + depositCaption},
		{"principal back", "Call Deposit Repayment",
			"Call Deposit Repayment; " + depositCaption},
		{"the statement era's shouted spelling", "FIXED TERM DEPOSIT REPAYMENT",
			"FIXED TERM DEPOSIT REPAYMENT; " + depositCaption},
		// The interest is income, not a transfer, and it is the one
		// movement of a deposit that must NOT be swept in with the
		// principal. It keeps the caption alone, exactly as before.
		{"interest is left alone", "Call Deposit Interest Payment", depositCaption},
		// And nothing else moves: a security caption is what the
		// caption path exists for, and gold's name lookups key on it.
		{"a security caption is untouched", "Dividend", depositCaption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := webDescription(&caption, tc.booking, webTxPayload{})
			if got == nil {
				t.Fatalf("description is nil")
			}
			if *got != tc.want {
				t.Errorf("description = %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestTheDepositVocabularyIsPrincipalOnly pins the list itself. A
// booking type wrongly in it takes real income out of the statement;
// one wrongly missing leaves a movement reading as a crossing to an
// account nobody collects.
func TestTheDepositVocabularyIsPrincipalOnly(t *testing.T) {
	for _, in := range []string{
		"CALL DEPOSIT NEW INVESTMENT", "CALL DEPOSIT INCREASE",
		"CALL DEPOSIT DECREASE", "CALL DEPOSIT REPAYMENT",
		"FIXED TERM DEPOSIT NEW INVESTMENT", "FIXED TERM DEPOSIT INCREASE",
		"FIXED TERM DEPOSIT DECREASE", "FIXED TERM DEPOSIT REPAYMENT",
		// The export title-cases what the statement shouts; both fold.
		"Call Deposit Decrease", "  fixed term deposit repayment  ",
	} {
		if !isDepositProductBooking(in) {
			t.Errorf("%q is a principal movement and was not recognised", in)
		}
	}
	for _, in := range []string{
		"CALL DEPOSIT INTEREST PAYMENT", "FIXED TERM DEPOSIT INTEREST PAYMENT",
		"INTEREST", "DIVIDEND", "E-BANKING PAYMENT ORDER", "", "DEPOSIT",
	} {
		if isDepositProductBooking(in) {
			t.Errorf("%q was taken for a principal movement", in)
		}
	}
}
