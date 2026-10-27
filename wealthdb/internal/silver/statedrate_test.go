package silver

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestWithCreditRate(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"keeps the other keys", `{"basis":"running_balance"}`,
			`{"basis":"running_balance","interest_rate":2.41,"interest_side":"credit"}`},
		{"no payload", ``, `{"interest_rate":2.41,"interest_side":"credit"}`},
		{"a null payload", `null`, `{"interest_rate":2.41,"interest_side":"credit"}`},
		{"replaces a stated rate", `{"interest_rate":9,"interest_side":"debit"}`,
			`{"interest_rate":2.41,"interest_side":"credit"}`},
		{"replaces every stated-rate key", `{"basis":"statement","interest_tiers":[{"rate":1}],` +
			`"interest_conditions":[{"interest_rate":5}],"interest_valid_from":"2020-01-01"}`,
			`{"basis":"statement","interest_rate":2.41,"interest_side":"credit"}`},
		{"keeps the other keys' bytes", `{"id":12345678901234567890,"v":1.10}`,
			`{"id":12345678901234567890,"interest_rate":2.41,"interest_side":"credit","v":1.10}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithCreditRate(json.RawMessage(tc.payload), 2.41, "")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	if got, err := WithCreditRate(nil, 3.5, "2026-01-31"); err != nil ||
		string(got) != `{"interest_rate":3.5,"interest_side":"credit","interest_valid_from":"2026-01-31"}` {
		t.Errorf("a rate with its day: %s %v", got, err)
	}
	if _, err := WithCreditRate(json.RawMessage(`[1]`), 1, ""); err == nil {
		t.Error("a payload that is no object: want an error")
	}
	if _, err := WithCreditRate(nil, math.NaN(), ""); err == nil {
		t.Error("a rate that is no number: want an error")
	}
	if _, err := WithCreditRate(nil, 1, "31.01.2026"); err == nil {
		t.Error("a day that is no date: want an error")
	}
}

func TestWithStatedRatesWritesWhatTheReaderReads(t *testing.T) {
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	limit := 50000.0
	want := []canonical.StatedRate{
		{Side: canonical.RateSideCredit, Tiers: []canonical.StatedTier{{Pct: 0.5}}},
		{Side: canonical.RateSideCredit, ValidFrom: &day,
			Tiers: []canonical.StatedTier{{Pct: 0.75, UpTo: &limit}, {Pct: 0.25}}},
	}
	got, err := WithStatedRates(json.RawMessage(`{"interest_rate":9}`), canonical.StatedRateKeys(want...))
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"interest_conditions":[{"interest_rate":0.5,"interest_side":"credit"},` +
		`{"interest_side":"credit","interest_tiers":[{"rate":0.75,"up_to":50000},{"rate":0.25,"up_to":null}],` +
		`"interest_valid_from":"2026-03-01"}]}`
	if string(got) != wantJSON {
		t.Errorf("got %s, want %s", got, wantJSON)
	}
	read := canonical.ReadStatedRates(got)
	if len(read) != len(want) {
		t.Fatalf("read %d rates, want %d", len(read), len(want))
	}
	for i, r := range read {
		w := want[i]
		if r.Side != w.Side || (r.ValidFrom == nil) != (w.ValidFrom == nil) ||
			(r.ValidFrom != nil && !r.ValidFrom.Equal(*w.ValidFrom)) || len(r.Tiers) != len(w.Tiers) {
			t.Fatalf("rate %d: read %+v, want %+v", i, r, w)
		}
		for j, tr := range r.Tiers {
			wt := w.Tiers[j]
			if tr.Pct != wt.Pct || (tr.UpTo == nil) != (wt.UpTo == nil) || (tr.UpTo != nil && *tr.UpTo != *wt.UpTo) {
				t.Errorf("rate %d tier %d: read %+v, want %+v", i, j, tr, wt)
			}
		}
	}
}
