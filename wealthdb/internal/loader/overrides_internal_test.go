package loader

// Internal tests for the config-override patchers: they are unexported,
// and what they must NOT touch is as load-bearing as what they must.

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestTransactionInstrumentsLinksOnlyWhatTheAdapterCouldNot pins the
// config link that closes the tail: it keys on the token the adapter
// looked up and failed on, never second-guesses a row that resolved,
// and no-ops silently on a token nothing states any more — which is
// what an adapter learning to resolve it looks like.
func TestTransactionInstrumentsLinksOnlyWhatTheAdapterCouldNot(t *testing.T) {
	already := "EXAMPLE0001"
	txns := []canonical.TransactionChange{
		{TransactionExternalID: "t1", InstrumentHint: "1234567"},
		{TransactionExternalID: "t2", InstrumentHint: "Example Fund, Renamed"},
		// Already resolved: config closes the tail, it does not
		// overrule the adapter.
		{TransactionExternalID: "t3", InstrumentExternalID: &already, InstrumentHint: "1234567"},
		// States no token at all.
		{TransactionExternalID: "t4"},
		// A token the list does not carry.
		{TransactionExternalID: "t5", InstrumentHint: "9999999"},
	}
	applyTransactionInstruments(txns, map[string]string{
		"1234567":               "EXAMPLE0009",
		"Example Fund, Renamed": "EXAMPLE0010",
		// A token nothing states any more: a no-op, not an error.
		"0000001": "EXAMPLE0011",
	})
	want := map[string]string{
		"t1": "EXAMPLE0009", "t2": "EXAMPLE0010",
		"t3": "EXAMPLE0001", "t4": "", "t5": "",
	}
	for _, x := range txns {
		got := ""
		if x.InstrumentExternalID != nil {
			got = *x.InstrumentExternalID
		}
		if got != want[x.TransactionExternalID] {
			t.Errorf("%s linked to %q, want %q", x.TransactionExternalID, got, want[x.TransactionExternalID])
		}
	}
}
