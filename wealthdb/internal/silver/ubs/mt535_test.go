package ubs

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// An amount with ISO 15022's negative sign, N before the currency, reads
// as negative, as the collector reads it; one without stays positive.
func TestParse19AReadsTheNegativeSign(t *testing.T) {
	got := parse19A([]string{":HOLD//NCHF1500,25", ":BOOK//CHF900,"})
	if len(got) != 2 {
		t.Fatalf("parse19A = %+v, want both entries", got)
	}
	if got[0].Qualifier != "HOLD" || got[0].Currency != "CHF" || got[0].Amount.String() != "-1500.25" {
		t.Errorf("negative entry = %+v, want HOLD CHF -1500.25", got[0])
	}
	if got[1].Amount.String() != "900" {
		t.Errorf("positive entry = %+v, want 900", got[1])
	}
}

// A quantity with the negative sign, N before the figure, reads as
// negative; one without stays positive.
func TestParse93BReadsTheNegativeSign(t *testing.T) {
	got := parse93B([]string{":AGGR//UNIT/N100,", ":AVAI//FAMT/2500,5"})
	if len(got) != 2 {
		t.Fatalf("parse93B = %+v, want both entries", got)
	}
	if got[0].Qualifier != "AGGR" || got[0].Format != "UNIT" || got[0].Amount.String() != "-100" {
		t.Errorf("negative entry = %+v, want AGGR UNIT -100", got[0])
	}
	if got[1].Amount.String() != "2500.5" {
		t.Errorf("positive entry = %+v, want 2500.5", got[1])
	}
}

// The corporate-action values read the same sign: an amount with N
// before its currency, and a number of units with N before the figure.
// A currency that itself begins with N is no sign.
func TestSwiftValuesReadTheNegativeSign(t *testing.T) {
	for _, tc := range []struct{ value, ccy, amount string }{
		{":PSTA//NCHF1500,25", "CHF", "-1500.25"},
		{":PSTA//NNOK12,", "NOK", "-12"},
		{":PSTA//NOK12,", "NOK", "12"},
	} {
		ccy, amount, ok := swiftMoney(tc.value)
		if !ok || ccy != tc.ccy || amount.String() != tc.amount {
			t.Errorf("swiftMoney(%q) = %s %s %v, want %s %s", tc.value, ccy, amount.String(), ok, tc.ccy, tc.amount)
		}
	}
	for value, want := range map[string]string{":CONB//UNIT/N40,": "-40", ":CONB//UNIT/40,": "40"} {
		if got := swiftUnits(value); got == nil || got.String() != want {
			t.Errorf("swiftUnits(%q) = %v, want %s", value, got, want)
		}
	}
}
