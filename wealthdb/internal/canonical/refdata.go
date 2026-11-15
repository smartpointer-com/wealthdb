package canonical

import (
	"encoding/json"
	"slices"
	"time"
)

// Reference records: what a fund holds, what a security is, and the
// public rate series. They come from a reference source (the refdata or
// fred collector, or a generator writing the synthetic kind), never from
// an account. Every one is a dated fact: a composition or a rate is dated
// by the day it describes, a company's figure by the last day of its
// period, a security fact by the day it was looked up, a distribution by
// the day its source stated it. docs/DESIGN.md §7.6 and
// docs/adapters/refdata.md say how they load.

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
	// Origin is CompositionProvider (or "") for a reference provider's
	// composition, CompositionParsed for one a holding's own source
	// parses from a document of the fund's, such as a quarterly report.
	// A parsed composition describes the fund from AsOf on, never before,
	// and up to ValidUntil, the date of the fund's next document, where
	// one exists (docs/EXPOSURE.md §4).
	Origin     string
	ValidUntil *int64
}

// The origins of a composition.
const (
	CompositionProvider = "provider"
	CompositionParsed   = "parsed"
)

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
	// Region is the region a line states where it states no country, as
	// a value of the vocabulary (Region or RegionOther).
	Region   *string
	Maturity *time.Time
	Coupon   *Decimal
	// The two legs of a currency forward at notional, each in its own
	// currency, where the provider states them.
	LongCurrency  *string
	LongAmount    *Decimal
	ShortCurrency *string
	ShortAmount   *Decimal
	// A debt line's coupon kind as the provider writes it (N-PORT's
	// Fixed, Floating, Variable, None), whether it is in default and its
	// interest in arrears, and whether its coupon is paid in kind (in
	// more of the same paper, not in cash).
	CouponKind *string
	IsDefault  *bool
	InArrears  *bool
	PaidInKind *bool
	// ExchangeRate is the rate the provider valued the line at: units of
	// the line's currency per unit of the composition's.
	ExchangeRate *Decimal
	// The duration and yield figures an issuer's file states per line:
	// durations in years, yields in percent. RealDuration and RealYield
	// are an inflation-linked line's. AccrualDate is the day interest
	// accrues from.
	Duration         *Decimal
	ModifiedDuration *Decimal
	YieldToMaturity  *Decimal
	YieldToWorst     *Decimal
	YieldToCall      *Decimal
	RealDuration     *Decimal
	RealYield        *Decimal
	AccrualDate      *time.Time
	Payload          json.RawMessage
}

// The reference tables that pass from silver to gold column for column
// are declared once, in RefTables: a reference silver's table, the gold
// table it loads into and their columns. The reference stream reads a
// table's rows by its entry (silver.NewReferenceStream) and the writer
// inserts them by it (gold.Writer.InsertReference). A batch carries
// them as RefRows, one value per column. Compositions and lines are no
// such tables: a source other than refdata writes them, and the reader
// derives some of their columns, so they keep records of their own.

// RefColKind says how a reference column's text in silver reads into its
// value in gold.
type RefColKind int

const (
	// RefText is text, nil where silver holds NULL.
	RefText RefColKind = iota
	// RefTextReq is text every row holds.
	RefTextReq
	// RefInt is an integer, nil where silver holds NULL.
	RefInt
	// RefIntReq is an integer every row holds, such as as_of.
	RefIntReq
	// RefBool is a boolean, nil where silver holds NULL.
	RefBool
	// RefDecimal is a decimal, nil where silver holds NULL, blank or
	// text that is no decimal.
	RefDecimal
	// RefDecimalReq is a decimal every row holds. Text that is no
	// decimal fails the load: the figure is the row's whole point.
	RefDecimalReq
	// RefDecimalStrict is a decimal, nil where silver holds NULL or
	// blank. Other text that is no decimal fails the load.
	RefDecimalStrict
	// RefDate is a YYYY-MM-DD date, nil where silver holds anything
	// else.
	RefDate
	// RefDateReq is a YYYY-MM-DD date every row holds. Text that is no
	// date fails the load.
	RefDateReq
	// RefDateStrict is a YYYY-MM-DD date, nil where silver holds NULL
	// or blank. Other text that is no date fails the load.
	RefDateStrict
	// RefJSON is a payload, nil where silver holds NULL or blank.
	RefJSON
)

// RefColumn is one column of a reference table: its name in silver, its
// name in gold, and how its value reads.
type RefColumn struct {
	Name, Gold string
	Kind       RefColKind
	// Bare says a value that fails to read is named in the error by its
	// row alone, not by its column and its text.
	Bare bool
}

// RefTable is a reference table that passes from silver to gold column
// for column.
type RefTable struct {
	// Silver and Gold are the table's names; What names it in errors.
	Silver, Gold, What string
	// Kind is the kind of record its rows are (Window.Spans): a fund's
	// tables are its fund-days, each security table a kind of its own.
	Kind string
	// Version is the reference silver's schema version that adds the
	// table. A file migrated before it holds no rows of it.
	Version int
	// Key orders the rows: within a fund-day for a fund's table, and as
	// the cursor of the pages for a security table. Each is a column.
	Key []string
	// Cols are the columns in gold's order, which is silver's too. Every
	// row's values follow them; the gold row adds silver_source_id first.
	Cols []RefColumn
	// Enums names, per gold column, the values it may hold. A value
	// outside them is refused by the writer; nil passes.
	Enums map[string][]string
	// WholeBy, where set, names the columns a table is read and
	// replaced by whole: every row of each group a row of the window
	// names, whatever its own as_of, replaces the group's rows in gold.
	WholeBy []string
	// Record and RecordBy name a row in a read error: a format and the
	// columns, as silver holds them, its verbs take.
	Record   string
	RecordBy []string
}

// RefRows are a batch's rows of one reference table: a value per column
// of Table.Cols, nil for NULL. Text is a string, an integer an int64, a
// boolean a bool, a decimal a Decimal, a date a time.Time at UTC
// midnight and a payload its JSON bytes.
type RefRows struct {
	Table          *RefTable
	SilverSourceID string
	Rows           [][]any
}

// Col is the index in t.Cols of the gold column named gold, -1 where t
// has none.
func (t *RefTable) Col(gold string) int {
	for i, c := range t.Cols {
		if c.Gold == gold {
			return i
		}
	}
	return -1
}

// RefTableOf is the entry of RefTables whose gold table is gold, nil
// where none is.
func RefTableOf(gold string) *RefTable {
	for _, t := range RefTables {
		if t.Gold == gold {
			return t
		}
	}
	return nil
}

// fundDay is the two columns that name a fund's table's fund-day.
var fundDay = []RefColumn{{"fund_ref", "fund_ref", RefTextReq, false}, {"as_of", "as_of", RefIntReq, false}}

// col is a column whose name is the same in silver and gold.
func col(name string, kind RefColKind) RefColumn {
	return RefColumn{Name: name, Gold: name, Kind: kind}
}

// RefTables are the reference tables declared once, in the order a
// batch writes them. Every row is a dated fact: a fund-day's by the day
// it describes, a security's by the day it was looked up, a period fact
// by its period's last day, a distribution by the day its source stated
// it.
var RefTables = []*RefTable{
	// What a fund is, as of one day. domicile is ISO 3166 alpha-2, ter
	// percent a year. hedged_currency is the currency a hedged share
	// class is hedged into; it replaces the currency of every line the
	// look-through reaches.
	{Silver: "attributes", Gold: "fund_attributes", What: "attributes", Kind: RefFundDays, Version: 1,
		Cols: append(slices.Clone(fundDay), col("domicile", RefText), col("legal_form", RefText),
			col("ucits", RefBool), col("replication", RefText), col("distribution", RefText),
			col("ter", RefDecimal), col("hedged_currency", RefText), col("lending", RefBool),
			col("index_name", RefText), col("depositary", RefText), col("payload", RefJSON))},
	// A party a fund is exposed to, in one role (swap, lending,
	// custodian or depositary), as of one day. share is in percent of
	// net assets.
	{Silver: "counterparties", Gold: "fund_counterparties", What: "counterparties", Kind: RefFundDays, Version: 1,
		Key: []string{"role", "name"},
		Cols: append(slices.Clone(fundDay), col("role", RefTextReq), col("name", RefTextReq),
			col("lei", RefText), col("share", RefDecimal), col("payload", RefJSON))},
	// The identifiers a lookup tied to one security, as of the day of
	// the lookup. security_key is the key gold asked about (SecurityKey).
	{Silver: "securities", Gold: "security_identities", What: "securities", Kind: RefSecurities, Version: 1,
		Key: []string{"security_key", "as_of", "provider"},
		Cols: []RefColumn{col("security_key", RefTextReq), col("as_of", RefIntReq), col("provider", RefTextReq),
			col("isin", RefText), col("cusip", RefText), col("figi", RefText), col("lei", RefText),
			col("ticker", RefText), col("exchange", RefText), col("name", RefText),
			col("security_type", RefText), col("listing_currency", RefText), col("payload", RefJSON)}},
	// One field of one security as one provider stated it on one day, in
	// the provider's own scheme (an SIC code, a vendor sector, an issuer
	// label).
	{Silver: "security_facts", Gold: "security_facts", What: "security facts", Kind: RefSecurityFacts, Version: 1,
		Key: []string{"security_key", "field", "as_of", "provider"},
		Cols: []RefColumn{col("security_key", RefTextReq), col("field", RefTextReq), col("as_of", RefIntReq),
			col("provider", RefTextReq), col("value", RefTextReq), col("scheme", RefText), col("payload", RefJSON)}},
	// How much a fund's value moves for a move in rates at one tenor
	// (RiskTenors), as its filing states it. An interest-rate row names
	// the curve's currency and carries dv01 and dv100, the move for one
	// and for one hundred basis points. A credit-spread row has currency
	// NoCurrency and carries the move for one basis point of
	// investment-grade and of non-investment-grade spreads. Amounts are
	// in the composition's currency, signed as filed. Filers follow no
	// one sign convention: gold's fund_risk_signed reads each filing
	// positive where the fund loses value as rates rise.
	{Silver: "fund_risk", Gold: "fund_risk", What: "fund risk", Kind: RefFundDays, Version: 2,
		Key: []string{"currency", "tenor"},
		Cols: append(slices.Clone(fundDay), col("currency", RefTextReq), col("tenor", RefTextReq),
			col("dv01", RefDecimal), col("dv100", RefDecimal), col("credit_ig_dv01", RefDecimal),
			col("credit_hy_dv01", RefDecimal), col("payload", RefJSON))},
	// A fund's total return for one month, in percent, as the filing of
	// the fund-day states it for the share class its reference maps to.
	// month is the month's last day, YYYY-MM-DD.
	{Silver: "fund_returns", Gold: "fund_returns", What: "fund returns", Kind: RefFundDays, Version: 4,
		Key: []string{"month"},
		Cols: append(slices.Clone(fundDay), col("month", RefTextReq), col("class_id", RefText),
			RefColumn{Name: "total_return", Gold: "total_return", Kind: RefDecimalReq, Bare: true},
			col("payload", RefJSON)),
		Record: "fund returns: %s %s", RecordBy: []string{"fund_ref", "month"}},
	// One figure a provider states for a fund as a whole on one day.
	// field names the figure (effective_duration, sec_yield,
	// seven_day_yield, credit_quality, …); label is "" for a single
	// figure and the provider's own label for one part of a breakdown.
	// value is percent for a yield or a share, years for a duration,
	// days for a weighted average maturity or life; a field whose unit
	// is another one names it (option_adjusted_spread_bp), and a ratio
	// is a plain number (convexity, price_earnings).
	{Silver: "characteristics", Gold: "fund_characteristics", What: "characteristics", Kind: RefFundDays,
		Version: 2, Key: []string{"provider", "field", "label"},
		Cols: append(slices.Clone(fundDay), col("provider", RefTextReq), col("field", RefTextReq),
			col("label", RefTextReq), col("value", RefDecimalReq), col("payload", RefJSON)),
		Record: "characteristic (%s, %d, %s, %s, %q)", RecordBy: []string{"fund_ref", "as_of", "provider", "field", "label"}},
	// A figure a company reported for a period. as_of is the period's
	// last day, period_start its first (the last day itself for a
	// balance on a day). field is wealthdb's name for the figure
	// (eps_diluted, dividends_per_share, net_income, …); scheme the
	// concept the provider served it under.
	{Silver: "period_facts", Gold: "security_period_facts", What: "period facts", Kind: RefPeriodFacts, Version: 2,
		Key: []string{"security_key", "field", "as_of", "period_start", "provider"},
		Cols: []RefColumn{col("security_key", RefTextReq), col("field", RefTextReq), col("as_of", RefIntReq),
			col("period_start", RefDateReq), col("provider", RefTextReq), col("value", RefDecimalReq),
			col("scheme", RefText), col("unit", RefText), col("fiscal_year", RefInt), col("fiscal_period", RefText),
			col("form", RefText), col("filed", RefDate), col("payload", RefJSON)},
		Record: "period fact (%s, %s, %d, %s, %s)", RecordBy: []string{"security_key", "field", "as_of", "period_start", "provider"}},
	// One distribution of a fund or a security as a provider states it.
	// ref is the fund reference or security key the wanted list names it
	// by, or jpx:<code> for a row of the JPX list, by the share's Tokyo
	// Stock Exchange code. event_id is the source's id of the
	// distribution, else its kind and days joined. A later statement of
	// the same distribution replaces an earlier one. The amount is gross
	// per share or unit, in currency. A provider that states a ref's
	// whole history drops from silver the rows it no longer states,
	// which leaves nothing in a window to say so, so a ref's and
	// provider's rows are read and replaced whole.
	{Silver: "dividend_events", Gold: "dividend_calendar", What: "dividend events", Kind: RefDividendEvents,
		Version: 3, Key: []string{"ref", "provider", "event_id"}, WholeBy: []string{"ref", "provider"},
		Cols: []RefColumn{col("ref", RefTextReq), col("provider", RefTextReq), col("event_id", RefTextReq),
			col("as_of", RefIntReq), col("kind", RefTextReq),
			{Name: "ex_date", Gold: "ex_day", Kind: RefDateStrict}, {Name: "record_date", Gold: "record_day", Kind: RefDateStrict},
			{Name: "pay_date", Gold: "pay_day", Kind: RefDateStrict},
			{Name: "declared_date", Gold: "declared_day", Kind: RefDateStrict},
			col("amount", RefDecimalStrict), col("currency", RefText), col("status", RefText)},
		Enums:  map[string][]string{"kind": dividendKindWords(), "status": {string(DividendFinal), string(DividendEstimated)}},
		Record: "dividend event (%s, %s, %s)", RecordBy: []string{"ref", "provider", "event_id"}},
}

// dividendKindWords is DividendEventKinds as words.
func dividendKindWords() []string {
	out := make([]string, len(DividendEventKinds))
	for i, k := range DividendEventKinds {
		out[i] = string(k)
	}
	return out
}

// NoCurrency is ISO 4217's code for "no currency": the currency of a
// fund's credit-spread risk rows, which belong to no one curve.
const NoCurrency = "XXX"

// The tenors a filing states a fund's rate sensitivity at.
var RiskTenors = []string{"3M", "1Y", "5Y", "10Y", "30Y"}

// DividendEventKind says what a distribution pays.
type DividendEventKind string

const (
	DividendKindDividend        DividendEventKind = "dividend"
	DividendKindCapitalGain     DividendEventKind = "capital_gain"
	DividendKindReturnOfCapital DividendEventKind = "return_of_capital"
	DividendKindSpecial         DividendEventKind = "special"
	DividendKindOther           DividendEventKind = "other"
)

// DividendEventKinds are the kinds a distribution may be.
var DividendEventKinds = []DividendEventKind{DividendKindDividend, DividendKindCapitalGain,
	DividendKindReturnOfCapital, DividendKindSpecial, DividendKindOther}

// Valid reports whether k is one of DividendEventKinds.
func (k DividendEventKind) Valid() bool { return slices.Contains(DividendEventKinds, k) }

// DividendEventStatus says whether a source states a distribution as
// final, or as estimated: its amount may still change.
type DividendEventStatus string

const (
	DividendFinal     DividendEventStatus = "final"
	DividendEstimated DividendEventStatus = "estimated"
)

// Valid reports whether s is final or estimated.
func (s DividendEventStatus) Valid() bool { return s == DividendFinal || s == DividendEstimated }

// StatedPeriodChange is one insert into gold's `stated_periods`: an
// account's summary of one period as a document states it. AsOf is the
// UTC midnight of the period's last day, PeriodStart its first. Each
// figure is nil where the document states none. NetAdditions is signed,
// money into the account positive; Fees is a positive amount. A stated
// period is informative: it is no position and no flow.
type StatedPeriodChange struct {
	SilverSourceID    string
	AccountExternalID string
	AsOf              int64
	PeriodStart       int64
	Currency          string
	BeginningValue    *Decimal
	EndingValue       *Decimal
	Accrual           *Decimal
	NetAdditions      *Decimal
	NetGain           *Decimal
	Income            *Decimal
	Fees              *Decimal
	// SourceSHA256 names the stating document, so two documents'
	// statements of one period stay apart.
	SourceSHA256 string
	Payload      json.RawMessage
}

// ExposureReportChange is one insert into gold's `exposure_reports`: a
// holding's split on one day as a source reads it from a document, such
// as a fund's quarterly report. AsOf is the UTC midnight of that day.
// Dimensions maps each dimension it states (DeclarationDimensions) to its
// values and their shares in percent, summing to 100. gold.SyncExposure
// copies each one into the declarations as a parsed record
// (docs/EXPOSURE.md §9).
type ExposureReportChange struct {
	SilverSourceID       string
	InstrumentExternalID string
	AsOf                 int64
	Dimensions           map[string]map[string]float64
	// Source names the document, as JSON; Note is free text.
	Source json.RawMessage
	Note   string
}

// RateObservationChange is one insert into gold's `rate_observations`:
// one day's value of a published interest-rate series, in percent, as
// the source serves it. The yield config's curves name series by
// silver_source_id and SeriesID.
type RateObservationChange struct {
	SilverSourceID string
	SeriesID       string
	AsOf           int64
	Value          Decimal
	Payload        json.RawMessage
}
