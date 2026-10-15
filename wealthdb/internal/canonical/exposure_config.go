package canonical

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

// ExposureDeclaration is one record of the declarations ledger: what the
// household states about a holding no public source describes, split
// into the leaves the look-through gives it.
type ExposureDeclaration struct {
	// Line is the record's line in the ledger, and its id in gold.
	Line                 int
	SilverSourceID       string
	InstrumentExternalID string
	// AsOf is the UTC midnight of the record's date, nil for a record
	// that is simply true.
	AsOf *int64
	// Dimensions are the dimensions the record declares, in the order of
	// DeclarationDimensions.
	Dimensions []string
	// Source is the record's source as JSON text, "" when it has none.
	Source string
	Note   string
	// Leaves are the cross product of the declared dimensions. Their
	// shares sum to exactly one.
	Leaves []ExposureDeclaredLeaf
}

// ExposureDeclaredLeaf is one part of a declared holding. A field is ""
// where the record does not declare its dimension, and Unclassified for
// the part of a declared dimension the record leaves open.
type ExposureDeclaredLeaf struct {
	// Share is the leaf's fraction of the holding's value.
	Share                                         float64
	AssetClass, Region, Country, Currency, Sector string
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

// Unclassified is the value of every dimension for what a declaration
// leaves open.
const Unclassified = "unclassified"

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
