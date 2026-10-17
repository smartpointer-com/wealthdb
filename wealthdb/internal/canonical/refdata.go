package canonical

import (
	"encoding/json"
	"time"
)

// Reference records: what a fund holds and what a security is. They come
// from a reference source (the refdata collector, or a generator writing
// the synthetic kind), never from an account. Every one is a dated fact:
// a composition is dated by the day it describes, a security fact by the
// day it was looked up. docs/DESIGN.md §7.6 and docs/adapters/refdata.md
// say how they load.

// CompositionBasis says what a composition's lines describe.
type CompositionBasis string

const (
	// CompositionHoldings is a fund's own holdings.
	CompositionHoldings CompositionBasis = "holdings"
	// CompositionCollateral is the substitute basket a swap-based fund
	// holds. Its economic exposure is the index, not the basket, so the
	// look-through never reads it.
	CompositionCollateral CompositionBasis = "collateral"
	// CompositionIndex is the index a fund tracks, where the provider
	// lists it: the exposure of a swap-based fund.
	CompositionIndex CompositionBasis = "index"
)

// Valid reports whether b is one of the three bases.
func (b CompositionBasis) Valid() bool {
	switch b {
	case CompositionHoldings, CompositionCollateral, CompositionIndex:
		return true
	}
	return false
}

// FundCompositionChange is one insert into gold's `fund_compositions`:
// one fund's composition as of one day, on one basis.
type FundCompositionChange struct {
	SilverSourceID string
	// FundRef is the fund's reference in the key scheme of refkeys.go:
	// isin:…, cusip:… or symbol:….
	FundRef string
	// AsOf is the UTC midnight of the day the composition describes,
	// never the day it was fetched or filed.
	AsOf  int64
	Basis CompositionBasis
	// Provider names the source module that served the composition.
	Provider string
	// SourceKind says what kind of document it is: daily_file, filing,
	// report, vendor or inbox.
	SourceKind string
	FundName   *string
	// Currency is the fund's base currency, NetAssets its net assets in
	// it, where the provider states them.
	Currency  *string
	NetAssets *Decimal
	// LineCount and WeightSum describe the lines; WeightSum is in
	// percent of net assets.
	LineCount        int64
	WeightSum        *Decimal
	UnitsOutstanding *Decimal
	// FetchedAt is the instant of the run that fetched it.
	FetchedAt int64
	Payload   json.RawMessage
}

// FundLineChange is one insert into gold's `fund_lines`: one line of a
// composition, its labels as the provider wrote them.
type FundLineChange struct {
	SilverSourceID string
	FundRef        string
	AsOf           int64
	Basis          CompositionBasis
	// LineNo orders the lines as the provider listed them.
	LineNo int64
	// SecurityKey is the line's security (SecurityKey), "" where the
	// line names none (a cash line without an identifier).
	SecurityKey string
	ISIN        *string
	CUSIP       *string
	FIGI        *string
	Ticker      *string
	Exchange    *string
	Name        *string
	// AssetType, Country, RiskCountry, Sector and Industry are the
	// provider's labels, mapped to the vocabularies at report time.
	AssetType *string
	// Weight is the line's share of net assets in percent; it can be
	// negative (a short, a liability) or absent.
	Weight      *Decimal
	Quantity    *Decimal
	MarketValue *Decimal
	Currency    *string
	Country     *string
	RiskCountry *string
	Sector      *string
	Industry    *string
	Maturity    *time.Time
	Coupon      *Decimal
	// The two legs of a currency forward at notional, each in its own
	// currency, where the provider states them.
	LongCurrency  *string
	LongAmount    *Decimal
	ShortCurrency *string
	ShortAmount   *Decimal
	Payload       json.RawMessage
}

// FundAttributesChange is one insert into gold's `fund_attributes`: what
// a fund is, as of one day.
type FundAttributesChange struct {
	SilverSourceID string
	FundRef        string
	AsOf           int64
	Domicile       *string // ISO 3166 alpha-2
	LegalForm      *string
	UCITS          *bool
	Replication    *string
	Distribution   *string
	TER            *Decimal // percent a year
	// HedgedCurrency is the currency a hedged share class is hedged
	// into; it replaces the currency of every line it looks through to.
	HedgedCurrency *string
	Lending        *bool
	IndexName      *string
	Depositary     *string
	Payload        json.RawMessage
}

// FundCounterpartyChange is one insert into gold's `fund_counterparties`:
// a party a fund is exposed to, in one role, as of one day.
type FundCounterpartyChange struct {
	SilverSourceID string
	FundRef        string
	AsOf           int64
	// Role is swap, lending, custodian or depositary.
	Role string
	Name string
	LEI  *string
	// Share is the counterparty's share in percent of net assets.
	Share   *Decimal
	Payload json.RawMessage
}

// SecurityChange is one insert into gold's `security_identities`: the
// identifiers a lookup tied to one security, as of the day of the
// lookup.
type SecurityChange struct {
	SilverSourceID string
	// SecurityKey is the key gold asked about (SecurityKey).
	SecurityKey     string
	AsOf            int64
	Provider        string
	ISIN            *string
	CUSIP           *string
	FIGI            *string
	LEI             *string
	Ticker          *string
	Exchange        *string
	Name            *string
	SecurityType    *string
	ListingCurrency *string
	Payload         json.RawMessage
}

// SecurityFactChange is one insert into gold's `security_facts`: one
// field of one security as one provider stated it on one day, in the
// provider's own scheme (an SIC code, a vendor sector, an issuer label).
type SecurityFactChange struct {
	SilverSourceID string
	SecurityKey    string
	Field          string
	AsOf           int64
	Provider       string
	Value          string
	Scheme         *string
	Payload        json.RawMessage
}
