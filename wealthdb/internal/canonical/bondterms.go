package canonical

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// BondTerms is what a bond's description or its OpenFIGI ticker states
// of its terms. A nil or zero field is one it does not state.
type BondTerms struct {
	// Coupon is the coupon in percent a year.
	Coupon   *float64
	Maturity *time.Time
	// Frequency is the coupon payments a year, 0 where unstated.
	Frequency  int
	CouponKind CouponKind
	// Bill is a government bill: discount paper with no coupon.
	Bill bool
	// CallDate is the first day the issuer may redeem the bond early at a
	// stated price. A make-whole call, which pays the holder the value of
	// the coupons it forgoes, sets MakeWhole and no call date: it does not
	// shorten the bond's life for a yield to worst.
	CallDate  *time.Time
	MakeWhole bool
	// Rating is the lower of the agencies' grades a description states,
	// as a bucket; "" where it states none. Default is a grade of default
	// (D, SD, RD).
	Rating  RatingBucket
	Default bool
}

// The words of a description that state a bond's coupon frequency. SEMI
// before ANNUALLY or ANNUAL states it too; ANNUAL alone does not, since
// a municipal bond's ANNUAL APPROP is its appropriation.
var frequencyWords = map[string]int{"ANNUALLY": 1, "SEMIANNUALLY": 2, "SEMI-ANNUALLY": 2, "SEMIANNUAL": 2,
	"SEMI-ANNUAL": 2, "QUARTERLY": 4, "MONTHLY": 12}

// The words of a description that state a bond's coupon kind on their
// own. A word before COUPON or CPN (FIXED COUPON, ZERO CPN) states it
// too, and wins; VAR and VARIABLE do only before RATE or RT, since a
// municipal bond's VAR PURP is its various purposes.
var couponKindWords = map[string]CouponKind{"FLOAT": CouponFloating, "FLOATING": CouponFloating,
	"FRN": CouponFloating, "STRIP": CouponZero, "STRIPS": CouponZero, "BILL": CouponZero, "BILLS": CouponZero,
	"TIPS": CouponInflationLinked, "INFL": CouponInflationLinked, "INFLATION": CouponInflationLinked}

// The words before COUPON or CPN that state the coupon kind.
var couponPhraseWords = map[string]CouponKind{"FIXED": CouponFixed, "FLOATING": CouponFloating,
	"VARIABLE": CouponFloating, "VAR": CouponFloating, "ZERO": CouponZero, "STEP": CouponFixed}

// The words that name a rating agency, its grade the next word.
var agencyWords = WordSet("MOODYS", "MOODY'S", "S&P", "FITCH")

// The words that open a date that is no maturity: the day a bond was
// dated or issued.
var datedWords = WordSet("DTD", "DATED", "ISSUED")

// callWindow is how many words after CALL its date and its MAKE WHOLE
// may stand.
const callWindow = 3

var (
	// A coupon as a description writes it: 4.25%, 4%, 4.00000%.
	couponPct = regexp.MustCompile(`^(\d{1,2}(?:\.\d+)?)%$`)
	// The fraction of a coupon written as a whole number and a fraction:
	// 4 1/2%.
	fractionPct = regexp.MustCompile(`^(\d)/(\d{1,2})%$`)
	wholeNumber = regexp.MustCompile(`^\d{1,2}$`)
	// A date: 06/15/2031 and 06/15/31 (US, month first), 15.06.2031
	// (Swiss and German, day first).
	usDate    = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{2}|\d{4})$`)
	dottedDay = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})\.(\d{4})$`)
	// A coupon as OpenFIGI's ticker writes it: 4.125, 4, or 4 1/4; a
	// variable coupon with a V before it (V4.565).
	figiCoupon   = regexp.MustCompile(`^V?\d{1,2}(?:\.\d+)?$`)
	figiFraction = regexp.MustCompile(`^(\d)/(\d{1,2})$`)
)

// descriptionWords are a description's words, upper-cased, each trimmed
// of the punctuation around it; a word wrapped in brackets loses them
// (a grade written (P)Aa2 keeps its own).
func descriptionWords(desc string) []string {
	raw := strings.Fields(strings.ToUpper(desc))
	out := make([]string, len(raw))
	for i, w := range raw {
		w = strings.Trim(w, ",;[]")
		if strings.HasPrefix(w, "(") && strings.HasSuffix(w, ")") && strings.Count(w, ")") == 1 {
			w = w[1 : len(w)-1]
		}
		out[i] = w
	}
	return out
}

// ReadBondTerms reads a bond's terms from a broker's description:
//
//   - a coupon in percent (4.25%, 4 1/2%) followed by a date is the
//     coupon and the maturity (4.25% 06/15/2031, 1.915% 10.05.2034); a
//     coupon alone is the coupon;
//   - else the first date that no CALL in the three words before, and no
//     DTD, DATED or ISSUED, precedes is the maturity;
//   - CALL and a date in the three words after it is the first call
//     date; MAKE WHOLE in those words a make-whole call; CALLABLE only
//     says a call exists;
//   - the frequency words (SEMIANNUALLY, SEMI ANNUALLY, QUARTERLY) and
//     the coupon kind words (FIXED COUPON, ZERO CPN, VAR RT, FRN, TIPS)
//     say what they say; BILL or BILLS make it a bill;
//   - MOODYS, MOODY'S, S&P and FITCH, each followed by a grade, give the
//     rating: the lower of the grades; a grade of default sets Default.
//
// A US date with a two-digit year is in this century.
func ReadBondTerms(desc string) BondTerms {
	var t BondTerms
	words := descriptionWords(desc)
	used := make([]bool, len(words))
	callAt := -1
	var grades []string
	var phrased CouponKind
	next := func(i int) string {
		if i+1 < len(words) {
			return words[i+1]
		}
		return ""
	}
	for i, w := range words {
		switch {
		case w == "CALL":
			callAt = i
			for j := i + 1; j < min(len(words)-1, i+1+callWindow); j++ {
				if words[j] == "MAKE" && words[j+1] == "WHOLE" {
					t.MakeWhole = true
				}
			}
		case datedWords[w]:
			if _, ok := descriptionDate(next(i)); ok {
				used[i+1] = true
			}
		case agencyWords[w]:
			grades = append(grades, next(i))
		case w == "SEMI" && (next(i) == "ANNUALLY" || next(i) == "ANNUAL"):
			t.Frequency = 2
			used[i+1] = true
		case frequencyWords[w] != 0 && !used[i]:
			t.Frequency = frequencyWords[w]
		case (w == "COUPON" || w == "CPN") && i > 0 && couponPhraseWords[words[i-1]] != "":
			phrased = couponPhraseWords[words[i-1]]
		case (w == "VAR" || w == "VARIABLE") && (next(i) == "RATE" || next(i) == "RT"):
			if t.CouponKind == "" {
				t.CouponKind = CouponFloating
			}
		case couponKindWords[w] != "" && t.CouponKind == "":
			t.CouponKind = couponKindWords[w]
		}
		if w == "BILL" || w == "BILLS" {
			t.Bill = true
		}
		if c, ok := coupon(words, i); ok && t.Coupon == nil {
			t.Coupon = &c
			if d, ok := descriptionDate(next(i)); ok && !used[i+1] {
				t.Maturity = &d
				used[i+1] = true
			}
			continue
		}
		if used[i] {
			continue
		}
		if d, ok := descriptionDate(w); ok {
			inCall := callAt >= 0 && i-callAt <= callWindow
			switch {
			case inCall && t.CallDate == nil && !t.MakeWhole:
				t.CallDate = &d
			case !inCall && t.Maturity == nil:
				t.Maturity = &d
			}
		}
	}
	if phrased != "" {
		t.CouponKind = phrased
	}
	if t.CouponKind == CouponZero {
		zero := 0.0
		t.Coupon = &zero
	}
	t.Rating, t.Default = ratingOfGrades(grades...)
	return t
}

// coupon reads the coupon at word i: a percentage, or a fraction with a
// percent sign after a whole number (4 1/2%).
func coupon(words []string, i int) (float64, bool) {
	if m := couponPct.FindStringSubmatch(words[i]); m != nil {
		c, _ := strconv.ParseFloat(m[1], 64)
		return c, true
	}
	if m := fractionPct.FindStringSubmatch(words[i]); m != nil && i > 0 && wholeNumber.MatchString(words[i-1]) {
		whole, _ := strconv.ParseFloat(words[i-1], 64)
		num, _ := strconv.ParseFloat(m[1], 64)
		den, _ := strconv.ParseFloat(m[2], 64)
		if den > 0 {
			return whole + num/den, true
		}
	}
	return 0, false
}

// ReadFigiTicker reads a bond's terms from the ticker OpenFIGI gives it:
// the issuer, the coupon (4.125, 4, 4 1/4; V4.565 a variable one; FLOAT
// or F a floating one) and the maturity (07/31/31 or 07/31/2031), then
// perhaps the original tenor (7Y). A Treasury bill's ticker starts with
// B and pays no coupon; an inflation-indexed Treasury's starts with
// TII.
func ReadFigiTicker(ticker string) BondTerms {
	var t BondTerms
	words := strings.Fields(strings.ToUpper(ticker))
	for i, w := range words {
		d, ok := descriptionDate(w)
		if !ok || i == 0 {
			continue
		}
		c := words[i-1]
		if c == "FLOAT" || c == "F" || c == "FRN" {
			t.Maturity, t.CouponKind = &d, CouponFloating
			break
		}
		frac := 0.0
		if m := figiFraction.FindStringSubmatch(c); m != nil && i >= 2 {
			num, _ := strconv.ParseFloat(m[1], 64)
			den, _ := strconv.ParseFloat(m[2], 64)
			if den > 0 {
				frac, c = num/den, words[i-2]
			}
		}
		if figiCoupon.MatchString(c) {
			v, _ := strconv.ParseFloat(strings.TrimPrefix(c, "V"), 64)
			v += frac
			t.Coupon, t.Maturity = &v, &d
			switch {
			case strings.HasPrefix(c, "V"):
				t.CouponKind = CouponFloating
			case words[0] == "TII":
				t.CouponKind = CouponInflationLinked
			case v == 0:
				t.CouponKind = CouponZero
				t.Bill = words[0] == "B"
			}
		}
		break
	}
	return t
}

// descriptionDate reads one word as a date: US month first, or day
// first between dots.
func descriptionDate(w string) (time.Time, bool) {
	var y, m, d int
	if g := usDate.FindStringSubmatch(w); g != nil {
		m, _ = strconv.Atoi(g[1])
		d, _ = strconv.Atoi(g[2])
		y, _ = strconv.Atoi(g[3])
		if len(g[3]) == 2 {
			y += 2000
		}
	} else if g := dottedDay.FindStringSubmatch(w); g != nil {
		d, _ = strconv.Atoi(g[1])
		m, _ = strconv.Atoi(g[2])
		y, _ = strconv.Atoi(g[3])
	} else {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if m < 1 || m > 12 || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}

// lowestRating is the worst of the buckets, "" for none; an unrated
// grade counts only where no agency grades the bond.
func lowestRating(grades []RatingBucket) RatingBucket {
	worst := RatingBucket("")
	for _, g := range grades {
		switch {
		case g == RatingUnrated:
			if worst == "" {
				worst = g
			}
		case worst == "" || worst == RatingUnrated || slices.Index(ratingBuckets, g) > slices.Index(ratingBuckets, worst):
			worst = g
		}
	}
	return worst
}
