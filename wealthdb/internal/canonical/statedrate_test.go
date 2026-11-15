package canonical

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
			if r.TermEnd != nil {
				s += " until " + r.TermEnd.Format("2006-01-02")
			}
			if r.DayCount != "" {
				s += " " + string(r.DayCount)
			}
			for _, t := range r.Tiers {
				if t.Number != 0 {
					s += fmt.Sprintf(" #%d", t.Number)
				}
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
		{`{"interest_rate": 3, "interest_term_end": "2027-03-31", "interest_day_count": "ACT/365"}`,
			"credit interest_rate until 2027-03-31 ACT/365 3"},
		{`{"interest_rate": 3, "interest_term_end": null, "interest_day_count": null}`, "credit interest_rate 3"},
		// Numbered tiers may leave a number out, and a limit before the
		// last.
		{`{"interest_tiers": [{"rate": 0.1, "tier": 1}, {"rate": 0.4, "up_to": 4000, "tier": 4}]}`,
			"credit interest_tiers #1 0.1 #4 0.4<4000"},
		{`{"interest_tiers": [{"rate": 0.3, "up_to": null, "tier": 3}]}`, "credit interest_tiers #3 0.3"},
		{`{"interest_conditions": [{"interest_tiers": [{"rate": 0.5, "up_to": 100}, {"rate": 0.1}]}]}`,
			"credit interest_conditions[0].interest_tiers 0.5<100 0.1"},
		// What does not read states nothing.
		{`{"interest_rate": ""}`, ""},
		{`{"interest_rate": 1, "interest_side": "both"}`, ""},
		{`{"interest_rate": 1, "interest_valid_from": "01.01.2026"}`, ""},
		{`{"interest_rate": 1, "interest_term_end": "31.03.2027"}`, ""},
		{`{"interest_rate": 1, "interest_day_count": "act/360"}`, ""},
		{`{"interest_tiers": [{"rate": 1, "tier": 1}, {"rate": 2}]}`, ""},
		{`{"interest_tiers": [{"rate": 1, "tier": 2}, {"rate": 2, "tier": 2}]}`, ""},
		{`{"interest_tiers": [{"rate": 1, "tier": 0}]}`, ""},
		{`{"interest_tiers": [{"rate": 1, "tier": 1.5}]}`, ""},
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
	end := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
	keys = StatedRateKeys(StatedRate{Side: RateSideCredit, Tiers: []StatedTier{{Pct: 3}}, TermEnd: &end,
		DayCount: DayCountAct365})
	if keys[PayloadInterestTermEnd] != "2027-03-31" || keys[PayloadInterestDayCount] != "ACT/365" {
		t.Errorf("a term deposit: %v, want its end and day count", keys)
	}
	keys = StatedRateKeys(StatedRate{Side: RateSideCredit, Tiers: []StatedTier{{Pct: 3, Number: 2}}})
	if tiers, ok := keys[PayloadInterestTiers].([]map[string]any); !ok || tiers[0][StatedTierNumber] != 2 {
		t.Errorf("a second tier alone: %v, want it numbered", keys)
	}
}
