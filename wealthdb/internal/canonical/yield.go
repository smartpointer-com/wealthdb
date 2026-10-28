package canonical

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// YieldRows is the config `yield` block as gold stores it: the tables
// gold.SyncYield replaces whole at every load. Conventions, Cycles and
// SettlementCycles are the built-in maps with the block's entries over
// them.
type YieldRows struct {
	Curves             []YieldCurvePoint
	Withholding        []YieldWithholding
	AccountWithholding []YieldAccountWithholding
	RatingFunds        []YieldRatingFund
	Conventions        []YieldConvention
	Cycles             []YieldCycle
	SettlementCycles   []SettlementCycle
}

// YieldCurvePoint is one tenor of a currency's public curve and the rate
// series that gives it.
type YieldCurvePoint struct {
	Currency string
	// Tenor is the config's text (3M, 1Y), TenorMonths its length.
	Tenor          string
	TenorMonths    int
	SilverSourceID string
	SeriesID       string
}

// YieldWithholding is the tax a country withholds at source on income,
// in percent, from an account of one tax wrapper. Wrapper is a
// TaxWrapper or WithholdingDefault.
type YieldWithholding struct {
	Country string
	Wrapper string
	RatePct float64
}

// YieldAccountWithholding is the tax a country withholds at source on
// one account's income, in percent, where it differs from the rate of
// the account's tax wrapper.
type YieldAccountWithholding struct {
	Country           string
	SilverSourceID    string
	AccountExternalID string
	RatePct           float64
}

// WithholdingDefault is the wrapper of the rate a country withholds from
// every wrapper it names no rate for.
const WithholdingDefault = "default"

// YieldRatingFund is a fund sliced by credit rating, and the bucket or
// range of buckets its lines carry.
type YieldRatingFund struct {
	FundRef string
	// Rating is the config's text (BBB, AAA-A); BucketHigh and BucketLow
	// are its two ends, the same bucket for a single one.
	Rating     string
	BucketHigh RatingBucket
	BucketLow  RatingBucket
}

// YieldConvention is a market's coupon frequency and day count. Market
// is a currency, or a currency, a colon and one of MarketSegments
// (USD:government).
type YieldConvention struct {
	Market string
	// Frequency is the coupon payments a year, 0 for a discount
	// instrument.
	Frequency int
	DayCount  DayCount
	Origin    YieldOrigin
}

// YieldCycle is the cycle one of the eleven broad sectors moves with.
type YieldCycle struct {
	Sector Sector
	Cycle  Cycle
	Origin YieldOrigin
}

// The projection's window in months: its default, and the longest the
// config takes.
const (
	DefaultHorizonMonths = 12
	MaxHorizonMonths     = 36
)

// YieldOrigin says where a convention or a cycle comes from: the
// built-in map, or the config, whose entry replaces the built-in one of
// its market or sector.
type YieldOrigin string

const (
	OriginBuiltin YieldOrigin = "builtin"
	OriginConfig  YieldOrigin = "config"
)

// MaxTenorMonths is the longest tenor a curve takes: 50 years.
const MaxTenorMonths = 50 * 12

// TenorMonths is the length in months of a tenor, a whole number and M
// or Y (3M, 1Y, 30Y), at most MaxTenorMonths.
func TenorMonths(tenor string) (int, error) {
	n := len(tenor)
	if n < 2 || tenor[0] == '0' || (tenor[n-1] != 'M' && tenor[n-1] != 'Y') ||
		strings.ContainsFunc(tenor[:n-1], func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, errors.New("a tenor is a whole number and M or Y: 3M, 1Y, 30Y")
	}
	digits, unit := tenor[:n-1], tenor[n-1:]
	// Four digits are past the bound in either unit, and fewer cannot
	// overflow.
	v, _ := strconv.Atoi(digits)
	if unit == "Y" {
		v *= 12
	}
	if len(digits) > 3 || v > MaxTenorMonths {
		return 0, errors.New("a tenor is at most 50 years")
	}
	return v, nil
}

// RatingBucket is a credit rating with its notches folded in: AA stands
// for AA+, AA and AA-, or Aa1 to Aa3.
type RatingBucket string

const (
	RatingAAA RatingBucket = "AAA"
	RatingAA  RatingBucket = "AA"
	RatingA   RatingBucket = "A"
	RatingBBB RatingBucket = "BBB"
	RatingBB  RatingBucket = "BB"
	// RatingB stands for B and every rating below it.
	RatingB RatingBucket = "B"
)

// ratingBuckets are the buckets, best first.
var ratingBuckets = []RatingBucket{RatingAAA, RatingAA, RatingA, RatingBBB, RatingBB, RatingB}

// ParseRating reads a bucket (BBB), or a range of two from its better
// end to its worse (AAA-A), into its two ends. A single bucket is both
// ends.
func ParseRating(s string) (high, low RatingBucket, err error) {
	h, l, isRange := strings.Cut(s, "-")
	if !isRange {
		l = h
	}
	hi, lo := slices.Index(ratingBuckets, RatingBucket(h)), slices.Index(ratingBuckets, RatingBucket(l))
	if hi < 0 || lo < 0 {
		names := make([]string, len(ratingBuckets))
		for i, b := range ratingBuckets {
			names[i] = string(b)
		}
		return "", "", fmt.Errorf("want a rating bucket (%s) or a range of two, best first (AAA-A)",
			strings.Join(names, ", "))
	}
	if hi > lo {
		return "", "", fmt.Errorf("a range runs from its better end to its worse: %s-%s", l, h)
	}
	return ratingBuckets[hi], ratingBuckets[lo], nil
}

// RatingUnrated is the bucket of a bond an agency states it does not
// rate (NR, WR), and of one a ledger record says no agency rates.
const RatingUnrated RatingBucket = "unrated"

// RatingBuckets are the buckets, best first, then RatingUnrated: the
// values a record of the ledger states a rating as.
func RatingBuckets() []RatingBucket {
	return append(slices.Clone(ratingBuckets), RatingUnrated)
}

// moodysGrades are Moody's long-term grades of each bucket above B.
var moodysGrades = map[string]RatingBucket{"AAA": RatingAAA, "AA1": RatingAA, "AA2": RatingAA, "AA3": RatingAA,
	"AA": RatingAA, "A1": RatingA, "A2": RatingA, "A3": RatingA, "BAA1": RatingBBB, "BAA2": RatingBBB,
	"BAA3": RatingBBB, "BAA": RatingBBB, "BA1": RatingBB, "BA2": RatingBB, "BA3": RatingBB, "BA": RatingBB}

// RatingOfGrade is the bucket of an agency's long-term grade: Moody's
// (Aa2, Baa1, a provisional (P)Aa2), or S&P's and Fitch's (AA-, BBB+).
// Every grade below BB is B; NR, WR and WD are RatingUnrated. A
// short-term grade (P-1, A-1+) and any other text are no grade.
func RatingOfGrade(grade string) (RatingBucket, bool) {
	g := plainGrade(grade)
	switch {
	case g == "NR" || g == "WR" || g == "WD" || g == "N/R":
		return RatingUnrated, true
	case moodysGrades[g] != "":
		return moodysGrades[g], true
	}
	base := strings.TrimRight(g, "+-")
	switch base {
	case "AAA", "AA", "A", "BBB", "BB":
		return RatingBucket(base), true
	case "B", "CCC", "CC", "C", "D", "SD", "RD", "DDD", "DD", "B1", "B2", "B3", "CAA1", "CAA2", "CAA3", "CAA", "CA":
		return RatingB, true
	}
	return "", false
}

// GradeInDefault reports whether an agency's grade says the issuer is in
// default: D, SD and RD, and Fitch's former DDD and DD.
func GradeInDefault(grade string) bool {
	switch plainGrade(grade) {
	case "D", "SD", "RD", "DDD", "DD":
		return true
	}
	return false
}

// plainGrade is a grade upper-cased, without a provisional (P) before it
// or a * after it.
func plainGrade(grade string) string {
	g := strings.ToUpper(strings.TrimSpace(grade))
	return strings.TrimSuffix(strings.TrimPrefix(g, "(P)"), "*")
}

// CouponKind is how a bond's coupon is set.
type CouponKind string

const (
	CouponFixed           CouponKind = "fixed"
	CouponFloating        CouponKind = "floating"
	CouponInflationLinked CouponKind = "inflation_linked"
	// CouponZero is a bond that pays no coupon: a zero, a strip, a bill.
	CouponZero CouponKind = "zero"
)

var couponKinds = []CouponKind{CouponFixed, CouponFloating, CouponInflationLinked, CouponZero}

// CouponKinds are the coupon kinds.
func CouponKinds() []CouponKind {
	return slices.Clone(couponKinds)
}

// Valid reports whether k is one of CouponKinds.
func (k CouponKind) Valid() bool {
	return slices.Contains(couponKinds, k)
}

// DayCount is how a bond counts the days of a coupon period.
type DayCount string

const (
	DayCount30360  DayCount = "30/360"
	DayCount30E360 DayCount = "30E/360"
	DayCountActAct DayCount = "ACT/ACT"
	DayCountAct360 DayCount = "ACT/360"
	DayCountAct365 DayCount = "ACT/365"
)

var dayCounts = []DayCount{DayCount30360, DayCount30E360, DayCountActAct, DayCountAct360, DayCountAct365}

// DayCounts are the day counts a convention takes.
func DayCounts() []DayCount {
	return slices.Clone(dayCounts)
}

// Valid reports whether d is one of DayCounts.
func (d DayCount) Valid() bool {
	return slices.Contains(dayCounts, d)
}

var couponFrequencies = []int{0, 1, 2, 4, 12}

// CouponFrequencies are the coupon payments a year a convention takes;
// 0 is a discount instrument, which pays none.
func CouponFrequencies() []int {
	return slices.Clone(couponFrequencies)
}

// BondFrequencies are the coupon payments a year of a bond that pays
// coupons: CouponFrequencies without 0.
func BondFrequencies() []int {
	return slices.DeleteFunc(CouponFrequencies(), func(n int) bool { return n == 0 })
}

// ValidCouponFrequency reports whether n is one of CouponFrequencies.
func ValidCouponFrequency(n int) bool {
	return slices.Contains(couponFrequencies, n)
}

// MarketBill is the market segment of a currency's bills: government
// paper that pays no coupon.
const MarketBill = "bill"

// MarketSegments are what may follow a currency and a colon in a
// convention's market: the bond sectors, and bill.
func MarketSegments() []string {
	out := make([]string, 0, len(bondSectors)+1)
	for _, s := range bondSectors {
		out = append(out, string(s))
	}
	return append(out, MarketBill)
}

// builtinConventions are the conventions of the markets wealthdb knows
// without config, by market. A US corporate or municipal bond reads the
// currency's own; a Treasury note or bond and a bill read theirs.
var builtinConventions = []YieldConvention{
	{Market: "CHF", Frequency: 1, DayCount: DayCount30360, Origin: OriginBuiltin},
	{Market: "EUR", Frequency: 1, DayCount: DayCount30360, Origin: OriginBuiltin},
	{Market: "USD", Frequency: 2, DayCount: DayCount30360, Origin: OriginBuiltin},
	{Market: "USD:" + MarketBill, Frequency: 0, DayCount: DayCountAct360, Origin: OriginBuiltin},
	{Market: "USD:" + string(SectorGovernment), Frequency: 2, DayCount: DayCountActAct, Origin: OriginBuiltin},
}

// BuiltinConventions are the built-in conventions, by market.
func BuiltinConventions() []YieldConvention {
	return slices.Clone(builtinConventions)
}

// A SettlementCycle is a market's settlement cycle from a day on: a trade
// settles Days business days after it. Market is a country (ISO 3166
// alpha-2). From is nil for a market's first cycle where it holds from
// the first day on. The yield report derives a dividend's record day from
// its ex day by the cycle in force on the ex day (docs/YIELD.md §4).
type SettlementCycle struct {
	Market string
	From   *time.Time
	Days   int
	Origin YieldOrigin
}

// EuroAreaMarkets are the members of the euro area, whose markets settle
// on TARGET2's days.
var EuroAreaMarkets = []string{"AT", "BE", "BG", "CY", "DE", "EE", "ES", "FI", "FR", "GR", "HR", "IE", "IT", "LT",
	"LU", "LV", "MT", "NL", "PT", "SI", "SK"}

// BuiltinSettlementCycles are the settlement cycles of the markets that
// have one where the config names none, by market and day, oldest first:
//
//   - the United States: 3 days; 2 from 2017-09-05; 1 from 2024-05-28;
//   - Canada: 3 days; 2 from 2017-09-05; 1 from 2024-05-27;
//   - the euro area, Switzerland, the United Kingdom, Sweden, Denmark and
//     Norway: 2 from 2014-10-06; 1 from 2027-10-11;
//   - Japan: 3 days; 2 from 2019-07-16.
//
// Hong Kong has none: a share's record day there follows the closure of
// its register, which no cycle gives.
func BuiltinSettlementCycles() []SettlementCycle {
	day := func(y int, m time.Month, d int) *time.Time {
		t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return &t
	}
	type step struct {
		from *time.Time
		days int
	}
	us := []step{{nil, 3}, {day(2017, time.September, 5), 2}, {day(2024, time.May, 28), 1}}
	canada := []step{{nil, 3}, {day(2017, time.September, 5), 2}, {day(2024, time.May, 27), 1}}
	europe := []step{{day(2014, time.October, 6), 2}, {day(2027, time.October, 11), 1}}
	japan := []step{{nil, 3}, {day(2019, time.July, 16), 2}}
	markets := map[string][]step{"US": us, "CA": canada, "JP": japan}
	for _, m := range slices.Concat(EuroAreaMarkets, []string{"CH", "GB", "SE", "DK", "NO"}) {
		markets[m] = europe
	}
	var out []SettlementCycle
	for _, m := range slices.Sorted(maps.Keys(markets)) {
		for _, s := range markets[m] {
			c := SettlementCycle{Market: m, Days: s.days, Origin: OriginBuiltin}
			if s.from != nil {
				c.From = day(s.from.Year(), s.from.Month(), s.from.Day())
			}
			out = append(out, c)
		}
	}
	return out
}

// Cycle is the part of the economic cycle a sector moves with.
type Cycle string

const (
	CycleCyclical  Cycle = "cyclical"
	CycleDefensive Cycle = "defensive"
	CycleSensitive Cycle = "sensitive"
)

var cycles = []Cycle{CycleCyclical, CycleDefensive, CycleSensitive}

// Cycles are the cycles a sector moves with.
func Cycles() []Cycle {
	return slices.Clone(cycles)
}

// Valid reports whether c is one of Cycles.
func (c Cycle) Valid() bool {
	return slices.Contains(cycles, c)
}

// builtinCycles is the cycle of each of the eleven broad sectors.
var builtinCycles = map[Sector]Cycle{
	SectorMaterials:        CycleCyclical,
	SectorConsumerCyclical: CycleCyclical,
	SectorFinancials:       CycleCyclical,
	SectorRealEstate:       CycleCyclical,
	SectorConsumerStaples:  CycleDefensive,
	SectorHealthCare:       CycleDefensive,
	SectorUtilities:        CycleDefensive,
	SectorCommunication:    CycleSensitive,
	SectorEnergy:           CycleSensitive,
	SectorIndustrials:      CycleSensitive,
	SectorTechnology:       CycleSensitive,
}

// BuiltinCycle is the built-in cycle of one of the eleven broad
// sectors; false for any other sector.
func BuiltinCycle(s Sector) (Cycle, bool) {
	c, ok := builtinCycles[s]
	return c, ok
}

// BuiltinCycles are the eleven broad sectors in the vocabulary's order,
// each with its built-in cycle.
func BuiltinCycles() []YieldCycle {
	out := make([]YieldCycle, len(equitySectors))
	for i, s := range equitySectors {
		out[i] = YieldCycle{Sector: s, Cycle: builtinCycles[s], Origin: OriginBuiltin}
	}
	return out
}
