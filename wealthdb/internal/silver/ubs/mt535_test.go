package ubs

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

func TestParseSwiftDecimal(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"1500000", "1500000"},
		{"150,123456", "150.123456"},
		{"60,839", "60.839"},
		{"0", "0"},
		{"10000", "10000"},
	}
	for _, c := range cases {
		got, err := parseSwiftDecimal(c.in)
		if err != nil {
			t.Errorf("parseSwiftDecimal(%q) err=%v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("parseSwiftDecimal(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestParse19A(t *testing.T) {
	raw := []string{
		":HOLD//USD1500000,",
		":BOOK//USD600000,",
		":HOLD//CHF1200000,",
		":ACRU//USD12,50",
		"garbage subfield ignored",
	}
	got := parse19A(raw)
	if len(got) != 4 {
		t.Fatalf("got %d parsed entries, want 4", len(got))
	}
	if got[0].Qualifier != "HOLD" || got[0].Currency != "USD" || got[0].Amount.String() != "1500000" {
		t.Errorf("[0] = %+v", got[0])
	}
	if got[1].Qualifier != "BOOK" || got[1].Amount.String() != "600000" {
		t.Errorf("[1] = %+v", got[1])
	}
	if got[2].Currency != "CHF" {
		t.Errorf("[2] currency = %q", got[2].Currency)
	}
	if got[3].Amount.String() != "12.5" {
		t.Errorf("[3] amount = %s, want 12.5", got[3].Amount)
	}
}

func TestParse93B(t *testing.T) {
	raw := []string{
		":AGGR//UNIT/10000,",
		":AVAI//UNIT/9500,",
		":AGGR//FAMT/100000,",
		":NAVL//UNIT/0,",
	}
	got := parse93B(raw)
	if len(got) != 4 {
		t.Fatalf("got %d, want 4", len(got))
	}
	wantTuples := []struct {
		qual, format, amount string
	}{
		{"AGGR", "UNIT", "10000"},
		{"AVAI", "UNIT", "9500"},
		{"AGGR", "FAMT", "100000"},
		{"NAVL", "UNIT", "0"},
	}
	for i, want := range wantTuples {
		if got[i].Qualifier != want.qual || got[i].Format != want.format || got[i].Amount.String() != want.amount {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestFindMarketValue(t *testing.T) {
	amounts := []mt535Money{
		{Qualifier: "HOLD", Currency: "USD", Amount: canonical.NewDecimalFromInt(1500)},
		{Qualifier: "BOOK", Currency: "USD", Amount: canonical.NewDecimalFromInt(1200)},
		{Qualifier: "HOLD", Currency: "CHF", Amount: canonical.NewDecimalFromInt(1300)},
	}

	// Currency match wins.
	got, ok := findMarketValue(amounts, "USD")
	if !ok || got.String() != "1500" {
		t.Errorf("match: got=%s ok=%v, want 1500/true", got, ok)
	}
	got, ok = findMarketValue(amounts, "CHF")
	if !ok || got.String() != "1300" {
		t.Errorf("match: got=%s ok=%v, want 1300/true", got, ok)
	}

	// No exact match → fall back to first HOLD.
	got, ok = findMarketValue(amounts, "JPY")
	if !ok || got.String() != "1500" {
		t.Errorf("fallback: got=%s ok=%v, want 1500/true (first HOLD)", got, ok)
	}

	// No HOLD at all → ok=false.
	noHold := []mt535Money{{Qualifier: "BOOK", Currency: "USD", Amount: canonical.NewDecimalFromInt(1200)}}
	_, ok = findMarketValue(noHold, "USD")
	if ok {
		t.Error("no-HOLD should give ok=false")
	}
}

// TestFindHoldEntry covers the (amount, currency, ok) shape used
// by appendHoldings to derive both market_value and the position's
// currency from one chosen 19A:HOLD entry.
func TestFindHoldEntry(t *testing.T) {
	amounts := []mt535Money{
		{Qualifier: "HOLD", Currency: "USD", Amount: canonical.NewDecimalFromInt(1500)},
		{Qualifier: "BOOK", Currency: "USD", Amount: canonical.NewDecimalFromInt(1200)},
		{Qualifier: "HOLD", Currency: "CHF", Amount: canonical.NewDecimalFromInt(1300)},
	}

	// Preferred match wins (amount AND currency).
	amt, ccy, ok := findHoldEntry(amounts, "CHF")
	if !ok || amt.String() != "1300" || ccy != "CHF" {
		t.Errorf("preferred CHF: amt=%s ccy=%s ok=%v, want 1300/CHF/true", amt, ccy, ok)
	}

	// No preference (instrument meta currency unknown): fall back
	// to first HOLD entry — currency comes from that entry.
	amt, ccy, ok = findHoldEntry(amounts, "")
	if !ok || amt.String() != "1500" || ccy != "USD" {
		t.Errorf("no-preference: amt=%s ccy=%s ok=%v, want 1500/USD/true", amt, ccy, ok)
	}

	// No HOLD at all → ok=false, both other fields zero.
	noHold := []mt535Money{{Qualifier: "BOOK", Currency: "USD", Amount: canonical.NewDecimalFromInt(1200)}}
	_, _, ok = findHoldEntry(noHold, "")
	if ok {
		t.Error("no-HOLD should give ok=false")
	}
}

func TestFindQuantity(t *testing.T) {
	// AGGR present → use it
	q, ok := findQuantity([]mt535Qty{
		{Qualifier: "AGGR", Format: "UNIT", Amount: canonical.NewDecimalFromInt(100)},
		{Qualifier: "AVAI", Format: "UNIT", Amount: canonical.NewDecimalFromInt(95)},
	})
	if !ok || q.String() != "100" {
		t.Errorf("AGGR-present: got=%s ok=%v", q, ok)
	}

	// AGGR missing, AVAI present → fall back
	q, ok = findQuantity([]mt535Qty{
		{Qualifier: "AVAI", Format: "UNIT", Amount: canonical.NewDecimalFromInt(95)},
	})
	if !ok || q.String() != "95" {
		t.Errorf("AVAI-fallback: got=%s ok=%v", q, ok)
	}

	// Neither AGGR nor AVAI → ok=false
	_, ok = findQuantity([]mt535Qty{
		{Qualifier: "NAVL", Format: "UNIT", Amount: canonical.NewDecimalFromInt(0)},
	})
	if ok {
		t.Error("no AGGR/AVAI should give ok=false")
	}
}
