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

// TestPdfCashIsExternalInternalTransferVeto is the CAPITAL-FABRICATION regression
// (defect #1). A mandate-funding / book-transfer row that the collector's own
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
	if pdfCashIsExternal(pInternal, own) {
		t.Fatalf("FABRICATION: internal_transfer=true mandate-funding move to a non-own "+
			"absent CH IBAN classified EXTERNAL; parser flag must veto. payload=%s", pInternal)
	}

	// Control: the SAME row without the parser flag DOES promote to external via the
	// IBAN path — proving the veto (not some other guard) is what flipped it.
	pExternal := pdfPayload(t, unknownCH, "UEBERTRAG", false)
	if !pdfCashIsExternal(pExternal, own) {
		t.Errorf("control: non-own CH IBAN, no internal_transfer, no guard token should be EXTERNAL; payload=%s", pExternal)
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
		want     bool
	}{
		{"parser-internal vetoes external", extCH, "SALE", true, false},
		{"parser-internal vetoes even a bare deposit", extCH, "", true, false},
		{"external CH counterparty", extCH, "SALE", false, true},
		{"own CH counterparty is internal", ownCH, "SALE", false, false},
		{"empty counter is internal", "", "SALE", false, false},
		{"non-CH/LI counter is internal", extDE, "SALE", false, false},
		{"mortgage payoff is internal", extCH, "HYPOTHEK AMORTISATION", false, false},
		{"structured-product maturity is internal", extCH, "PRODUCT MATURITY", false, false},
		{"structured-product closing is internal", extCH, "POSITION CLOSING", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pdfCashIsExternal(pdfPayload(t, c.counter, c.booking, c.internal), own)
			if got != c.want {
				t.Errorf("pdfCashIsExternal = %v, want %v (counter=%q booking=%q internal=%v)",
					got, c.want, c.counter, c.booking, c.internal)
			}
		})
	}
}

// TestPdfCashIsExternalMalformedPayload keeps the conservative default: an
// unparseable payload is never external (never fabricates capital).
func TestPdfCashIsExternalMalformedPayload(t *testing.T) {
	if pdfCashIsExternal("{not json", map[string]bool{}) {
		t.Error("malformed payload must default to INTERNAL")
	}
}
