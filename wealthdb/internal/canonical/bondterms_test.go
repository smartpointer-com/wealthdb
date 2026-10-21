package canonical

import (
	"fmt"
	"testing"
	"time"
)

func day(y, m, d int) *time.Time {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return &t
}

func pct(v float64) *float64 { return &v }

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func sameFloat(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func checkTerms(t *testing.T, what string, got, want BondTerms) {
	t.Helper()
	if !sameFloat(got.Coupon, want.Coupon) || !sameTime(got.Maturity, want.Maturity) ||
		got.Frequency != want.Frequency || got.CouponKind != want.CouponKind || got.Bill != want.Bill ||
		!sameTime(got.CallDate, want.CallDate) || got.MakeWhole != want.MakeWhole || got.Rating != want.Rating ||
		got.Default != want.Default {
		t.Errorf("%s:\n got %s\nwant %s", what, termsText(got), termsText(want))
	}
}

func termsText(b BondTerms) string {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	d := func(p *time.Time) string {
		if p == nil {
			return "-"
		}
		return p.Format(time.DateOnly)
	}
	return fmt.Sprintf("coupon=%s maturity=%s frequency=%d kind=%q bill=%v call=%s make_whole=%v rating=%q default=%v",
		f(b.Coupon), d(b.Maturity), b.Frequency, b.CouponKind, b.Bill, d(b.CallDate), b.MakeWhole, b.Rating, b.Default)
}

// The descriptions are invented, in the layouts brokers write.
func TestReadBondTerms(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		desc string
		want BondTerms
	}{
		// A coupon and the maturity after it, the US way.
		{"EXAMPLE CNTY TEX REV BDS SER 2020 B 4.00000% 06/15/2031",
			BondTerms{Coupon: pct(4), Maturity: day(2031, 6, 15)}},
		// The series year before the coupon is no date.
		{"QLAND ST GO BDS 2019 5% 08/01/32", BondTerms{Coupon: pct(5), Maturity: day(2032, 8, 1)}},
		// The Swiss way: the day first, between dots.
		{"QLAND 1.915% 10.05.2034", BondTerms{Coupon: pct(1.915), Maturity: day(2034, 5, 10)}},
		// A statement's metadata line: the maturity first, the coupon kind,
		// the grades, the frequency and the call schedule.
		{"EXAMPLE CITY TX GO BDS 06/15/31 1,234.56 FIXED COUPON MOODYS Aa2 S&P AA- SEMIANNUALLY " +
			"CALL ON 06/15/2027 100.00 NEXT CALL 06/15/2028 CUSIP: 000000AB1",
			BondTerms{Maturity: day(2031, 6, 15), Frequency: 2, CouponKind: CouponFixed, CallDate: day(2027, 6, 15),
				Rating: RatingAA}},
		// The lower grade wins.
		{"EXAMPLE UTIL REV 07/01/30 FIXED COUPON MOODYS A2 S&P BBB+ ANNUALLY",
			BondTerms{Maturity: day(2030, 7, 1), Frequency: 1, CouponKind: CouponFixed, Rating: RatingBBB}},
		// An agency that rates nothing yields to one that does.
		{"EXAMPLE AUTH 07/01/30 MOODYS NR S&P A", BondTerms{Maturity: day(2030, 7, 1), Rating: RatingA}},
		{"EXAMPLE AUTH 07/01/30 MOODYS WR", BondTerms{Maturity: day(2030, 7, 1), Rating: RatingUnrated}},
		// A make-whole call shortens nothing.
		{"EXAMPLE CORP NOTE CALL MAKE WHOLE 4.65000%", BondTerms{Coupon: pct(4.65), MakeWhole: true}},
		{"EXAMPLE CORP NOTE CALL MAKE WHOLE", BondTerms{MakeWhole: true}},
		// A bill pays no coupon.
		{"EXAMPLE TREAS BILLS ZERO CPN", BondTerms{Coupon: pct(0), CouponKind: CouponZero, Bill: true}},
		// VAR PURP is a municipal bond's various purposes; its coupon is fixed.
		{"QLAND VAR PURP GO BDS 06/15/31 FIXED COUPON", BondTerms{Maturity: day(2031, 6, 15), CouponKind: CouponFixed}},
		{"EXAMPLE HSG AUTH VAR RT DEMAND BDS", BondTerms{CouponKind: CouponFloating}},
		{"EXAMPLE BANK FRN 03/20/2029", BondTerms{Maturity: day(2029, 3, 20), CouponKind: CouponFloating}},
		{"QLAND TIPS 0.125% 01/15/2032", BondTerms{Coupon: pct(0.125), Maturity: day(2032, 1, 15),
			CouponKind: CouponInflationLinked}},
		// No terms at all, and a date that is none.
		{"EXAMPLE MGMT INC DEL NOTE", BondTerms{}},
		{"EXAMPLE CORP 4.5% 13/45/2031", BondTerms{Coupon: pct(4.5)}},
		// CALLABLE says a call exists; it opens no window for a date.
		{"EXAMPLE CORP CALLABLE 4.25% 06/15/2031", BondTerms{Coupon: pct(4.25), Maturity: day(2031, 6, 15)}},
		{"EXAMPLE AGENCY CALLABLE BOND 06/15/2031", BondTerms{Maturity: day(2031, 6, 15)}},
		// A make-whole call after the call's date.
		{"EXAMPLE CORP 5% 03/01/2033 CALL 03/01/2028 MAKE WHOLE", BondTerms{Coupon: pct(5),
			Maturity: day(2033, 3, 1), MakeWhole: true}},
		// Two words, and the grades a comma follows.
		{"EXAMPLE AUTH 07/01/30 MOODYS Baa2, S&P AA- SEMI ANNUALLY", BondTerms{Maturity: day(2030, 7, 1),
			Frequency: 2, Rating: RatingBBB}},
		// ANNUAL APPROP is no frequency; a dated date is no maturity.
		{"QLAND ANNUAL APPROP REV DTD 06/01/2012 08/01/2032", BondTerms{Maturity: day(2032, 8, 1)}},
		// A fraction of a coupon; MOODY'S and FITCH; a grade of default.
		{"EXAMPLE CORP 4 1/2% 12/01/2029 MOODY'S Caa1 FITCH D", BondTerms{Coupon: pct(4.5),
			Maturity: day(2029, 12, 1), Rating: RatingB, Default: true}},
		{"EXAMPLE CORP 6% 12/01/2029 MOODYS (P)Aa2", BondTerms{Coupon: pct(6), Maturity: day(2029, 12, 1),
			Rating: RatingAA}},
	} {
		checkTerms(t, c.desc, ReadBondTerms(c.desc), c.want)
	}
}

func TestReadFigiTicker(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		ticker string
		want   BondTerms
	}{
		{"T 4.125 07/31/31 7Y", BondTerms{Coupon: pct(4.125), Maturity: day(2031, 7, 31)}},
		{"TX EXMP 5 08/15/2034", BondTerms{Coupon: pct(5), Maturity: day(2034, 8, 15)}},
		{"EXMP 4 1/4 05/15/29", BondTerms{Coupon: pct(4.25), Maturity: day(2029, 5, 15)}},
		{"B 0 01/15/27", BondTerms{Coupon: pct(0), Maturity: day(2027, 1, 15), CouponKind: CouponZero, Bill: true}},
		{"EXMP 0 01/15/27", BondTerms{Coupon: pct(0), Maturity: day(2027, 1, 15), CouponKind: CouponZero}},
		{"EXMP FLOAT 01/31/28", BondTerms{Maturity: day(2028, 1, 31), CouponKind: CouponFloating}},
		{"EXMP F 01/31/28", BondTerms{Maturity: day(2028, 1, 31), CouponKind: CouponFloating}},
		{"EXMP V4.565 06/14/30", BondTerms{Coupon: pct(4.565), Maturity: day(2030, 6, 14), CouponKind: CouponFloating}},
		{"TII 0 1/8 01/15/30", BondTerms{Coupon: pct(0.125), Maturity: day(2030, 1, 15),
			CouponKind: CouponInflationLinked}},
		{"EXMP", BondTerms{}},
		{"", BondTerms{}},
	} {
		checkTerms(t, c.ticker, ReadFigiTicker(c.ticker), c.want)
	}
}

func TestRatingOfGrade(t *testing.T) {
	t.Parallel()
	for grade, want := range map[string]RatingBucket{
		"Aaa": RatingAAA, "Aa2": RatingAA, "A3": RatingA, "Baa1": RatingBBB, "Ba2": RatingBB, "B3": RatingB,
		"Caa1": RatingB, "AAA": RatingAAA, "AA+": RatingAA, "A-": RatingA, "BBB": RatingBBB, "BB-": RatingBB,
		"B+": RatingB, "CCC": RatingB, "D": RatingB, "NR": RatingUnrated, "WR": RatingUnrated, "Aa1*": RatingAA,
		"(P)Aa2": RatingAA, "WD": RatingUnrated, "DDD": RatingB, "DD": RatingB,
	} {
		if got, ok := RatingOfGrade(grade); !ok || got != want {
			t.Errorf("RatingOfGrade(%q) = %q, %v; want %q", grade, got, ok, want)
		}
	}
	for _, grade := range []string{"P-1", "A-1+", "SP-1", "", "GOOD"} {
		if got, ok := RatingOfGrade(grade); ok {
			t.Errorf("RatingOfGrade(%q) = %q; want no grade", grade, got)
		}
	}
	for grade, want := range map[string]bool{"D": true, "SD": true, "RD": true, "DDD": true, "B-": false, "CCC": false} {
		if GradeInDefault(grade) != want {
			t.Errorf("GradeInDefault(%q) = %v", grade, !want)
		}
	}
}
