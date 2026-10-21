package canonical

import (
	"slices"
	"testing"
)

func TestFrequencyOfWord(t *testing.T) {
	t.Parallel()
	for word, want := range map[string]int{"SEMIANNUALLY": 2, "SEMI-ANNUAL": 2, "Semi Annually": 2, "QUARTERLY": 4,
		"monthly": 12, "ANNUALLY": 1, "ANNUAL": 1, " semi  annual ": 2} {
		if got, ok := frequencyOfWord(word); !ok || got != want {
			t.Errorf("frequencyOfWord(%q) = %d, %v; want %d", word, got, ok, want)
		}
	}
	// AT MATURITY names no frequency: a zero pays no coupon, and a
	// frequency of 0 would make it a bill.
	for _, word := range []string{"", "WEEKLY", "SEMI", "APPROP", "AT MATURITY"} {
		if got, ok := frequencyOfWord(word); ok {
			t.Errorf("frequencyOfWord(%q) = %d, want none", word, got)
		}
	}
}

func TestCouponKindOfWord(t *testing.T) {
	t.Parallel()
	for word, want := range map[string]CouponKind{"FIXED": CouponFixed, "Step": CouponFixed,
		"FLOATING": CouponFloating, "VARIABLE": CouponFloating, "VAR": CouponFloating, "ZERO": CouponZero,
		"FRN": CouponFloating, "TIPS": CouponInflationLinked, "inflation_linked": CouponInflationLinked,
		"fixed": CouponFixed} {
		if got, ok := couponKindOfWord(word); !ok || got != want {
			t.Errorf("couponKindOfWord(%q) = %q, %v; want %q", word, got, ok, want)
		}
	}
	for _, word := range []string{"", "COUPON", "TAXABLE"} {
		if got, ok := couponKindOfWord(word); ok {
			t.Errorf("couponKindOfWord(%q) = %q, want none", word, got)
		}
	}
}

func TestReadStatedDateAndPercent(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]string{"2031-06-15": "2031-06-15", "06/15/2031": "2031-06-15",
		" 06/15/31 ": "2031-06-15", "15.06.2031": "2031-06-15"} {
		if got, ok := readStatedDate(s); !ok || got.Format("2006-01-02") != want {
			t.Errorf("readStatedDate(%q) = %v, %v; want %s", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "JUN 30, DEC 30", "02/30/2031", "2031"} {
		if got, ok := readStatedDate(s); ok {
			t.Errorf("readStatedDate(%q) = %v, want none", s, got)
		}
	}
	for s, want := range map[string]float64{"4.25": 4.25, "3.00000%": 3, " 0 ": 0, "-0.5%": -0.5} {
		if got, ok := readStatedPercent(s); !ok || got != want {
			t.Errorf("readStatedPercent(%q) = %v, %v; want %v", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "%", "NaN", "n/a"} {
		if got, ok := readStatedPercent(s); ok {
			t.Errorf("readStatedPercent(%q) = %v, want none", s, got)
		}
	}
}

func TestRatingOfGrades(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		grades      []string
		want        RatingBucket
		wantDefault bool
	}{
		{[]string{"Aa2", "AA-"}, RatingAA, false},
		// The worst grade wins.
		{[]string{"A2", "BBB+", "A"}, RatingBBB, false},
		// An agency that rates nothing yields to one that does.
		{[]string{"NR", "A+"}, RatingA, false},
		{[]string{"NR", ""}, RatingUnrated, false},
		{[]string{"WR", "AA-"}, RatingAA, false},
		// Text that is no grade counts for nothing.
		{[]string{"No rating av.", "", "A-1+"}, "", false},
		{[]string{"Baa1", "SD"}, RatingB, true},
		{nil, "", false},
	} {
		got, def := ratingOfGrades(c.grades...)
		if got != c.want || def != c.wantDefault {
			t.Errorf("ratingOfGrades(%q) = %q, %v; want %q, %v", c.grades, got, def, c.want, c.wantDefault)
		}
	}
}

// The payloads are invented, in the shapes the statements print.
func TestReadPayloadTerms(t *testing.T) {
	t.Parallel()
	got := ReadPayloadTerms(map[string]string{PayloadInterestRate: "3.5", PayloadMaturityDate: "06/30/2027",
		PayloadCouponKind: "FIXED", PayloadCouponFrequency: "SEMI-ANNUAL", PayloadCallDate: "06/30/25",
		PayloadRatingMoodys: "Aa2", PayloadRatingSP: "A+", "unrelated": "BBB"})
	if !sameFloat(got.Coupon, pct(3.5)) || !sameTime(got.Maturity, day(2027, 6, 30)) ||
		!sameTime(got.CallDate, day(2025, 6, 30)) || got.Frequency != 2 ||
		got.CouponKind != CouponFixed || got.Rating != RatingA || got.Default {
		t.Errorf("full payload: %+v", got)
	}
	// A zero states a coupon of 0 and no frequency, and a grade under the
	// source's own key counts with the agencies'.
	got = ReadPayloadTerms(map[string]string{PayloadCouponKind: "ZERO", PayloadCouponFrequency: "AT MATURITY",
		PayloadRatingFitch: "AAA"}, "D")
	if !sameFloat(got.Coupon, pct(0)) || got.Frequency != 0 ||
		got.CouponKind != CouponZero || got.Rating != RatingB || !got.Default {
		t.Errorf("zero in default: %+v", got)
	}
	// What it cannot read states nothing; an empty payload states nothing.
	for _, stated := range []map[string]string{
		{PayloadInterestRate: "n/a", PayloadCouponFrequency: "WEEKLY", PayloadCouponKind: "TAXABLE",
			PayloadCallDate: "CONTINUOUSLY CALLABLE", PayloadRatingSP: "No rating av."},
		nil,
	} {
		if got := ReadPayloadTerms(stated); got != (PayloadTerms{}) {
			t.Errorf("ReadPayloadTerms(%v) = %+v, want nothing", stated, got)
		}
	}
	if keys := PayloadTermsKeys(); !slices.Contains(keys, PayloadRatingSP) || len(keys) != 8 {
		t.Errorf("PayloadTermsKeys() = %v", keys)
	}
}
