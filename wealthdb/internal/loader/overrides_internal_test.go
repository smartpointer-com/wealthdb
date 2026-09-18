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
//
// And pins what the link takes AWAY. A trade's own taxonomy pair is
// the adapter's answer for a row nothing could name; once config names
// the instrument, that answer is the worse of the two and would mask
// the dimension's, which the cash flow statement reads in preference.
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
		// A token the list does not carry: keeps the coarse pair its
		// feed could state, having nothing better to fall back on.
		{TransactionExternalID: "t5", InstrumentHint: "9999999",
			AssetClass: canonical.AssetClassMetal, Vehicle: canonical.VehiclePhysical},
		// A linked row hands the question back to the instrument.
		{TransactionExternalID: "t6", InstrumentHint: "7654321",
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund},
	}
	applyTransactionInstruments(txns, map[string]string{
		"1234567":               "EXAMPLE0009",
		"Example Fund, Renamed": "EXAMPLE0010",
		"7654321":               "EXAMPLE0012",
		// A token nothing states any more: a no-op, not an error.
		"0000001": "EXAMPLE0011",
	})
	for _, tc := range []struct {
		id, instrument string
		class          canonical.AssetClass
		vehicle        canonical.Vehicle
	}{
		{"t1", "EXAMPLE0009", "", ""},
		{"t2", "EXAMPLE0010", "", ""},
		{"t3", "EXAMPLE0001", "", ""},
		{"t4", "", "", ""},
		{"t5", "", canonical.AssetClassMetal, canonical.VehiclePhysical},
		{"t6", "EXAMPLE0012", "", ""},
	} {
		var got canonical.TransactionChange
		for _, x := range txns {
			if x.TransactionExternalID == tc.id {
				got = x
			}
		}
		id := ""
		if got.InstrumentExternalID != nil {
			id = *got.InstrumentExternalID
		}
		if id != tc.instrument {
			t.Errorf("%s linked to %q, want %q", tc.id, id, tc.instrument)
		}
		if got.AssetClass != tc.class || got.Vehicle != tc.vehicle {
			t.Errorf("%s kept (%q, %q), want (%q, %q)",
				tc.id, got.AssetClass, got.Vehicle, tc.class, tc.vehicle)
		}
	}
}

// TestTaxableWrapperMovesOnlyTheGenericTaxableAnswer pins the blanket
// rule and, more importantly, what it must not touch. An adapter says
// `taxable_personal` because a bank feed states what a product is and
// never who holds it; every other wrapper is something it had positive
// evidence for, and a source-wide statement about joint ownership has
// no business overruling that.
func TestTaxableWrapperMovesOnlyTheGenericTaxableAnswer(t *testing.T) {
	w := func(s canonical.TaxWrapper) *canonical.TaxWrapper { return &s }
	accounts := []canonical.AccountChange{
		{AccountExternalID: "a1", TaxWrapper: w(canonical.TaxWrapperTaxablePersonal)},
		{AccountExternalID: "a2", TaxWrapper: w(canonical.TaxWrapperRothIRA)},
		{AccountExternalID: "a3", TaxWrapper: w(canonical.TaxWrapperTrustNonGrantor)},
		{AccountExternalID: "a4", TaxWrapper: w(canonical.TaxWrapperCustodialUTMA)},
		// No wrapper at all: absent is not the same as taxable, and
		// guessing here is the error the coverage canary reports.
		{AccountExternalID: "a5"},
	}
	applyTaxableWrapper(accounts, string(canonical.TaxWrapperTaxableJoint))
	want := map[string]canonical.TaxWrapper{
		"a1": canonical.TaxWrapperTaxableJoint,
		"a2": canonical.TaxWrapperRothIRA,
		"a3": canonical.TaxWrapperTrustNonGrantor,
		"a4": canonical.TaxWrapperCustodialUTMA,
		"a5": "",
	}
	for _, a := range accounts {
		got := canonical.TaxWrapper("")
		if a.TaxWrapper != nil {
			got = *a.TaxWrapper
		}
		if got != want[a.AccountExternalID] {
			t.Errorf("%s = %q, want %q", a.AccountExternalID, got, want[a.AccountExternalID])
		}
	}
	// Unset is a no-op, not a wipe.
	before := *accounts[0].TaxWrapper
	applyTaxableWrapper(accounts, "")
	if *accounts[0].TaxWrapper != before {
		t.Errorf("an empty rule changed %q", *accounts[0].TaxWrapper)
	}
}
