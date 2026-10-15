package canonical

// ExposureRows is the config `exposure` block as gold stores it: the
// tables gold.SyncExposure replaces whole at every load.
type ExposureRows struct {
	Proxies        []ExposureProxy
	ReferenceFunds []string
	SizeFunds      []ExposureSizeFund
	Tags           []ExposureTag
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

// ExposureTag states what one holding is, where no composition and no
// security fact says so. A blank field states nothing.
type ExposureTag struct {
	SilverSourceID       string
	InstrumentExternalID string
	Sector               string
	Country              string
	Currency             string
}

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
