package ubs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The mortgage reference, and the narrative cliff that lost it.
//
// Through the statement era a mortgage payment's composed description
// carried the word HYPOTHEK, which is what the narrative rule fires on.
// The export era prints the same stamp in `Description2` and composes a
// bare "Maturity" — so the same payment on the same account stopped
// being recognised, and the statement drew it as a move to an account
// nobody collects.
//
// Every value below is synthetic: a placeholder branch, a placeholder
// account base, and tranche codes that name no real product.

const (
	synMortgageID    = "0000 00111111.AAA 0009"
	synMortgageStamp = "HYPOTHEK 111111.AAA 0009"
)

// TestAMortgageStampAndAnAccountIDFoldTogether pins the one thing the
// lookup rests on: the bank's stamp and gold's account id are two
// spellings of one reference, and they differ only in the branch the
// stamp omits and the padding it drops.
func TestAMortgageStampAndAnAccountIDFoldTogether(t *testing.T) {
	fromID := mortgageRefKey(synMortgageID, true)
	fromStamp := mortgageRefFromNarrative("Interest; " + synMortgageStamp)
	if fromID == "" || fromID != fromStamp {
		t.Fatalf("id folds to %q, stamp folds to %q — they must agree", fromID, fromStamp)
	}
	// The branch is dropped rather than compared: the stamp omits it, so
	// the same base and tranche at another branch has to fold onto the
	// same key or the stamp would resolve to nothing.
	if other := mortgageRefKey("0001 00111111.AAA 0009", true); other != fromID {
		t.Errorf("the branch is not part of the key; got %q want %q", other, fromID)
	}
	// A different tranche of the same mortgage is a different account.
	if same := mortgageRefKey("0000 00111111.BBB 0009", true); same == fromID {
		t.Error("two tranches folded onto one key — the tranche code is load-bearing")
	}
}

// TestOnlyAMortgageStampIsReadAsOne keeps the reach narrow. Description2
// carries booking-type phrases, e-billing references and payment
// references on most rows; none of them names a mortgage.
func TestOnlyAMortgageStampIsReadAsOne(t *testing.T) {
	for _, narrative := range []string{
		"",
		"E-BANKING ORDER",
		"EBILL-RECHNUNG 12345678-9 01/02",
		"DIVIDEND",
		"HYPOTHEK",                 // the word alone names nothing
		"HYPOTHEK 111111.AAA",      // no sequence
		"MORTGAGE 111111.AAA 0009", // a different word entirely
	} {
		if got := mortgageRefFromNarrative(narrative); got != "" {
			t.Errorf("%q was read as the mortgage reference %q", narrative, got)
		}
	}
}

// TestTheExportEraResolvesItsMortgageStamp is the defect itself: a row
// whose only mortgage evidence is Description2 comes out carrying the
// account id gold holds the mortgage under, so the enrichment pass can
// resolve a far account and the statement can draw debt service.
func TestTheExportEraResolvesItsMortgageStamp(t *testing.T) {
	r := newWebTxFixture(t)
	if _, err := r.db.Exec(`
        CREATE TABLE mortgages (snapshot_at INTEGER, account_external_id TEXT,
            banking_relationship_id TEXT, portfolio_external_id TEXT,
            currency_iso TEXT, description TEXT, payload TEXT);
        INSERT INTO mortgages (snapshot_at, account_external_id, currency_iso, payload)
        VALUES (1000, ?, 'CHF', '{}')`, synMortgageID); err != nil {
		t.Fatalf("seed mortgages: %v", err)
	}

	idx, err := r.buildMortgageAccountIndex(context.Background())
	if err != nil {
		t.Fatalf("buildMortgageAccountIndex: %v", err)
	}
	key := mortgageRefFromNarrative(synMortgageStamp)
	if got := idx[key]; got != synMortgageID {
		t.Fatalf("the stamp resolves to %q, want %q", got, synMortgageID)
	}

	// And the payload the row reaches gold with carries it under the one
	// key both eras answer on.
	payload := withCounterAccount(json.RawMessage(`{"Description1":"Maturity"}`), idx[key])
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("payload no longer parses: %v", err)
	}
	if out["counter_account"] != synMortgageID {
		t.Errorf("payload counter_account = %v, want %q", out["counter_account"], synMortgageID)
	}
}

// TestAMortgageIndexIsEmptyWithoutTheTables pins the older-silver path:
// a vintage with no mortgage tables builds an empty index rather than
// failing the whole load.
func TestAMortgageIndexIsEmptyWithoutTheTables(t *testing.T) {
	r := newWebTxFixture(t)
	idx, err := r.buildMortgageAccountIndex(context.Background())
	if err != nil {
		t.Fatalf("buildMortgageAccountIndex: %v", err)
	}
	if len(idx) != 0 {
		t.Errorf("index has %d entries, want 0", len(idx))
	}
}

// TestAMortgagePaymentReachesGoldNamingItsMortgage drives the real
// transaction stream, because the fold and the index are only worth
// anything if the row loop actually consults them. Deleting the lookup
// leaves every other test in this file green.
func TestAMortgagePaymentReachesGoldNamingItsMortgage(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	if _, err := r.db.Exec(`
        CREATE TABLE mortgages (snapshot_at INTEGER, account_external_id TEXT,
            banking_relationship_id TEXT, portfolio_external_id TEXT,
            currency_iso TEXT, description TEXT, payload TEXT);
        INSERT INTO mortgages (snapshot_at, account_external_id, currency_iso, payload)
        VALUES (1000, ?, 'CHF', '{}')`, synMortgageID); err != nil {
		t.Fatalf("seed mortgages: %v", err)
	}
	// The export era as it actually writes a mortgage payment: a bare
	// booking word in Description1, the stamp in Description2, and
	// nothing anywhere else that names the mortgage.
	seedWebTxRaw(t, r, "AMORT", vetoAcctA, vetoDay1, "CHF", -321987.65, "Maturity",
		`{"Description1":"Maturity","Description2":"`+synMortgageStamp+`"}`)
	// A payment on the same account that names no mortgage must not
	// acquire one.
	seedWebTxRaw(t, r, "GROCERIES", vetoAcctA, vetoDay1, "CHF", -12.34, "DIRECT DEBIT",
		`{"Description1":"Direct debit","Description2":"EBILL-RECHNUNG 12345678-9 01/02"}`)

	payloads := emittedPayloads(t, r)
	if !strings.Contains(payloads["AMORT@"+vetoAcctA], `"counter_account":"`+synMortgageID+`"`) {
		t.Errorf("the mortgage payment did not reach gold naming its mortgage: %s",
			payloads["AMORT@"+vetoAcctA])
	}
	if strings.Contains(payloads["GROCERIES@"+vetoAcctA], "counter_account") {
		t.Errorf("a payment naming no mortgage acquired a counter account: %s",
			payloads["GROCERIES@"+vetoAcctA])
	}
}

// TestTheStatementEraResolvesItsMortgageStampToo is the same fact
// arriving by the other door, and the reason it went unnoticed for so
// long: the statement parser FILLS the counter account, so the field
// looked answered. What it fills it with is the stamp's own text — the
// mortgage's name — where every consumer joins on the id silver holds
// it under, so the field resolved to nothing at all.
//
// It matters more than a missing name. The cashflow split reads a
// mortgage's own outstanding balance to tell interest from principal,
// and it can only find that balance through the account the row names;
// a row naming the mortgage in prose reaches it as a payment to nobody
// and keeps its whole amount as interest.
func TestTheStatementEraResolvesItsMortgageStampToo(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	if _, err := r.db.Exec(`
        CREATE TABLE mortgages (snapshot_at INTEGER, account_external_id TEXT,
            banking_relationship_id TEXT, portfolio_external_id TEXT,
            currency_iso TEXT, description TEXT, payload TEXT);
        INSERT INTO mortgages (snapshot_at, account_external_id, currency_iso, payload)
        VALUES (1000, ?, 'CHF', '{}')`, synMortgageID); err != nil {
		t.Fatalf("seed mortgages: %v", err)
	}
	// The statement era as it writes one: the parser's own
	// counter_account field, carrying the stamp text rather than an id.
	seedWebTxRaw(t, r, statementIDPrefix+"AMORT", vetoAcctA, vetoDay1, "CHF", -321987.65, "MATURITY",
		`{"source":"account_statement_pdf","booking_type":"MATURITY",`+
			`"counter_account":"`+synMortgageStamp+`","continuation":["`+synMortgageStamp+`"]}`)
	// A stated IBAN is not a mortgage stamp and must survive untouched:
	// the index is asked only about text shaped like a stamp.
	seedWebTxRaw(t, r, statementIDPrefix+"WIRE", vetoAcctA, vetoDay1, "CHF", -50.00, "E-BANKING PAYMENT ORDER",
		`{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER",`+
			`"counter_account":"`+vetoAcctB+`"}`)

	payloads := emittedPayloads(t, r)
	if !strings.Contains(payloads[statementIDPrefix+"AMORT@"+vetoAcctA],
		`"counter_account":"`+synMortgageID+`"`) {
		t.Errorf("the statement-era payment still names its mortgage in prose: %s",
			payloads[statementIDPrefix+"AMORT@"+vetoAcctA])
	}
	if !strings.Contains(payloads[statementIDPrefix+"WIRE@"+vetoAcctA],
		`"counter_account":"`+vetoAcctB+`"`) {
		t.Errorf("a stated IBAN was rewritten: %s",
			payloads[statementIDPrefix+"WIRE@"+vetoAcctA])
	}
}
