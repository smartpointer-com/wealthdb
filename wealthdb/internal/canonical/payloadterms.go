package canonical

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The keys a source's payload states a bond's terms under: its
// instrument's payload, or a holding's. Each value is as the source
// prints it, the coupon a number or a percentage; ReadPayloadTerms
// reads them.
const (
	// PayloadInterestRate is a bond's coupon in percent a year: 4.25 or
	// 4.25%. On cash it is the rate the balance earns or pays
	// (statedrate.go).
	PayloadInterestRate = "interest_rate"
	// PayloadMaturityDate is the maturity: 2031-06-15, 06/15/2031,
	// 06/15/31 or 15.06.2031.
	PayloadMaturityDate = "maturity_date"
	// PayloadCouponKind is the word that names the coupon's kind: FIXED,
	// FLOATING, VARIABLE, ZERO.
	PayloadCouponKind = "coupon_kind"
	// PayloadCouponFrequency is the word that names how often the coupon
	// pays: SEMIANNUALLY, QUARTERLY. AT MATURITY names no frequency: a
	// zero's coupon kind says it pays none.
	PayloadCouponFrequency = "coupon_frequency"
	// PayloadCallDate is the first day the issuer may redeem the bond,
	// written as a maturity is.
	PayloadCallDate = "call_date"
	// PayloadRatingMoodys, PayloadRatingSP and PayloadRatingFitch are each
	// agency's grade: Aa2, AA-, NR.
	PayloadRatingMoodys = "rating_moodys"
	PayloadRatingSP     = "rating_sp"
	PayloadRatingFitch  = "rating_fitch"
)

var payloadTermsKeys = []string{PayloadInterestRate, PayloadMaturityDate, PayloadCouponKind, PayloadCouponFrequency,
	PayloadCallDate, PayloadRatingMoodys, PayloadRatingSP, PayloadRatingFitch}

// PayloadTermsKeys are the keys a payload states a bond's terms under.
func PayloadTermsKeys() []string {
	return slices.Clone(payloadTermsKeys)
}

// PayloadTerms is what a source's payload states of a bond's terms. A
// nil or zero field is one it does not state.
type PayloadTerms struct {
	Coupon             *float64
	Maturity, CallDate *time.Time
	// Frequency is the coupon payments a year, 0 where unstated.
	Frequency  int
	CouponKind CouponKind
	// Rating is the worst of the grades, as a bucket; Default is a grade
	// of default among them.
	Rating  RatingBucket
	Default bool
}

// statedFrequencies are the words that state a coupon frequency on
// their own: the description's (frequencyWords), and those a statement
// prints where ANNUAL can mean nothing else.
var statedFrequencies = func() map[string]int {
	m := maps.Clone(frequencyWords)
	m["ANNUAL"], m["SEMI ANNUAL"], m["SEMI ANNUALLY"] = 1, 2, 2
	return m
}()

// frequencyOfWord is the coupon payments a year a word states, case and
// spacing aside: SEMIANNUALLY or SEMI-ANNUAL 2, QUARTERLY 4, MONTHLY 12,
// ANNUALLY or ANNUAL 1. Any other word states none.
func frequencyOfWord(word string) (int, bool) {
	f, ok := statedFrequencies[strings.Join(strings.Fields(strings.ToUpper(word)), " ")]
	return f, ok
}

// couponKindOfWord is the coupon kind a word states, case aside: a word
// that names it before COUPON (FIXED, STEP, FLOATING, VARIABLE, VAR,
// ZERO), one a description uses alone (FLOAT, FRN, TIPS, INFL,
// INFLATION, STRIP, BILL), or the kind's own name (fixed,
// inflation_linked). Any other word states none.
func couponKindOfWord(word string) (CouponKind, bool) {
	w := strings.ToUpper(strings.TrimSpace(word))
	if k := couponPhraseWords[w]; k != "" {
		return k, true
	}
	if k := couponKindWords[w]; k != "" {
		return k, true
	}
	if k := CouponKind(strings.ToLower(w)); k.Valid() {
		return k, true
	}
	return "", false
}

// readStatedDate reads a date a source states: YYYY-MM-DD, or as a
// description writes one (06/15/2031, 06/15/31, 15.06.2031).
func readStatedDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, true
	}
	return descriptionDate(s)
}

// readStatedPercent reads a rate a source states in percent: 4.25 or
// 4.25%.
func readStatedPercent(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ratingOfGrades is the worst bucket of agencies' grades, each read by
// RatingOfGrade, by lowestRating's rule: an unrated grade counts only
// where no agency grades the bond. Text that is no grade counts for
// nothing. inDefault reports a grade of default (GradeInDefault).
func ratingOfGrades(grades ...string) (bucket RatingBucket, inDefault bool) {
	var buckets []RatingBucket
	for _, g := range grades {
		if b, ok := RatingOfGrade(g); ok {
			buckets = append(buckets, b)
			inDefault = inDefault || GradeInDefault(g)
		}
	}
	return lowestRating(buckets), inDefault
}

// ReadPayloadTerms reads the terms a payload states under the keys
// PayloadTermsKeys names; grades are more grades it states, under keys
// of its source's own. A coupon kind of zero with no interest rate
// states a coupon of 0. A value it cannot read states nothing.
func ReadPayloadTerms(stated map[string]string, grades ...string) PayloadTerms {
	var t PayloadTerms
	if c, ok := readStatedPercent(stated[PayloadInterestRate]); ok {
		t.Coupon = &c
	}
	if d, ok := readStatedDate(stated[PayloadMaturityDate]); ok {
		t.Maturity = &d
	}
	if d, ok := readStatedDate(stated[PayloadCallDate]); ok {
		t.CallDate = &d
	}
	t.Frequency, _ = frequencyOfWord(stated[PayloadCouponFrequency])
	if k, ok := couponKindOfWord(stated[PayloadCouponKind]); ok {
		t.CouponKind = k
		if k == CouponZero && t.Coupon == nil {
			zero := 0.0
			t.Coupon = &zero
		}
	}
	t.Rating, t.Default = ratingOfGrades(append(slices.Clone(grades),
		stated[PayloadRatingMoodys], stated[PayloadRatingSP], stated[PayloadRatingFitch])...)
	return t
}
