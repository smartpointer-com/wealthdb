package ubs

import (
	"sort"
	"strconv"
	"testing"
)

// A bundled payment order reaches gold as its payments, never as the batch.
//
// The account statement books a batch of e-banking payments as ONE movement
// carrying the batch total, with the beneficiaries listed under it. Left
// whole, that row reaches gold as a single transaction whose narrative is
// every beneficiary concatenated and whose amount is a sum nobody paid to
// anybody — one merchant signature standing for the lot. The ubs-web parser
// splits the batch in silver, so what arrives here is plain single
// transactions. These pin that: the adapter needs no bundle vocabulary, and
// would need one if the split ever stopped happening.
//
// Every beneficiary, amount and id below is invented.

// multiLegPayload is one split payment's payload: the batch's booking type
// and the leg's own narrative lines, plus the batch position the parser
// records for provenance — a field the adapter does not read and must not
// need to.
func multiLegPayload(index, count int, continuation ...string) string {
	lines := ""
	for i, c := range continuation {
		if i > 0 {
			lines += ","
		}
		lines += `"` + c + `"`
	}
	return `{"source":"account_statement_pdf","booking_type":"MULTI E-BANKING ORDER",` +
		`"internal_transfer":false,"counter_account":null,"continuation":[` + lines + `],` +
		`"multi_leg":{"index":` + strconv.Itoa(index) + `,"count":` + strconv.Itoa(count) +
		`,"rail":"E-Banking CHF domestic"}}`
}

func TestSplitBundleReachesGoldAsPlainTransactions(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	// A CSV-feed row first, so the statement rows fall in the MT940 rail
	// era and classify as they do in production (see seedWebTextRows).
	seedWebTextRow(t, r, "W-EARLY", 100*86400, 80.0, nil, "EXAMPLE PAYEE",
		"e-banking payment order",
		`{"Description1":"EXAMPLE PAYEE","Description2":"e-banking payment order","Description3":""}`)
	// One batch of 300.00, split into the two payments that make it up.
	seedWebTextRow(t, r, "P-LEG1", 300*86400, 100.0, nil,
		"EXAMPLE DENTAL AG", "MULTI E-BANKING ORDER",
		multiLegPayload(1, 2, "EXAMPLE DENTAL AG", "9999 EXAMPLETOWN"))
	seedWebTextRow(t, r, "P-LEG2", 300*86400, 200.0, nil,
		"NORTHWIND CLINIC", "MULTI E-BANKING ORDER",
		multiLegPayload(2, 2, "NORTHWIND CLINIC", "9999 EXAMPLEBURG"))
	got := drainTx(t, emitWebStream(t, r))
	// Gold scopes a web id by its account.
	leg1, leg2 := "P-LEG1@"+textAcct, "P-LEG2@"+textAcct

	// Each payment carries its own beneficiary and its own amount — the
	// two things a lumped batch destroys.
	checkText(t, got, map[string]textCase{
		leg1: {
			desc: "MULTI E-BANKING ORDER; EXAMPLE DENTAL AG; 9999 EXAMPLETOWN",
			cp:   "EXAMPLE DENTAL AG", cat: "MULTI E-BANKING ORDER",
		},
		leg2: {
			desc: "MULTI E-BANKING ORDER; NORTHWIND CLINIC; 9999 EXAMPLEBURG",
			cp:   "NORTHWIND CLINIC", cat: "MULTI E-BANKING ORDER",
		},
	})

	// The batch total reaches gold as no row of its own — only as the
	// payments that make it up.
	var amounts []string
	for id, tx := range got {
		if tx.NetAmount == nil {
			t.Fatalf("%s has no amount", id)
		}
		if tx.NetAmount.String() == "-300" {
			t.Errorf("%s is the batch total; gold must only see the payments", id)
		}
		if id != leg1 && id != leg2 {
			continue
		}
		amounts = append(amounts, tx.NetAmount.String())
	}
	sort.Strings(amounts)
	if len(amounts) != 2 || amounts[0] != "-100" || amounts[1] != "-200" {
		t.Errorf("amounts = %v, want the two payments the batch was made of", amounts)
	}
}
