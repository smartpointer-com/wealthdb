package canonical

import (
	"slices"
	"strings"
	"time"
)

// ExposureRows is the config `exposure` block as gold stores it: the
// tables gold.SyncExposure replaces whole at every load.
type ExposureRows struct {
	Proxies        []ExposureProxy
	ReferenceFunds []string
	SizeFunds      []ExposureSizeFund
	Declarations   []ExposureDeclaration
	Custody        []ExposureCustody
	Settings       ExposureSettings
}

// ExposureProxy says that ProxyRef's composition stands in for FundRef's:
// only on the days FundRef has none for, or always. Leverage scales the
// proxy's lines; the difference goes to cash.
type ExposureProxy struct {
	FundRef  string
	ProxyRef string
	Always   bool
	Leverage float64
}

// ExposureSizeFund is a broad fund whose weights size the stocks no
// market cap sizes, and the share of its markets' value it covers, in
// percent.
type ExposureSizeFund struct {
	FundRef     string
	CoveragePct float64
}

// ExposureDeclaration is one record of the declarations ledger, or one
// gold parses from a source's exposure_reports (gold.parsedDeclarations):
// what the household states about a holding no public source describes,
// split into the leaves the look-through gives it, the holding's legal
// situs, and the yield figures no source states (docs/EXPOSURE.md §9). A
// rate names an account instead of a holding.
type ExposureDeclaration struct {
	// Line is the record's line in the ledger, and its id in gold; a
	// parsed record has no line and takes a negative id.
	Line                 int
	SilverSourceID       string
	InstrumentExternalID string
	AccountExternalID    string
	// AsOf is the UTC midnight of the record's date, nil for a record
	// that is simply true.
	AsOf *int64
	// Dimensions are the dimensions the record declares, in the order of
	// DeclarationDimensions.
	Dimensions []string
	// Situs is the legal situs the record declares, an ISO 3166 code, ""
	// for none.
	Situs string
	// Source is the record's source as JSON text, "" when it has none.
	Source string
	Note   string
	// Leaves are the cross product of the declared dimensions. Their
	// shares sum to one, up to float rounding; a record that declares
	// none has none.
	Leaves []ExposureDeclaredLeaf
	// The yield records, nil where the line states none.
	Rate     *DeclaredRate
	Rating   *DeclaredRating
	Dividend *DeclaredDividend
	Terms    *DeclaredTerms
}

// StatesExposure reports whether the record declares a dimension or a
// situs: a record of yield figures alone describes no exposure.
func (d ExposureDeclaration) StatesExposure() bool {
	return len(d.Dimensions) > 0 || d.Situs != ""
}

// The ledger's keys of the yield records, and of the account a rate
// names.
const (
	DeclarationRate     = "rate"
	DeclarationRating   = "rating"
	DeclarationDividend = "dividend"
	DeclarationTerms    = "terms"
	DeclarationAccount  = "account"
)

// DeclarationYieldKinds are the yield records a line may carry.
var DeclarationYieldKinds = []string{DeclarationRate, DeclarationRating, DeclarationDividend, DeclarationTerms}

// DeclaredRate is the interest rate an account pays on its balance, in
// percent a year, from the record's day on: in Currency where the
// account holds several, to Until where it is a term deposit.
type DeclaredRate struct {
	Pct      float64
	Currency string
	Until    *time.Time
}

// DeclaredRating is a holding's credit bucket, and the agency that
// grades it where one does.
type DeclaredRating struct {
	Bucket RatingBucket
	Agency string
}

// DeclaredDividend is a stock's indicated dividend: the amount per share
// of one payment, in Currency ("" the holding's own), and the payments a
// year.
type DeclaredDividend struct {
	Amount    float64
	Frequency int
	Currency  string
}

// DeclaredTerms are the terms of a bond, a note or a mortgage that no
// source states, and the figures a fund's factsheet states for a fund
// no filing describes. A nil pointer or an empty string states nothing;
// a Frequency of 0 states a discount instrument, a Coupon of 0 a zero.
type DeclaredTerms struct {
	Coupon     *float64
	Frequency  *int
	Maturity   *time.Time
	CallDate   *time.Time
	FixedUntil *time.Time
	ResetDate  *time.Time
	CouponKind CouponKind
	DayCount   DayCount
	// Duration is a fund's effective duration in years, Yield its yield
	// to maturity in percent.
	Duration *float64
	Yield    *float64
}

// ExposureDeclaredLeaf is one part of a declared holding. A field is ""
// where the record does not declare its dimension, and Unknown for
// the part of a declared dimension the record leaves open.
type ExposureDeclaredLeaf struct {
	// Share is the leaf's fraction of the holding's value.
	Share                                         float64
	AssetClass, Region, Country, Currency, Sector string
}

// Set sets the leaf's value in the dimension dim, one of
// DeclarationDimensions; any other dim sets nothing.
func (l *ExposureDeclaredLeaf) Set(dim, value string) {
	switch dim {
	case DimensionRegion:
		l.Region = value
	case DimensionCountry:
		l.Country = value
	case DimensionCurrency:
		l.Currency = value
	case DimensionSector:
		l.Sector = value
	case DimensionAssetClass:
		l.AssetClass = value
	}
}

// Get is the leaf's value in the dimension dim, one of
// DeclarationDimensions; "" for any other dim.
func (l ExposureDeclaredLeaf) Get(dim string) string {
	switch dim {
	case DimensionRegion:
		return l.Region
	case DimensionCountry:
		return l.Country
	case DimensionCurrency:
		return l.Currency
	case DimensionSector:
		return l.Sector
	case DimensionAssetClass:
		return l.AssetClass
	}
	return ""
}

// The dimensions a declaration may state, in the order gold lists them.
const (
	DimensionRegion     = "region"
	DimensionCountry    = "country"
	DimensionCurrency   = "currency"
	DimensionSector     = "sector"
	DimensionAssetClass = "asset_class"
)

// DeclarationDimensions are the dimensions a declaration may state.
var DeclarationDimensions = []string{DimensionRegion, DimensionCountry, DimensionCurrency, DimensionSector,
	DimensionAssetClass}

// DeclarationSitus is the key that declares a holding's legal situs: one
// country, never split, so never crossed with the dimensions.
const DeclarationSitus = "situs"

// Unknown is the value of every dimension for what a declaration
// leaves open.
const Unknown = "unknown"

// ExposureCustody names who keeps a source's assets.
type ExposureCustody struct {
	SilverSourceID string
	Custodian      string
	Country        string
}

// ExposureSettings are the scalars gold reads: max_gap_days from the
// config block, the other two fixed.
type ExposureSettings struct {
	// MaxGapDays is how far a composition may lie from a day, before or
	// after, and still describe it.
	MaxGapDays int
	// AgedDays is how old a composition may be before a row flags it.
	AgedDays int
	// PartialPct is how much of a fund, in percent, its weights may
	// leave unexplained before a row flags it.
	PartialPct float64
}

// Values of the exposure settings; only MaxGapDays has a config key.
const (
	DefaultMaxGapDays = 400
	DefaultAgedDays   = 35
	DefaultPartialPct = 3.0
)

// DeclaredValueValid reports whether value is one a record may state in
// the dimension dim: Unknown, or a name of the dimension's vocabulary (a
// region or RegionOther, an ISO 3166 alpha-2 code, a 3-letter ISO 4217 code, a sector,
// an asset class). Any other dim takes no value.
func DeclaredValueValid(dim, value string) bool {
	if value == Unknown {
		return slices.Contains(DeclarationDimensions, dim)
	}
	switch dim {
	case DimensionRegion:
		return Region(value) == RegionOther || slices.Contains(Regions(), Region(value))
	case DimensionCountry:
		c, ok := CountryByCode(value)
		return ok && c.Code == value
	case DimensionCurrency:
		return len(value) == 3 && strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == ""
	case DimensionSector:
		return Sector(value).Valid()
	case DimensionAssetClass:
		return AssetClass(value).Valid()
	}
	return false
}
