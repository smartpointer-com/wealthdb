package ubs

import (
	"testing"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// A payee the bank cut at its line width, and the narrative line its
// remainder opens. Every name, street and town here is invented. The cut
// line is exactly partyLineWidth characters, with an accented letter in
// it, so the width is counted in characters rather than bytes.
const (
	wrappedHead      = "ALEXANDRA BEATRIX ÉLODIE MUSTERFRAU"
	wrappedRemainder = "BEISPIEL-NACHNAME"
	wrappedPayee     = wrappedHead + " " + wrappedRemainder
	wrappedExport    = wrappedHead + "; " + wrappedRemainder + "; EXAMPLE STREET 1; 9999 EXAMPLETOWN"
)

// TestAPayeeCutAtTheLineWidthIsRejoined pins the join in both eras: a
// payee that fills its line exactly, followed by a line that opens with
// a name's remainder, reaches gold whole, and the merchant signature keys
// on the whole name. The description keeps the lines as the bank wrote
// them.
func TestAPayeeCutAtTheLineWidthIsRejoined(t *testing.T) {
	if n := utf8.RuneCountInString(wrappedHead); n != partyLineWidth {
		t.Fatalf("fixture is wrong: the cut line is %d characters, want %d", n, partyLineWidth)
	}
	cases := []struct {
		name        string
		payee       string
		p           webTxPayload
		pdfBackfill bool
		want        string
	}{
		{"the export's next Description1 segment", wrappedHead,
			webTxPayload{Description1: wrappedExport}, false, wrappedPayee},
		{"the statement's next continuation line", wrappedHead,
			webTxPayload{Source: "account_statement_pdf", Continuation: []string{
				wrappedHead, wrappedRemainder, "EXAMPLE STREET 1", "CH EXAMPLETOWN 9999"}}, true, wrappedPayee},
		{"a remainder with a particle and an apostrophe", wrappedHead,
			webTxPayload{Description1: wrappedHead + "; VON D'EXEMPLE; 9999 EXAMPLETOWN"}, false,
			wrappedHead + " VON D'EXEMPLE"},
		{"the leading name tokens only", wrappedHead,
			webTxPayload{Description1: wrappedHead + "; BEISPIEL (EXAMPLE)"}, false,
			wrappedHead + " BEISPIEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _, _ := projectWebTxText(tc.payee, "e-banking payment order", tc.p, tc.pdfBackfill)
			if text.counterparty != tc.want {
				t.Errorf("counterparty = %q, want %q", text.counterparty, tc.want)
			}
		})
	}

	text, _, _ := projectWebTxText(wrappedHead, "e-banking payment order",
		webTxPayload{Description1: wrappedExport}, false)
	if text.description != wrappedExport {
		t.Errorf("description = %q, want the narrative as written", text.description)
	}
	const wantKey = "ALEXANDRA BEATRIX ELODIE MUSTERFRAU BEISPIEL NACHNAME"
	if got := spending.Normalize(text.counterparty, text.description); got != wantKey {
		t.Errorf("signature = %q, want %q", got, wantKey)
	}
}

// TestAPayeeIsJoinedOnlyToANamesRemainder is the guard on the join. A
// payee that does not fill its line was not cut. A next line that carries
// a figure, opens with a label or punctuation, or is a booking type is
// the narrative's next field, never a name's remainder.
func TestAPayeeIsJoinedOnlyToANamesRemainder(t *testing.T) {
	const (
		short = "ALEXANDRA BEATRIX ELODIE MUSTERFRA"   // one character under the width
		long  = "ALEXANDRA BEATRIX ÉLODIE MUSTERFRAUE" // one character over it
		// Firms whose names fill the line and end in a legal form.
		firmAG   = "EXAMPLE PRECISION INSTRUMENTS SW AG"
		firmGmbH = "EXEMPLAR MASCHINEN UND ANLAGEN GMBH"
		firmLtd  = "EXAMPLE TRADING INTERNATIONALE Ltd."
	)
	for _, firm := range []string{firmAG, firmGmbH, firmLtd} {
		if n := utf8.RuneCountInString(firm); n != partyLineWidth {
			t.Fatalf("fixture is wrong: %q is %d characters, want %d", firm, n, partyLineWidth)
		}
	}
	cases := []struct {
		name, payee, description1 string
	}{
		{"a payee under the width", short, short + "; BEISPIEL; 9999 EXAMPLETOWN"},
		{"a payee over the width", long, long + "; BEISPIEL; 9999 EXAMPLETOWN"},
		{"a next line opening with a postcode", wrappedHead, wrappedHead + "; 9999 EXAMPLETOWN"},
		{"a street and its number", wrappedHead, wrappedHead + "; EXAMPLE STREET 1; 9999 EXAMPLETOWN"},
		{"a card number", wrappedHead, wrappedHead + "; 0000XXXXXXXX0000; ATM Withdrawal"},
		{"the payment's reason", wrappedHead, wrappedHead + "; Reason for payment: Rent"},
		{"a reference label", wrappedHead, wrappedHead + "; Reference EXAMPLE"},
		{"a booking type", wrappedHead, wrappedHead + "; ATM Withdrawal"},
		{"an arrival's booking type", wrappedHead, wrappedHead + "; Salary payment"},
		{"a rail's own reference", wrappedHead, wrappedHead + "; TWINT-EXAMPLE"},
		{"an abbreviation", wrappedHead, wrappedHead + "; C/O EXAMPLE AG"},
		{"a firm ending in AG, then its branch", firmAG, firmAG + "; Branch Exampletown"},
		{"a firm ending in GMBH, then its town", firmGmbH, firmGmbH + "; Exampletown"},
		{"a legal form in mixed case with a dot", firmLtd, firmLtd + "; Exampletown"},
		{"a country prefix on the town", wrappedHead, wrappedHead + "; AT EXAMPLETOWN"},
		{"no next line", wrappedHead, wrappedHead},
		{"a payee the narrative does not lead with", wrappedHead, "EXAMPLE PAYEE; BEISPIEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _, _ := projectWebTxText(tc.payee, "e-banking payment order",
				webTxPayload{Description1: tc.description1}, false)
			if text.counterparty != tc.payee {
				t.Errorf("counterparty = %q, want %q left as promoted", text.counterparty, tc.payee)
			}
		})
	}

	// The statement era refuses the same way: a payee that fills its
	// line, followed by its street and number, is a whole name.
	text, _, _ := projectWebTxText(wrappedHead, "E-BANKING PAYMENT ORDER", webTxPayload{
		Source:       "account_statement_pdf",
		Continuation: []string{wrappedHead, "EXAMPLE STREET 1", "CH EXAMPLETOWN 9999"},
	}, true)
	if text.counterparty != wrappedHead {
		t.Errorf("statement counterparty = %q, want %q left as promoted", text.counterparty, wrappedHead)
	}
}

// TestAWrappedPayeeReachesGoldWhole runs the join through the emitted
// stream, one row per era, so the column gold stores is the one pinned.
func TestAWrappedPayeeReachesGoldWhole(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedWebTextRow(t, r, "WR1", 100*86400, 25.0, nil, wrappedHead, "e-banking payment order",
		`{"Description1":"`+wrappedExport+`","Description2":"e-banking payment order","Description3":""}`)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, "WR2", 200*86400, 30.0, nil, wrappedHead, "E-BANKING PAYMENT ORDER",
		`{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER","internal_transfer":false,"counter_account":null,"continuation":["`+
			wrappedHead+`","`+wrappedRemainder+`","EXAMPLE STREET 1","CH EXAMPLETOWN 9999"]}`)
	got := drainTx(t, emitWebStream(t, r))
	checkText(t, got, map[string]textCase{
		"WR1@" + textAcct: {wrappedExport, wrappedPayee, "e-banking payment order"},
		"WR2@" + textAcct: {"E-BANKING PAYMENT ORDER; " + wrappedHead + "; " + wrappedRemainder +
			"; EXAMPLE STREET 1; CH EXAMPLETOWN 9999", wrappedPayee, "E-BANKING PAYMENT ORDER"},
	})
}
