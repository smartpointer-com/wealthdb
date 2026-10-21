package canonical

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CouponLine is what an interest line's description says of the bond
// whose coupon it pays: the coupon, in percent a year, to half a unit of
// its last printed place (Tolerance), the maturity, and whether the line
// names a bond issue.
type CouponLine struct {
	Coupon, Tolerance float64
	Maturity          time.Time
	// Issue says the line names a bond issue: a Treasury, a note or a
	// bond (US TREASU NT, UST BOND).
	Issue bool
}

var (
	// A coupon, not part of a longer figure (100%), perhaps glued to
	// the month and year of its maturity: 9.875%09/99.
	lineCoupon = regexp.MustCompile(`(?:^|[^\d.])(\d{1,2}(?:\.\d+)?|\.\d+)%(?:(\d{2})/(\d{2})(?:[^\d/]|$))?`)
	// The maturity after DUE: DUE 09/09/99.
	lineMaturity = regexp.MustCompile(`\bDUE\s+(\d{1,2}/\d{1,2}/(?:\d{4}|\d{2}))\b`)
)

// The words that name a bond issue in an interest line.
var issueWords = WordSet("TREASURY", "TREASU", "TREAS", "UST", "NOTE", "NOTES", "NT", "BOND", "BONDS", "BD")

// ReadCouponLine reads the bond an interest line names where a broker
// books a bond's coupon as the account's interest, with no instrument:
// a coupon in percent and a maturity after DUE, as in "US TREASU NT
// 9.875%09/99UST NOTE DUE 09/09/99". A month and year glued to the
// coupon must be the maturity's. ok is false where the line states
// either not, as a cash balance's interest does.
func ReadCouponLine(desc string) (CouponLine, bool) {
	upper := strings.ToUpper(desc)
	c := lineCoupon.FindStringSubmatch(upper)
	m := lineMaturity.FindStringSubmatch(upper)
	if c == nil || m == nil {
		return CouponLine{}, false
	}
	coupon, err := strconv.ParseFloat(c[1], 64)
	maturity, ok := descriptionDate(m[1])
	if err != nil || !ok {
		return CouponLine{}, false
	}
	if c[2] != "" && (c[2] != maturity.Format("01") || c[3] != maturity.Format("06")) {
		return CouponLine{}, false
	}
	places := 0
	if i := strings.IndexByte(c[1], '.'); i >= 0 {
		places = len(c[1]) - i - 1
	}
	issue := false
	for _, w := range strings.FieldsFunc(upper, func(r rune) bool { return r < 'A' || r > 'Z' }) {
		issue = issue || issueWords[w]
	}
	return CouponLine{Coupon: coupon, Tolerance: 0.5 * math.Pow(10, -float64(places)), Maturity: maturity,
		Issue: issue}, true
}

// Pays says the line pays the coupon of a bond of the coupon and the
// maturity given: the coupon as far as the line prints it, the maturity
// to the day. A bond whose terms state either not is paid by no line.
func (l CouponLine) Pays(coupon *float64, maturity *time.Time) bool {
	if coupon == nil || maturity == nil {
		return false
	}
	y, m, d := maturity.Date()
	ly, lm, ld := l.Maturity.Date()
	return math.Abs(*coupon-l.Coupon) <= l.Tolerance+1e-9 && y == ly && m == lm && d == ld
}

// OnePaying is the key of the one bond of n that the line pays, at(i)
// giving the i-th bond's key, coupon and maturity; false where none pays
// it, or bonds of two keys do.
func (l CouponLine) OnePaying(n int, at func(i int) (key string, coupon *float64, maturity *time.Time)) (string,
	bool) {
	found := ""
	for i := range n {
		key, coupon, maturity := at(i)
		if !l.Pays(coupon, maturity) || key == found {
			continue
		}
		if found != "" {
			return "", false
		}
		found = key
	}
	return found, found != ""
}
