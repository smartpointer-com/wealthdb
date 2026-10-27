package canonical

import (
	"fmt"
	"strings"
	"testing"
)

func TestReadStatedRates(t *testing.T) {
	t.Parallel()
	show := func(rs []StatedRate) string {
		var out []string
		for _, r := range rs {
			s := r.Side + " " + r.Field
			if r.ValidFrom != nil {
				s += " from " + r.ValidFrom.Format("2006-01-02")
			}
			for _, t := range r.Tiers {
				s += fmt.Sprintf(" %g", t.Pct)
				if t.UpTo != nil {
					s += fmt.Sprintf("<%g", *t.UpTo)
				}
			}
			out = append(out, s)
		}
		return strings.Join(out, "; ")
	}
	for _, tc := range []struct{ payload, want string }{
		{`{"interest_rate": 0.75}`, "credit interest_rate 0.75"},
		{`{"interest_rate": "0.75%", "interest_side": "Debit", "interest_valid_from": "2026-01-01"}`,
			"debit interest_rate from 2026-01-01 0.75"},
		{`{"interest_tiers": [{"rate": 0.5, "up_to": "10000"}, {"rate": 1}], "interest_rate": 9}`,
			"credit interest_tiers 0.5<10000 1"},
		{`{"interest_conditions": [{"interest_rate": 1}, {"interest_rate": 7, "interest_side": "debit"},
		   {"interest_rate": "x"}]}`,
			"credit interest_conditions[0].interest_rate 1; debit interest_conditions[1].interest_rate 7"},
		{`{"interest_conditions": [{"interest_rate": 1}, {"interest_rate": 2, "interest_side": null},
		   {"interest_rate": 3, "interest_valid_from": "2026-09-01"}]}`,
			"credit interest_conditions[0].interest_rate 1; credit interest_conditions[2].interest_rate from 2026-09-01 3"},
		{`{"interest_tiers": null, "interest_rate": 1}`, "credit interest_rate 1"},
		{`{"interest_conditions": [{"interest_tiers": [{"rate": 0.5, "up_to": 100}, {"rate": 0.1}]}]}`,
			"credit interest_conditions[0].interest_tiers 0.5<100 0.1"},
		// What does not read states nothing.
		{`{"interest_rate": ""}`, ""},
		{`{"interest_rate": 1, "interest_side": "both"}`, ""},
		{`{"interest_rate": 1, "interest_valid_from": "01.01.2026"}`, ""},
		{`{"interest_tiers": [{"rate": 1}, {"rate": 2}]}`, ""},
		{`{"interest_tiers": []}`, ""},
		{`{"interest_tiers": [{"rate": 1, "up_to": 0}, {"rate": 2}]}`, ""},
		{`{"interest_tiers": [{"rate": 1, "up_to": "10000%"}, {"rate": 2}]}`, ""},
		{`{"interest_conditions": {"interest_rate": 1}}`, ""},
		{`{"basis": "statement"}`, ""},
		{`not json`, ""},
		{``, ""},
	} {
		if got := show(ReadStatedRates([]byte(tc.payload))); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.payload, got, tc.want)
		}
	}
}

func TestStatedRateKeys(t *testing.T) {
	t.Parallel()
	if keys := StatedRateKeys(); keys != nil {
		t.Errorf("no rate: %v, want no keys", keys)
	}
	limit := 100.0
	keys := StatedRateKeys(StatedRate{Side: RateSideDebit, Tiers: []StatedTier{{Pct: 2, UpTo: &limit}}})
	if keys[PayloadInterestSide] != RateSideDebit || keys[PayloadInterestRate] != nil || keys[PayloadInterestTiers] == nil {
		t.Errorf("one tier with a limit: %v, want its tiers", keys)
	}
}
