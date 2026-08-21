package ubs

import (
	"encoding/json"
	"testing"
)

// pdfPayload builds a pre-2024 Account-Statement PDF-backfill payload with only
// the fields pdfCashIsExternal reads. All values synthetic / IBAN-spec placeholder
// letters (CLAUDE.md §4) — no real account IDs.
func pdfPayload(t *testing.T, counter, bookingType string, internalTransfer bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"source":            "account_statement_pdf",
		"counter_account":   counter,
		"booking_type":      bookingType,
		"internal_transfer": internalTransfer,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// TestPdfCashIsExternalInternalTransferVeto is the CAPITAL-FABRICATION regression.
// A mandate-funding / book-transfer row that the collector's own
// name-free markers already flagged internal_transfer=true, whose counter_account
// is a non-own CH/LI IBAN ABSENT from the relationship's `accounts` set, and whose
// booking_type carries none of the guard tokens (HYPOTHEK/MATURITY/CLOSING —
// the mandate markers live on the continuation lines, not booking_type), satisfies
// all four legacy EXTERNAL conditions. Before the fix it was classified EXTERNAL,
// fabricating owner capital on an intra-relationship conduit move. The parser's
// internal_transfer=true must VETO external BEFORE the IBAN promotion.
func TestPdfCashIsExternalInternalTransferVeto(t *testing.T) {
	// A CH IBAN that is NOT in the own set — an own mandate/portfolio destination
	// the parser recognised via continuation-line markers but that never made it
	// into `accounts`, so own-IBAN membership can't demote it.
	const unknownCH = "CH00 0000 0000 0000 0000 0"
	own := map[string]bool{} // deliberately empty: the destination is absent

	// internal_transfer=true ⇒ INTERNAL despite passing every IBAN/booking check.
	pInternal := pdfPayload(t, unknownCH, "UEBERTRAG", true)
	if pdfCashIsExternal(pInternal, own, false, true) {
		t.Fatalf("FABRICATION: internal_transfer=true mandate-funding move to a non-own "+
			"absent CH IBAN classified EXTERNAL; parser flag must veto. payload=%s", pInternal)
	}

	// Control: the SAME row without the parser flag DOES promote to external via the
	// IBAN path — proving the veto (not some other guard) is what flipped it.
	pExternal := pdfPayload(t, unknownCH, "UEBERTRAG", false)
	if !pdfCashIsExternal(pExternal, own, false, true) {
		t.Errorf("control: non-own CH IBAN, no internal_transfer, no guard token should be EXTERNAL; payload=%s", pExternal)
	}

	// The parser flag also vetoes the outbound payment-rail promotion: a payment
	// order whose continuation markers identified an intra-relationship move must
	// stay INTERNAL even though its booking type is in the rail set.
	pRailInternal := pdfPayload(t, "", "E-BANKING PAYMENT ORDER", true)
	if pdfCashIsExternal(pRailInternal, own, true, true) {
		t.Fatalf("FABRICATION: internal_transfer=true payment order classified EXTERNAL "+
			"via the rail promotion; parser flag must veto. payload=%s", pRailInternal)
	}
}

// TestPdfCashIsExternalDecisionMatrix pins the full classifier so the veto sits
// correctly relative to the existing IBAN / booking-type rules and nothing else
// regressed.
func TestPdfCashIsExternalDecisionMatrix(t *testing.T) {
	const ownCH = "CH11 1111 1111 1111 1111 1"
	const extCH = "CH22 2222 2222 2222 2222 2"
	const extDE = "DE33 3333 3333 3333 3333 33"
	own := map[string]bool{normalizeIBAN(ownCH): true}

	cases := []struct {
		name     string
		counter  string
		booking  string
		internal bool
		outbound bool
		railEra  bool
		want     bool
	}{
		{"parser-internal vetoes external", extCH, "SALE", true, false, true, false},
		{"parser-internal vetoes even a bare deposit", extCH, "", true, false, true, false},
		{"external CH counterparty", extCH, "SALE", false, false, true, true},
		{"external CH counterparty, deep era", extCH, "SALE", false, false, false, true},
		{"own CH counterparty is internal", ownCH, "SALE", false, false, true, false},
		{"empty counter is internal", "", "SALE", false, false, true, false},
		{"non-CH/LI counter is internal", extDE, "SALE", false, false, true, false},
		{"mortgage payoff is internal", extCH, "HYPOTHEK AMORTISATION", false, false, true, false},
		{"structured-product maturity is internal", extCH, "PRODUCT MATURITY", false, false, true, false},
		{"structured-product closing is internal", extCH, "POSITION CLOSING", false, false, true, false},
		// Interbank-rail promotion, outbound: an owner-initiated payment
		// through the rails is external even with no counter IBAN — the shape
		// of the own-name wire to another bank and of everyday bill payments.
		{"outbound payment order, no counter", "", "E-BANKING PAYMENT ORDER", false, true, true, true},
		{"outbound direct debit, no counter", "", "DIRECT DEBIT", false, true, true, true},
		{"outbound ATM cash, no counter", "", "ATM WITHDRAWAL", false, true, true, true},
		{"rail booking title-cased matches exactly", "", "e-banking payment order", false, true, true, true},
		// Interbank-rail promotion, inbound: the mirror shapes — an incoming
		// wire, its e-banking flavour, salary. One-sided counting would
		// fabricate return.
		{"inbound bare credit, no counter", "", "CREDIT", false, false, true, true},
		{"inbound e-banking credit, no counter", "", "E-BANKING CREDIT", false, false, true, true},
		{"inbound salary payment, no counter", "", "SALARY PAYMENT", false, false, true, true},
		// Deep era (before any MT940 coverage): the rail promotion is off in
		// BOTH directions — that era's inbound capital rides the onboarding
		// step-ups, and one-sided counting would fabricate return.
		{"deep era: outbound payment order stays internal", "", "E-BANKING PAYMENT ORDER", false, true, false, false},
		{"deep era: inbound bare credit stays internal", "", "CREDIT", false, false, false, false},
		{"deep era: salary stays internal", "", "SALARY PAYMENT", false, false, false, false},
		// The promotion is direction-matched and shape-matched; everything
		// below stays with the conservative default.
		{"inbound leg of an outbound rail booking is internal", "", "E-BANKING PAYMENT ORDER", false, false, true, false},
		{"outbound leg of an arrival booking is internal", "", "CREDIT", false, true, true, false},
		{"own-product cash parking is internal", "", "CALL DEPOSIT DECREASE", false, false, true, false},
		{"own-product repayment is internal", "", "FIXED TERM DEPOSIT REPAYMENT", false, false, true, false},
		{"outbound rail to an own counter is internal", ownCH, "PAYMENT ORDER", false, true, true, false},
		{"inbound arrival from an own counter is internal", ownCH, "CREDIT", false, false, true, false},
		// A rail booking with a populated non-CH/LI counter is still external:
		// the rail evidence carries, the counter country guard applies only to
		// the IBAN-promotion path.
		{"outbound rail to a non-CH/LI counter", extDE, "PAYMENT ORDER", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pdfCashIsExternal(pdfPayload(t, c.counter, c.booking, c.internal), own, c.outbound, c.railEra)
			if got != c.want {
				t.Errorf("pdfCashIsExternal = %v, want %v (counter=%q booking=%q internal=%v outbound=%v railEra=%v)",
					got, c.want, c.counter, c.booking, c.internal, c.outbound, c.railEra)
			}
		})
	}
}

// TestPdfCashIsExternalMalformedPayload keeps the conservative default: an
// unparseable payload is never external (never fabricates capital).
func TestPdfCashIsExternalMalformedPayload(t *testing.T) {
	if pdfCashIsExternal("{not json", map[string]bool{}, true, true) {
		t.Error("malformed payload must default to INTERNAL")
	}
}
