package ubs

import (
	"encoding/json"
	"strings"
	"testing"
)

// The bank's own reference, made readable downstream.
//
// UBS stamps ONE "Transaction no." on both sides of a move between two
// accounts of one relationship — the fact the ubs-web silver schema makes its
// transactions primary key compound to accommodate. On the web feed that
// number IS the silver row's id, but gold's id for the row is the per-leg
// composition, so the number travels as a payload field instead: one key,
// whichever feed wrote the row, which is the same bargain the counter account
// strikes.

// TestTheWebTransactionNumberIsTakenAsTheBankReference pins which web ids
// name a bank reference and which name nothing. A statement-era id is the
// collector's own content hash of a printed row, and stamping one would claim
// an identity no bank ever asserted.
func TestTheWebTransactionNumberIsTakenAsTheBankReference(t *testing.T) {
	for _, tc := range []struct{ name, txID, want string }{
		{"a machine-readable feed's id is the bank's number", "9930036ED0000002", "9930036ED0000002"},
		{"a statement reconstruction names no reference", statementIDPrefix + "abc123", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := webBankRef(tc.txID); got != tc.want {
				t.Errorf("webBankRef(%q) = %q, want %q", tc.txID, got, tc.want)
			}
		})
	}
}

// TestTheBankReferenceReachesThePayload is what makes the fact readable at
// all: gold keeps the payload verbatim, so a row that carried its reference
// only in its id now carries it in a field every source's reader can ask for.
func TestTheBankReferenceReachesThePayload(t *testing.T) {
	const ref = "9930036ED0000002"
	for _, tc := range []struct{ name, payload string }{
		{"an ordinary object", `{"Description1":"PAYMENT ORDER"}`},
		{"an empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out map[string]any
			if err := json.Unmarshal(withBankRef(json.RawMessage(tc.payload), ref), &out); err != nil {
				t.Fatalf("the stamped payload is not an object: %v", err)
			}
			if out["bank_ref"] != ref {
				t.Errorf("bank_ref = %v, want %q", out["bank_ref"], ref)
			}
		})
	}
	// A row with no reference to state is left byte-identical, which is what
	// a statement reconstruction reaches this call as.
	if got := string(withBankRef(json.RawMessage(`{"a":1}`), "")); got != `{"a":1}` {
		t.Errorf("an empty reference rewrote the payload: %s", got)
	}
	// The MT940 feed writes the key itself, and a derived value must never
	// displace a parsed one.
	stated := `{"bank_ref":"PARSED","amount":1.0}`
	if got := string(withBankRef(json.RawMessage(stated), "DERIVED")); !strings.Contains(got, "PARSED") ||
		strings.Count(got, "bank_ref") != 1 {
		t.Errorf("the feed's own reference was displaced: %s", got)
	}
}

// TestASplicedValueIsEncodedRatherThanQuoted keeps the splice from being a
// way to corrupt a payload. The shortcut this file takes — write the field in
// after the opening brace instead of decoding and re-marshalling, so the
// other keys keep their order and spacing — costs nothing until a value
// carries a character JSON gives a meaning to.
func TestASplicedValueIsEncodedRatherThanQuoted(t *testing.T) {
	var out map[string]any
	const awkward = `RE"F\1`
	if err := json.Unmarshal(spliceStringField(`{"a":1}`, bankRefKey, awkward), &out); err != nil {
		t.Fatalf("a value carrying a quote produced invalid JSON: %v", err)
	}
	if out["bank_ref"] != awkward {
		t.Errorf("bank_ref = %v, want %q", out["bank_ref"], awkward)
	}
	if out["a"] == nil {
		t.Error("the splice dropped the keys that were already there")
	}
}
