package ubs

import (
	"sort"
	"testing"
)

// The veto's cross-currency phase.
//
// The other two phases bucket by (day, currency), so neither can see a
// movement whose two legs are denominated differently. Left alone, the paying
// leg of a conversion between two own accounts is demoted by the external
// gate's conservative default while its receiving twin is promoted to
// external on its arrival booking type — a one-sided demotion, which is a
// phantom arrival of owner capital counted as return.

// convLeg builds a veto leg for these tests: an amount in its own currency,
// and optionally the other leg as the statement describes it.
func convLeg(id, acct, ccy string, amt float64, statedCcy, statedAmt string) offsetLeg {
	return offsetLeg{
		vetoKey: id, txID: id, acct: acct, amt: amt,
		day: 100, ccy: ccy, statedCcy: statedCcy, statedAmt: statedAmt,
	}
}

// vetoed runs the phase and returns the keys it demoted, sorted.
func vetoed(legs []offsetLeg) []string {
	var got []string
	vetoConversions(legs, func(l offsetLeg) { got = append(got, l.vetoKey) })
	sort.Strings(got)
	return got
}

func TestTheVetoDemotesBothLegsOfAConversion(t *testing.T) {
	got := vetoed([]offsetLeg{
		convLeg("pays", "A", "GBP", -1234.50, "", ""),
		convLeg("gets", "B", "USD", 1587.75, "GBP", "1234.50"),
	})
	if len(got) != 2 || got[0] != "gets" || got[1] != "pays" {
		t.Errorf("demoted %v, want both legs", got)
	}
}

// Whichever side the statement had room to write it on, the pair is the same.
func TestTheVetoReadsTheDescriptionFromEitherSide(t *testing.T) {
	got := vetoed([]offsetLeg{
		convLeg("pays", "A", "GBP", -1234.50, "USD", "1587.75"),
		convLeg("gets", "B", "USD", 1587.75, "", ""),
	})
	if len(got) != 2 {
		t.Errorf("demoted %v, want both legs", got)
	}
}

// A demotion is only safe in pairs, so anything the description cannot
// resolve demotes NOTHING. Demoting one side alone fabricates the very flow
// the veto exists to prevent.
func TestTheVetoRefusesWhatTheDescriptionCannotResolve(t *testing.T) {
	for _, tc := range []struct {
		name string
		legs []offsetLeg
	}{
		{"two legs answer the description", []offsetLeg{
			convLeg("gets", "B", "USD", 1587.75, "GBP", "1234.50"),
			convLeg("pays", "A", "GBP", -1234.50, "", ""),
			convLeg("also-pays", "C", "GBP", -1234.50, "", ""),
		}},
		{"two descriptions name one leg", []offsetLeg{
			convLeg("gets", "B", "USD", 1587.75, "GBP", "1234.50"),
			convLeg("also-gets", "C", "USD", 1587.75, "GBP", "1234.50"),
			convLeg("pays", "A", "GBP", -1234.50, "", ""),
		}},
		{"the described leg is on the same account", []offsetLeg{
			convLeg("gets", "A", "USD", 1587.75, "GBP", "1234.50"),
			convLeg("pays", "A", "GBP", -1234.50, "", ""),
		}},
		{"the described leg runs the same way", []offsetLeg{
			convLeg("gets", "B", "USD", -1587.75, "GBP", "1234.50"),
			convLeg("pays", "A", "GBP", -1234.50, "", ""),
		}},
		{"nothing answers the description", []offsetLeg{
			convLeg("gets", "B", "USD", 1587.75, "GBP", "1234.50"),
			convLeg("unrelated", "A", "GBP", -999.99, "", ""),
		}},
		{"no description at all", []offsetLeg{
			convLeg("gets", "B", "USD", 1587.75, "", ""),
			convLeg("pays", "A", "GBP", -1234.50, "", ""),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vetoed(tc.legs); len(got) != 0 {
				t.Errorf("demoted %v, want nothing", got)
			}
		})
	}
}

// The phase reaches across the per-currency buckets the other two phases are
// grouped into, so its verdict must not depend on the order a map happened to
// hand the legs over in.
func TestTheVetoConversionPhaseIsOrderIndependent(t *testing.T) {
	legs := []offsetLeg{
		convLeg("pays", "A", "GBP", -1234.50, "", ""),
		convLeg("gets", "B", "USD", 1587.75, "GBP", "1234.50"),
		convLeg("other-pays", "C", "JPY", -3333333, "", ""),
		convLeg("other-gets", "B", "USD", 22222.22, "JPY", "3333333"),
	}
	want := vetoed(legs)
	if len(want) != 4 {
		t.Fatalf("fixture demoted %v, want all four legs", want)
	}
	for i := range legs {
		rotated := append(append([]offsetLeg{}, legs[i:]...), legs[:i]...)
		got := vetoed(rotated)
		if len(got) != len(want) {
			t.Fatalf("rotation by %d demoted %v, want %v", i, got, want)
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("rotation by %d demoted %v, want %v", i, got, want)
			}
		}
	}
}
