package canonical

import (
	"slices"
	"strings"
	"testing"
)

func TestTenorMonths(t *testing.T) {
	t.Parallel()
	for tenor, want := range map[string]int{"1M": 1, "3M": 3, "6M": 6, "12M": 12, "1Y": 12, "2Y": 24, "10Y": 120,
		"30Y": 360, "50Y": 600, "600M": 600} {
		if got, err := TenorMonths(tenor); err != nil || got != want {
			t.Errorf("TenorMonths(%q) = %d, %v; want %d", tenor, got, err, want)
		}
	}
	for tenor, want := range map[string]string{
		"": "whole number", "M": "whole number", "3": "whole number", "0M": "whole number", "03M": "whole number",
		"3m": "whole number", "3W": "whole number", "1.5Y": "whole number", "-1Y": "whole number", " 3M": "whole number",
		"51Y": "at most 50 years", "601M": "at most 50 years", "999Y": "at most 50 years",
		"1000M": "at most 50 years", "99999999999999999999Y": "at most 50 years",
	} {
		if _, err := TenorMonths(tenor); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("TenorMonths(%q): err %v, want %q", tenor, err, want)
		}
	}
}

// A rating is one bucket, both its ends, or a range from its better end
// to its worse.
func TestParseRating(t *testing.T) {
	t.Parallel()
	for s, want := range map[string][2]RatingBucket{
		"AAA": {RatingAAA, RatingAAA}, "B": {RatingB, RatingB}, "AAA-A": {RatingAAA, RatingA},
		"BBB-B": {RatingBBB, RatingB}, "AA-AA": {RatingAA, RatingAA},
	} {
		if hi, lo, err := ParseRating(s); err != nil || hi != want[0] || lo != want[1] {
			t.Errorf("ParseRating(%q) = %s, %s, %v; want %s, %s", s, hi, lo, err, want[0], want[1])
		}
	}
	for s, want := range map[string]string{
		"A-AAA": "from its better end to its worse: AAA-A", "B-BB": "worse: BB-B",
		"aaa": "want a rating bucket (AAA, AA, A, BBB, BB, B)", "CCC": "want a rating bucket",
		"AA+": "want a rating bucket", "": "want a rating bucket", "AAA-": "want a rating bucket",
		"-A": "want a rating bucket", "AAA-A-B": "want a rating bucket", "AAA - A": "want a rating bucket",
	} {
		if _, _, err := ParseRating(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRating(%q): err %v, want %q", s, err, want)
		}
	}
}

// The built-in cycles give each of the eleven broad sectors one cycle,
// and no other sector any.
func TestBuiltinCycles(t *testing.T) {
	t.Parallel()
	got := BuiltinCycles()
	if len(got) != len(equitySectors) {
		t.Fatalf("cycles = %+v, want the eleven broad sectors", got)
	}
	want := map[Cycle][]Sector{
		CycleCyclical:  {SectorMaterials, SectorConsumerCyclical, SectorFinancials, SectorRealEstate},
		CycleDefensive: {SectorConsumerStaples, SectorHealthCare, SectorUtilities},
		CycleSensitive: {SectorCommunication, SectorEnergy, SectorIndustrials, SectorTechnology},
	}
	for i, c := range got {
		if c.Sector != equitySectors[i] || c.Origin != OriginBuiltin || !slices.Contains(want[c.Cycle], c.Sector) {
			t.Errorf("cycle %d = %+v", i, c)
		}
		if cy, ok := BuiltinCycle(c.Sector); !ok || cy != c.Cycle {
			t.Errorf("BuiltinCycle(%s) = %s, %v", c.Sector, cy, ok)
		}
	}
	for _, s := range []Sector{SectorGovernment, SectorCash, "tech"} {
		if _, ok := BuiltinCycle(s); ok {
			t.Errorf("%s has a cycle", s)
		}
	}
	if !CycleDefensive.Valid() || Cycle("growth").Valid() || len(Cycles()) != 3 {
		t.Errorf("cycles = %v", Cycles())
	}
}

// Every built-in convention names a market of the config's shape, once,
// in order, with a frequency and day count a config entry may take.
func TestBuiltinConventions(t *testing.T) {
	t.Parallel()
	got := BuiltinConventions()
	markets := make([]string, len(got))
	for i, c := range got {
		markets[i] = c.Market
		ccy, seg, hasSeg := strings.Cut(c.Market, ":")
		if len(ccy) != 3 || (hasSeg && !slices.Contains(MarketSegments(), seg)) || !ValidCouponFrequency(c.Frequency) ||
			!c.DayCount.Valid() || c.Origin != OriginBuiltin {
			t.Errorf("convention %+v", c)
		}
	}
	if want := []string{"CHF", "EUR", "USD", "USD:bill", "USD:government"}; !slices.Equal(markets, want) {
		t.Errorf("markets = %v, want %v", markets, want)
	}
	if got[3].Frequency != 0 || got[3].DayCount != DayCountAct360 || got[4].DayCount != DayCountActAct {
		t.Errorf("US bills and Treasuries = %+v, %+v", got[3], got[4])
	}
	got[0].Frequency = 4
	if BuiltinConventions()[0].Frequency != 1 {
		t.Error("BuiltinConventions hands out its own table")
	}
	if want := []string{"government", "municipal", "corporate", "securitised", "bill"}; !slices.Equal(MarketSegments(), want) {
		t.Errorf("segments = %v, want %v", MarketSegments(), want)
	}
	for _, n := range []int{3, 6, -1} {
		if ValidCouponFrequency(n) {
			t.Errorf("frequency %d is valid", n)
		}
	}
	if DayCount("30/365").Valid() || DayCount("act/act").Valid() {
		t.Error("a day count outside DayCounts is valid")
	}
}
