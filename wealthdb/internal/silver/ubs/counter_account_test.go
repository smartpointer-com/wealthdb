package ubs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The counter account, and the one thing the EXPORT era never said out
// loud. The statement era carries it in a field of its own; the export
// era writes the same fact into free text, and until it was read the
// only evidence of an own-account move on those rows was a narrative
// rule's guess.
//
// Every value below is synthetic — IBAN-shaped placeholder letters, not
// an account this or any deployment holds (CLAUDE.md §4).

// csvPayload builds an export-era payload: no `counter_account` field,
// the counter IBAN buried in Description3 the way the CSV writes it.
func csvPayload(t *testing.T, description3 string) webTxPayload {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"Description1": "SPECIAL PAYMENT ORDER",
		"Description3": description3,
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

// TestTheExportNarrativeYieldsItsCounterAccount walks the shapes the CSV
// actually writes. The IBAN is taken and nothing else: the purpose names
// what the money was for and the payee names a person, and neither is
// compared against an account id.
func TestTheExportNarrativeYieldsItsCounterAccount(t *testing.T) {
	const want = "CH0000000000000000CCC"
	for _, tc := range []struct {
		name, narrative, want string
	}{
		{"the three-part export shape",
			"Reason for payment: Funding; Account no. IBAN: CH00 0000 0000 0000 00CC C; Transaction no. 9930036ED0000001",
			want},
		{"spaces stripped, case raised",
			"Account no. IBAN: ch00 0000 0000 0000 00cc c;",
			want},
		{"the IBAN ends the narrative",
			"Reason for payment: Funding; Account no. IBAN: CH00 0000 0000 0000 00CC C",
			want},
		{"a narrative naming no account",
			"Reason for payment: Groceries; Transaction no. 9930036ED0000002", ""},
		{"an empty narrative", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := counterAccountFromNarrative(csvPayload(t, tc.narrative)); got != tc.want {
				t.Errorf("counter account = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAStatedCounterAccountIsNotOverwritten pins the precedence between
// the two eras' carriers. The statement parser's field is the parser's
// answer; a value derived from free text must never replace it.
func TestAStatedCounterAccountIsNotOverwritten(t *testing.T) {
	const stated = `{"counter_account":"CH0000000000000000DDD","Description3":"Account no. IBAN: CH0000000000000000CCC;"}`
	got := string(withCounterAccount(json.RawMessage(stated), "CH0000000000000000CCC"))
	if !strings.Contains(got, "CH0000000000000000DDD") {
		t.Fatalf("the stated counter account was lost: %s", got)
	}
	if strings.Count(got, "counter_account") != 1 {
		t.Errorf("payload carries the key twice: %s", got)
	}
}

// TestTheDerivedCounterAccountReachesThePayload is what makes the fact
// readable downstream: gold keeps the payload verbatim, so a consumer
// reads ONE key whichever feed produced the row.
func TestTheDerivedCounterAccountReachesThePayload(t *testing.T) {
	const iban = "CH0000000000000000CCC"
	for _, tc := range []struct{ name, payload string }{
		{"an ordinary object", `{"Description1":"SPECIAL PAYMENT ORDER"}`},
		{"an empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			if err := json.Unmarshal(withCounterAccount(json.RawMessage(tc.payload), iban), &out); err != nil {
				t.Fatalf("the stamped payload is not an object: %v", err)
			}
			if out["counter_account"] != iban {
				t.Errorf("counter_account = %v, want %q", out["counter_account"], iban)
			}
		})
	}
	// Nothing to stamp leaves the payload byte-identical: a row that
	// names no counter account must not be rewritten at all.
	if got := string(withCounterAccount(json.RawMessage(`{"a":1}`), "")); got != `{"a":1}` {
		t.Errorf("an empty counter account rewrote the payload: %s", got)
	}
	// A payload that is not an object has nowhere to put the key, and
	// silently producing malformed JSON would be worse than not trying.
	if got := string(withCounterAccount(json.RawMessage(`not json`), iban)); got != `not json` {
		t.Errorf("a non-object payload was rewritten: %s", got)
	}
}

// emittedPayloads runs the web transaction stream and maps
// TransactionExternalID → the payload gold would keep.
func emittedPayloads(t *testing.T, r *webReader) map[string]string {
	t.Helper()
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, nil, nil)
	if err != nil {
		t.Fatalf("transactionsBeforePSNStart: %v", err)
	}
	out := map[string]string{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			out[tx.TransactionExternalID] = string(tx.Payload)
		}
		if !more {
			break
		}
	}
	return out
}

// TestAnExportRowNamingAnOwnAccountIsInternal is the returns half of the
// same fact. pdfCashIsExternal has always demoted a row whose counter
// account the relationship owns — it is the whole reason the own-IBAN set
// is built — but it runs on the STATEMENT era only. An export-era wire
// between two of the holder's own accounts therefore counted as owner
// capital leaving the bank, which is the conduit error that model exists
// to prevent, arriving by the one door it was not watching.
func TestAnExportRowNamingAnOwnAccountIsInternal(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	// Export-era rows: no `source` marker, the counter account stated in
	// the narrative the way the CSV writes it.
	seedWebTxRaw(t, r, "OWN", vetoAcctA, vetoDay1, "CHF", -5000, "SPECIAL PAYMENT ORDER",
		`{"Description3":"Reason for payment: Funding; Account no. IBAN: `+vetoAcctB+`; Transaction no. 1"}`)
	seedWebTxRaw(t, r, "THIRD-PARTY", vetoAcctA, vetoDay1, "CHF", -700, "E-BANKING PAYMENT ORDER",
		`{"Description3":"Reason for payment: Invoice; Account no. IBAN: CH0000000000000000CCC; Transaction no. 2"}`)

	internal := emittedInternal(t, r, nil)
	if !internal["OWN@"+vetoAcctA] {
		t.Error("a wire to the relationship's OWN account counted as owner capital leaving the bank")
	}
	// Demote-only: a counter account the relationship does not own tells
	// us nothing, and must not move the row either way.
	if internal["THIRD-PARTY@"+vetoAcctA] {
		t.Error("a payment to a third party was demoted; the rule may only demote on a KNOWN own counter")
	}

	// And the fact itself reaches gold, so the enrichment pass can read
	// where the money went without re-parsing prose.
	payloads := emittedPayloads(t, r)
	if !strings.Contains(payloads["OWN@"+vetoAcctA], `"counter_account":"`+vetoAcctB+`"`) {
		t.Errorf("the derived counter account did not reach the payload: %s", payloads["OWN@"+vetoAcctA])
	}
}

// seedExportEraDebit inserts a withdrawal the way the CSV EXPORT writes
// one: the Debit cell already signed, which is the convention the offset
// veto used to read as a credit.
func seedExportEraDebit(t *testing.T, r *webReader, txID, acct string, day int64, ccy string, magnitude float64) {
	t.Helper()
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, amount_credit,
            description_kind, payload)
        VALUES (?, ?, 1000, ?, ?, ?, NULL, 'SPECIAL PAYMENT ORDER', '{}')`,
		txID, acct, day, ccy, -magnitude); err != nil {
		t.Fatalf("seed export-era debit: %v", err)
	}
}

// TestTheOffsetVetoPairsAcrossTheTwoEraConventions is the regression the
// era fold's own rule already named: "only the projection resolves" the
// two eras' column conventions.
//
// The veto bucketed legs on the raw column difference. The export signs
// its Debit cell and the statement prints the figure as printed, so an
// export withdrawal came out POSITIVE and was filed beside the deposits,
// where the only legs it could mirror were other credits. The effect was
// invisible within one era and total across two: an export leg could
// never veto against a statement or a PSN one, which is most of what the
// probe exists for.
func TestTheOffsetVetoPairsAcrossTheTwoEraConventions(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	// One movement, recorded by two eras on the holder's two accounts:
	// the export's signed debit out of A, the statement's printed credit
	// into B.
	seedExportEraDebit(t, r, "EXPORT-OUT", vetoAcctA, vetoDay1, "CHF", 5000)
	seedWebTxRaw(t, r, statementIDPrefix+"IN", vetoAcctB, vetoDay1, "CHF", 5000, "CREDIT",
		`{"source":"account_statement_pdf","booking_type":"CREDIT","internal_transfer":false}`)

	internal := emittedInternal(t, r, nil)
	if !internal["EXPORT-OUT@"+vetoAcctA] {
		t.Error("the export-era leg did not veto: a signed Debit cell is still a debit")
	}
	if !internal[statementIDPrefix+"IN@"+vetoAcctB] {
		t.Error("the statement-era leg did not veto; a pair drops on BOTH sides or neither")
	}
}
