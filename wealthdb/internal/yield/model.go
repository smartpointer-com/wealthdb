// Package yield computes what the holdings earn and how they move with
// interest rates (the `wealthdb yield` report), and what they are
// expected to pay month by month (the income projection). It reads
// Inputs that gold assembles for one instant and returns plain rows:
// one set of measures per holding, which every grain sums, so the
// grains reconcile by construction. docs/YIELD.md defines every figure.
//
// Rates are in percent a year (4.25 is 4.25%). Amounts are in a
// holding's own currency unless a field says the output currency.
package yield

import (
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// Inputs is what the report and the projection read at one instant.
type Inputs struct {
	// AsOf is the instant, the last second of a day; Day its UTC
	// midnight.
	AsOf int64
	Day  time.Time
	// Currency is the output currency.
	Currency string
	Holdings []Holding
	Accounts map[AccountKey]Account
	// Terms are instrument_terms, by holding instrument.
	Terms map[InstrumentKey]Terms
	// Rates are the account rates in force at the instant.
	Rates map[AccountKey][]Rate
	// Funds are the facts of each fund held, by its security key.
	Funds map[string]*Fund
	// Curves are the public curves, by currency.
	Curves map[string]*Curve
	// Conventions are yield_conventions, by market.
	Conventions map[string]canonical.YieldConvention
	// Income is each holding's income received over the twelve months
	// to the instant; AccountIncome what an account received with no
	// holding named (a deposit's interest, rent, a pension), by its
	// currency.
	Income        map[HoldingKey]Received
	AccountIncome map[AccountCurrency]Received
	// AccountPayments are the payments of those twelve months that name
	// no holding: rent and pensions, which the projection repeats, and
	// interest, which it repeats for a cash balance whose rate no source
	// states.
	AccountPayments []AccountPayment
	// FXRates are the rates from each currency to the output currency at
	// the instant.
	FXRates map[string]float64
	// Payments are what each holding was paid in the 24 months to the
	// instant, oldest first.
	Payments map[HoldingKey][]Payment
	// Dividends are the ledger's indicated dividends, by instrument.
	Dividends map[InstrumentKey]canonical.DeclaredDividend
	// Securities are the reported figures of each security held, by its
	// key.
	Securities map[string]Facts
	// Lines are what the largest line naming each security in the funds
	// held says of it, by its key.
	Lines map[string]LineInfo
	// AverageAmount is each holding's average amount, in its own
	// currency, at the month ends of the twelve months to the instant.
	AverageAmount map[HoldingKey]float64
	// Withholding is yield_withholding: percent by situs country and
	// tax wrapper (canonical.WithholdingDefault for every other).
	Withholding map[string]map[string]float64
	// Situs is each holding's legal situs country, where known.
	Situs map[HoldingKey]string
}

// AccountKey names an account.
type AccountKey struct{ Source, Account string }

// AccountCurrency names an account's balance in one currency.
type AccountCurrency struct {
	AccountKey
	Currency string
}

// InstrumentKey names an instrument at its source.
type InstrumentKey struct{ Source, Instrument string }

// HoldingKey names one line of the holdings: a position or a cash
// balance (PositionKey cash:<CCY>).
type HoldingKey struct{ Source, Account, PositionKey string }

// Holding is one line of the holdings at the instant, valued as
// `holdings global` values it.
type Holding struct {
	HoldingKey
	Instrument string
	AssetClass string
	Vehicle    string
	// Currency is the holding's own.
	Currency string
	IsCash   bool
	// Value is in the output currency; Amount in the holding's own.
	Value, Amount float64
	// Quantity is the units held: a bond's face value, a fund's shares.
	Quantity *float64
	// Accrued is the interest accrued, where the source states it.
	Accrued    *float64
	Acquired   *time.Time
	Symbol     string
	Name       string
	SecurityID string // the holding's security key, "" for none
	// Key is the key `exposure securities` names the holding's security
	// by: its security key, else one of its own.
	Key string
}

// FX is the rate from the holding's currency to the output currency:
// Value over Amount.
func (h Holding) FX() float64 {
	if h.Amount == 0 {
		return 0
	}
	return h.Value / h.Amount
}

// Account is what the grains name an account by.
type Account struct {
	Name, Kind, TaxWrapper, Nickname string
	Portfolio, PortfolioName         string
}

// Terms are a holding's instrument_terms row.
type Terms struct {
	Coupon                                *float64
	Maturity, CallDate, FixedUntil, Reset *time.Time
	Frequency                             *int
	CouponKind                            canonical.CouponKind
	DayCount                              canonical.DayCount
	MakeWhole                             bool
	Rating                                string
	// InDefault is an agency's grade of default.
	InDefault bool
	// Duration and Yield are a fund's factsheet figures from a ledger
	// record, as of TermsAsOf.
	Duration, Yield *float64
	TermsAsOf       *time.Time
	// Declared says which fields a ledger record states.
	CouponDeclared, MaturityDeclared bool
}

// Rate is one account_rates row in force at the instant.
type Rate struct {
	Currency  string // "" for the account's own
	Credit    bool
	Tier      int
	Pct       float64
	UpTo      *float64
	ValidFrom *time.Time
	TermEnd   *time.Time
	Declared  bool
}

// LineInfo is what a fund's line says of the security it names: a
// bond's coupon, maturity and coupon kind, its price (per 100 of face
// for a bond, per unit otherwise, in the line's currency), and the
// yield and modified duration it states or its price implies. A line in
// default or arrears, or paid in kind, implies no yield.
type LineInfo struct {
	Default         bool
	Coupon          *float64
	Maturity        *time.Time
	CouponKind      string
	Price           *float64
	Yield, Duration *float64
	Currency        string
}

// Fund is what is known of one fund at the instant: its issuer's or its
// filing's figures, its rate sensitivity, and what its lines imply.
type Fund struct {
	// The figures its page or filing states, each with its day.
	YieldToMaturity, YieldToWorst, Duration, SECYield, DistributionYield, TrailingYield *Dated
	SevenDayYield, MaturityYears, PriceEarnings                                         *Dated
	// Risk is the fund's DV01 per curve currency and tenor, for its whole
	// net assets, in the currency of NetAssets, as of RiskAsOf.
	Risk      map[string]map[string]float64
	RiskAsOf  *time.Time
	NetAssets *float64
	// Lines summarises the fund's composition at the instant.
	Lines *LineSummary
}

// Dated is a figure and the day it describes.
type Dated struct {
	Value float64
	Day   time.Time
}

// LineSummary is what a fund's lines say, weighted by their value: the
// yield and duration their prices imply, their maturity, and the shares
// of the fund each reading covers.
type LineSummary struct {
	AsOf  time.Time
	Aged  bool
	Yield *float64 // implied, in percent
	// Duration is modified duration in years; MaturityYears the years to
	// maturity.
	Duration, MaturityYears *float64
	// The shares of the fund's value, in percent: lines with an implied
	// yield, floating lines, lines in default, paid-in-kind lines.
	CoveredPct, FloatingPct, DefaultPct, PaidInKindPct float64
}

// Curve is a currency's public curve at the instant.
type Curve struct {
	Currency string
	Zero     *fixedincome.Curve
	// Benchmark is the rate of BenchmarkMonths, the currency's
	// benchmark tenor (3M where the curve has it, else its shortest).
	Benchmark       float64
	BenchmarkMonths int
}

// Received is income received, by type, in the output currency.
type Received map[IncomeType]float64

// Total is the income of every type.
func (r Received) Total() float64 {
	t := 0.0
	for _, v := range r {
		t += v
	}
	return t
}

// AccountPayment is one payment an account received that names no
// holding, in its currency.
type AccountPayment struct {
	AccountCurrency
	Day    time.Time
	Amount float64
	Type   IncomeType
}

// HoldingOf is the holding a transaction belongs to: the holding of the
// instrument it names in its account, else in the account's portfolio,
// else the first at its source. A bank books a dividend to the cash
// account beside the custody account that holds the security.
func (in *Inputs) HoldingOf(src, acct, iid string) (HoldingKey, bool) {
	if iid == "" {
		return HoldingKey{}, false
	}
	var first *HoldingKey
	portfolio := in.Accounts[AccountKey{src, acct}].Portfolio
	for i := range in.Holdings {
		h := &in.Holdings[i]
		if h.Source != src || h.Instrument != iid {
			continue
		}
		if h.Account == acct {
			return h.HoldingKey, true
		}
		if p := in.Accounts[AccountKey{h.Source, h.Account}].Portfolio; p != "" && p == portfolio {
			return h.HoldingKey, true
		}
		if first == nil {
			first = &h.HoldingKey
		}
	}
	if first == nil {
		return HoldingKey{}, false
	}
	return *first, true
}

// fxOf is the rate from ccy to the output currency, 0 where none is
// known.
func (in *Inputs) fxOf(ccy string) float64 {
	if ccy == in.Currency {
		return 1
	}
	return in.FXRates[ccy]
}

// Payment is one payment a holding received.
type Payment struct {
	Day      time.Time
	Amount   float64 // in the holding's own currency
	Quantity *float64
	Type     IncomeType
}

// Facts are a security's reported figures: trailing twelve months of
// earnings and dividends per share, a year earlier too, and the ratios
// behind interest coverage.
type Facts struct {
	EPS, EPSYearAgo, DPS, DPSYearAgo *float64
	OperatingIncome, InterestExpense *float64
	Currency                         string
}

// IncomeType is a kind of income the projection counts.
type IncomeType string

const (
	TypeInterest      IncomeType = "interest"
	TypeCoupons       IncomeType = "coupons"
	TypeDividends     IncomeType = "dividends"
	TypeDistributions IncomeType = "distributions"
	TypeRent          IncomeType = "rent"
	TypePension       IncomeType = "pension"
)

// IncomeTypes are the types, in the order the reports list them.
var IncomeTypes = []IncomeType{TypeInterest, TypeCoupons, TypeDividends, TypeDistributions, TypeRent, TypePension}
