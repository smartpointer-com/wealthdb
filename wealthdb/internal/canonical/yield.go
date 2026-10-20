package canonical

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// YieldRows is the config `yield` block as gold stores it: the tables
// gold.SyncYield replaces whole at every load. Conventions and Cycles
// are the built-in maps with the block's entries over them.
type YieldRows struct {
	Curves      []YieldCurvePoint
	Withholding []YieldWithholding
	RatingFunds []YieldRatingFund
	Conventions []YieldConvention
	Cycles      []YieldCycle
	Settings    YieldSettings
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

// YieldSettings are the scalars of the yield block.
type YieldSettings struct {
	// HorizonMonths is the projection's window when none is given.
	HorizonMonths int
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
