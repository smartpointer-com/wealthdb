package ubs

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// The statement era's TWINT rows, end to end: the kind is money moving, the
// amount is oriented by the kind, the narrative leads with the booking type
// and then the payee the statement prints first, and the merchant signature
// spending builds from the two text columns is the payee alone — never the
// phone number a person-to-person payment carries in its later segments.
// Every value is synthetic; the row shapes are the statement's.

func seedTwintRows(t *testing.T, r *webReader) {
	t.Helper()
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// A merchant payment: payee, street, town.
	seedWebTextRow(t, r, "TW1", 200*86400, 12.5, nil, "EXAMPLE CHOCOLATIER AG", "PAYMENT UBS TWINT",
		`{"source":"account_statement_pdf","booking_type":"PAYMENT UBS TWINT","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE CHOCOLATIER AG","EXAMPLE STREET 1","CH EXAMPLETOWN 9999"]}`)
	// A person-to-person payment: payee, phone number, TWINT reference.
	seedWebTextRow(t, r, "TW2", 201*86400, 20.0, nil, "EXAMPLE, PERSON", "DEBIT UBS TWINT",
		`{"source":"account_statement_pdf","booking_type":"DEBIT UBS TWINT","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE, PERSON","+41 00 000 00 00","TWINT-EXAMPLE"]}`)
	// A payment received, and a merchant payment reversed.
	seedWebTextRow(t, r, "TW3", 202*86400, nil, 30.0, "EXAMPLE, PERSON", "CREDIT UBS TWINT",
		`{"source":"account_statement_pdf","booking_type":"CREDIT UBS TWINT","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE, PERSON","+41 00 000 00 00","TWINT-EXAMPLE"]}`)
	seedWebTextRow(t, r, "TW4", 203*86400, nil, 12.5, "EXAMPLE CHOCOLATIER AG", "REVERSAL UBS TWINT",
		`{"source":"account_statement_pdf","booking_type":"REVERSAL UBS TWINT","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE CHOCOLATIER AG","EXAMPLE STREET 1","CH EXAMPLETOWN 9999"]}`)
}

// TestTwintMovesMoney pins the kinds and signed amounts the statement era's
// four TWINT types reach gold with: the outflows as withdrawals, the inflows
// as deposits, none demoted to `other` in the MT940 era — so they enter the
// spending population and the external cash flows.
func TestTwintMovesMoney(t *testing.T) {
	r := newWebTxFixture(t)
	seedTwintRows(t, r)
	got := drainTx(t, emitWebStream(t, r))
	want := map[string]struct {
		kind canonical.TxKind
		net  string
	}{
		"TW1@" + textAcct: {canonical.TxKindWithdrawal, "-12.5"},
		"TW2@" + textAcct: {canonical.TxKindWithdrawal, "-20"},
		"TW3@" + textAcct: {canonical.TxKindDeposit, "30"},
		"TW4@" + textAcct: {canonical.TxKindDeposit, "12.5"},
	}
	for id, w := range want {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if tx.Kind != w.kind {
			t.Errorf("%s Kind = %q, want %q", id, tx.Kind, w.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != w.net {
			t.Errorf("%s NetAmount = %v, want %s", id, tx.NetAmount, w.net)
		}
	}
}

// TestTwintNarrativeLeadsWithPayee pins the text columns and the signature
// built from them: the description is "<BOOKING TYPE>; <payee>; …", the
// counterparty is the payee, and spending.Normalize keys the row by the payee
// — the phone-number segment of a person-to-person payment never survives
// into the signature.
func TestTwintNarrativeLeadsWithPayee(t *testing.T) {
	r := newWebTxFixture(t)
	seedTwintRows(t, r)
	got := drainTx(t, emitWebStream(t, r))
	checkText(t, got, map[string]textCase{
		"TW1@" + textAcct: {"PAYMENT UBS TWINT; EXAMPLE CHOCOLATIER AG; EXAMPLE STREET 1; CH EXAMPLETOWN 9999", "EXAMPLE CHOCOLATIER AG", "PAYMENT UBS TWINT"},
		"TW2@" + textAcct: {"DEBIT UBS TWINT; EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE", "EXAMPLE, PERSON", "DEBIT UBS TWINT"},
		"TW3@" + textAcct: {"CREDIT UBS TWINT; EXAMPLE, PERSON; +41 00 000 00 00; TWINT-EXAMPLE", "EXAMPLE, PERSON", "CREDIT UBS TWINT"},
		"TW4@" + textAcct: {"REVERSAL UBS TWINT; EXAMPLE CHOCOLATIER AG; EXAMPLE STREET 1; CH EXAMPLETOWN 9999", "EXAMPLE CHOCOLATIER AG", "REVERSAL UBS TWINT"},
	})
	signatures := map[string]string{
		"TW1@" + textAcct: "EXAMPLE CHOCOLATIER AG",
		"TW2@" + textAcct: "EXAMPLE PERSON",
		"TW3@" + textAcct: "EXAMPLE PERSON",
		"TW4@" + textAcct: "EXAMPLE CHOCOLATIER AG",
	}
	for id, want := range signatures {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		sig := spending.Normalize(textOrNil(tx.Counterparty), textOrNil(tx.Description))
		if sig != want {
			t.Errorf("%s signature = %q, want %q", id, sig, want)
		}
		if strings.ContainsAny(sig, "0123456789") {
			t.Errorf("%s signature %q carries a figure — the phone number leaked past the payee", id, sig)
		}
	}
}
