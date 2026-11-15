package ubs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// showRates is the rates a payload states, one line each.
func showRates(payload []byte) string {
	var out []string
	for _, r := range canonical.ReadStatedRates(payload) {
		s := r.Side
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

// A PSN record's tiers read as the stated-rate keys: per side, each tier
// with a rate under its column's number, one past the first only after a
// capped column; a limit
// outside 0.005 to 1e14 is none; the side's day, the term's end before
// the open one and the day count, case and spaces aside. The rates are
// invented.
func TestPSNRateKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ payload, want string }{
		{`{"CdtIntrstRate1": "0.333", "CdtLmtAmt1": "77777", "CdtIntrstRate2": "0.111",
		   "CdtLmtAmt2": "99999999999999999.99", "CdtIntrstRate3": "0", "CdtLmtAmt3": "0",
		   "CdtIntrstVldFrmDt": "2026-01-01", "DbtIntrstRate1": "7.777", "DbtLmtAmt1": "0", "DbtIntrstRate2": "0",
		   "DbtIntrstVldFrmDt": "2025-07-01", "CalIntrstBasisDesc": "act/360", "EndOfDurtn": "9999-12-31"}`,
			"credit from 2026-01-01 ACT/360 #1 0.333<77777 #2 0.111; debit from 2025-07-01 ACT/360 7.777"},
		// A tier keeps its column's number where one before it has no rate.
		{`{"CdtIntrstRate1": "x", "CdtLmtAmt1": "1000", "CdtIntrstRate2": "0.2", "CdtIntrstRate4": "0.4",
		   "CdtLmtAmt3": "3000"}`, "credit #2 0.2 #4 0.4"},
		{`{"CdtIntrstRate1": "3.333", "CdtLmtAmt1": "0", "CalIntrstBasisDesc": "ACT / 365", "EndOfDurtn": "2027-03-31"}`,
			"credit until 2027-03-31 ACT/365 3.333"},
		{`{"CdtIntrstRate1": " 1.5 ", "CdtLmtAmt1": "1_000", "CalIntrstBasisDesc": "Actual"}`, "credit #1 1.5<1000"},
		{`{"CdtIntrstRate1": "0.5", "CdtIntrstVldFrmDt": "2026-02-30", "EndOfDurtn": "2026-1-5"}`,
			"credit until 2026-01-05 0.5"},
		// What does not read states nothing.
		{`{"CdtIntrstRate1": "n/a", "DbtIntrstRate1": "inf"}`, ""},
		{`{"CdtIntrstRate1": ""}`, ""},
		{`{"BookBalAmt": 10}`, ""},
	} {
		keys := psnRateKeys([]byte(tc.payload))
		payload, err := silver.WithStatedRates(json.RawMessage(`{}`), keys)
		if err != nil {
			t.Fatal(err)
		}
		if got := showRates(payload); got != tc.want {
			t.Errorf("%s:\n %q, want %q", tc.payload, got, tc.want)
		}
	}
}

// psnShaped tells a record that states PSN's tiers, a value or not.
func TestPSNShaped(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]bool{
		`{"CdtIntrstRate1": "0.4"}`: true, `{"CdtIntrstRate1": ""}`: true, `{"CdtIntrstRate1": null}`: false,
		`{"interest_rate": 0.4}`: false, `not json`: false,
	} {
		if got := psnShaped([]byte(payload)); got != want {
			t.Errorf("%s: %v, want %v", payload, got, want)
		}
	}
}
