package ubs

import (
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// mandateFunding is the continuation of a mandate-funding move: a marker of
// internalTransferMarkers, and nothing that names anyone.
var mandateFunding = []string{"UEBERTRAG EXAMPLE MANDATE"}

// pdfPayload builds a pre-2024 Account-Statement PDF-backfill payload with only
// the fields pdfCashIsExternal reads, and returns it decoded the way the reader
// decodes a row — so the JSON tag names stay pinned. All values synthetic /
// IBAN-spec placeholder letters (AGENTS.md §4) — no real account IDs.
func pdfPayload(t *testing.T, counter, bookingType string, continuation []string) webTxPayload {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"source":          "account_statement_pdf",
		"counter_account": counter,
		"booking_type":    bookingType,
		"continuation":    continuation,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	p, ok := decodeWebTxPayload(string(b))
	if !ok {
		t.Fatalf("payload does not decode: %s", b)
	}
	return p
}

// TestPdfInternalTransfer pins the rule's two readings: a statement row's
// markers count under a cash-moving booking type only, and an advice's purpose
// lines count with no booking type at all, in either silver's spelling of an
// advice. Every narrative is synthetic.
func TestPdfInternalTransfer(t *testing.T) {
	advice := func(source string, purpose ...string) webTxPayload {
		return webTxPayload{Source: source, Document: sourceAdvicePDF, Continuation: purpose}
	}
	cases := []struct {
		name string
		p    webTxPayload
		want bool
	}{
		{"mandate funding", webTxPayload{BookingType: "CREDIT",
			Continuation: []string{"INCREAS US EQUITY PORTFOLIO", "P. HOLDER"}}, true},
		{"mandate reduction by telephone", webTxPayload{BookingType: "PAYMENT ORDER BY TELEPHONE",
			Continuation: []string{"UEBERTRAG", "REDUKTION US EQUITY MANDAT"}}, true},
		{"marker in lower case", webTxPayload{BookingType: "SPECIAL PAYMENT ORDER",
			Continuation: []string{"reduction investment portfolio"}}, true},
		{"external credit", webTxPayload{BookingType: "CREDIT",
			Continuation: []string{"SOME EXTERNAL BANK", "1 times Incoming SIC-payment"}}, false},
		{"salary", webTxPayload{BookingType: "CREDIT", Continuation: []string{"SALARY PAYER LTD"}}, false},
		{"a settlement is no flow", webTxPayload{BookingType: "SHARE",
			Continuation: []string{"MANAGE US EQ PORTFOLIO"}}, false},
		{"a dividend is no flow", webTxPayload{BookingType: "DIVIDEND",
			Continuation: []string{"MANAGE PORTFOLIO"}}, false},
		{"advice for a mandate", advice(sourceAdvicePDF, "Increase Example Mandate"), true},
		{"older silver's advice", advice(sourceStatementPDF, "Uebertrag Example Portfolio"), true},
		{"advice for a payment", advice(sourceAdvicePDF, "YOUR PAYMENT ORDER DATED 01012020"), false},
	}
	for _, c := range cases {
		if got := pdfInternalTransfer(c.p); got != c.want {
			t.Errorf("%s: pdfInternalTransfer = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPdfCashIsExternalDecisionMatrix pins the full classifier so the veto sits
// correctly relative to the existing IBAN / booking-type rules and nothing else
// regressed.
//
// The first rows are the CAPITAL-FABRICATION regression. A mandate-funding or
// book-transfer row whose continuation carries a name-free marker, whose
// counter is a CH IBAN absent from the own set, and whose booking type carries
// no guard token passes every other EXTERNAL condition. Classified EXTERNAL it
// fabricates owner capital on an intra-relationship move, so the marker vetoes
// external before the IBAN and the rail promotions. The same row without the
// marker is external, which proves the veto is what decides.
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
		{"an internal transfer vetoes external", extCH, "CREDIT", true, false, true, false},
		{"an internal transfer vetoes the rail promotion", "", "PAYMENT ORDER", true, true, true, false},
		{"the same transfer without the marker is external", extCH, "CREDIT", false, false, true, true},
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
		// TWINT crosses the boundary in both directions: the debits sit
		// in the outbound set, the credit and the reversal in the
		// inbound one, so neither side is left one-legged.
		{"outbound TWINT payment, no counter", "", "PAYMENT UBS TWINT", false, true, true, true},
		{"outbound TWINT debit, no counter", "", "DEBIT UBS TWINT", false, true, true, true},
		{"inbound TWINT credit, no counter", "", "CREDIT UBS TWINT", false, false, true, true},
		{"inbound TWINT reversal, no counter", "", "REVERSAL UBS TWINT", false, false, true, true},
		{"deep era: inbound TWINT credit stays internal", "", "CREDIT UBS TWINT", false, false, false, false},
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
			var continuation []string
			if c.internal {
				continuation = mandateFunding
			}
			got := pdfCashIsExternal(pdfPayload(t, c.counter, c.booking, continuation), own, c.outbound, c.railEra)
			if got != c.want {
				t.Errorf("pdfCashIsExternal = %v, want %v (counter=%q booking=%q internal=%v outbound=%v railEra=%v)",
					got, c.want, c.counter, c.booking, c.internal, c.outbound, c.railEra)
			}
		})
	}
}

// TestPdfCashIsExternalMalformedPayload keeps the conservative default: an
// unparseable payload decodes to nothing and is never external (never
// fabricates capital).
func TestPdfCashIsExternalMalformedPayload(t *testing.T) {
	p, ok := decodeWebTxPayload("{not json")
	if ok {
		t.Fatal("a malformed payload must report its decode failure")
	}
	if pdfCashIsExternal(p, map[string]bool{}, true, true) {
		t.Error("malformed payload must default to INTERNAL")
	}
}

// TestUndecodablePayloadStaysGated pins the gate a decode failure must not
// open. The reader routes a row through pdfCashIsExternal only when its
// payload names the PDF backfill as the source, so a payload it cannot read
// at all — valid JSON, one field of an unexpected type — would otherwise skip
// the classifier entirely and keep the row's withdrawal kind, which is the
// permissive outcome on the guard against fabricating owner capital. An
// unreadable payload is treated as a backfill row instead, and the
// classifier's own conservative default demotes it.
func TestUndecodablePayloadStaysGated(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	seedRailEraAnchor(t, r)

	const rail = `"source":"account_statement_pdf",` +
		`"booking_type":"E-BANKING PAYMENT ORDER"`
	// Control: a readable payment-order row promotes to EXTERNAL on the
	// rail evidence and keeps its withdrawal kind.
	seedWebTxRaw(t, r, "RAIL-READABLE", vetoAcctA, vetoDay1, "CHF", -40,
		"E-BANKING PAYMENT ORDER", "{"+rail+`,"continuation":[]}`)
	// The same row whose continuation is a string where the reader expects
	// a list: the decode fails, so no field of it is readable.
	seedWebTxRaw(t, r, "RAIL-UNDECODABLE", vetoAcctA, vetoDay2, "CHF", -70,
		"E-BANKING PAYMENT ORDER", "{"+rail+`,"continuation":"not-a-list"}`)

	kinds := emittedKinds(t, r, nil)
	internal := emittedInternal(t, r, nil)
	if got := kinds["RAIL-READABLE@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Fatalf("control = %q, want withdrawal — the promotion path must be open", got)
	}
	if internal["RAIL-READABLE@"+vetoAcctA] {
		t.Error("control carries the conduit verdict; the promotion path must be open")
	}
	// The gate still catches it — the verdict just rides the payload now,
	// so the row keeps a kind the spending population can read.
	if !internal["RAIL-UNDECODABLE@"+vetoAcctA] {
		t.Error("undecodable row: want the conduit verdict — it must not skip the gate")
	}
}
