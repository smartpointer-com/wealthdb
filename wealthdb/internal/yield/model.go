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
	"slices"
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
	// Daily is each holding's amount, in its own currency, on each day of
	// the twelve months to the instant. FormerDaily is, for each
	// instrument held, its amount on those days in the positions that are
	// no holding at the instant, such as an account's that sold it, in the
	// currency of its holdings.
	Daily       map[HoldingKey]Daily
	FormerDaily map[InstrumentKey]Daily
	// Held are the accounts whose positions held each instrument at some
	// snapshot to the instant, sorted.
	Held map[InstrumentKey][]string
	// pools are the holdings of each instrument (poolKey), which Compute
	// indexes.
	pools map[string][]Holding
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
	// Duration is modified duration in years, over the lines that have
	// one; MaturityYears the years to maturity.
	Duration, MaturityYears *float64
	// The shares of the fund's value, in percent: lines with an implied
	// yield, lines with a duration, floating lines, lines in default,
	// paid-in-kind lines.
	CoveredPct, DurationPct, FloatingPct, DefaultPct, PaidInKindPct float64
	// The shares of the whole fund, in percent, a part its composition
	// leaves out counting as neither: debt lines, the debt lines that
	// mature within a year, equity lines.
	DebtPct, ShortDebtPct, EquityPct float64
	// Derivatives says a line is a derivative: a future, a swap, an
	// option or a forward.
	Derivatives bool
}

// Daily is a holding's amount, in its own currency, on each of the Days
// of the twelve months to the instant: the sum of its spans. A day no
// span covers was not held.
type Daily struct {
	Days  int
	Spans []Span
}

// Span is an amount held over the days [From, To) of a Daily, the
// window's first day 0.
type Span struct {
	From, To int
	Amount   float64
}

// Mean is the mean daily amount: every day counts, a day not held as
// zero. A span outside the window counts nothing; with no days it is
// zero.
func (d Daily) Mean() float64 {
	if d.Days == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range d.Spans {
		if s.From >= 0 && s.To <= d.Days && s.From < s.To {
			sum += s.Amount * float64(s.To-s.From)
		}
	}
	return sum / float64(d.Days)
}

// Curve is a currency's public curve at the instant.
type Curve struct {
	Zero *fixedincome.Curve
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
// instrument it names in its account. An account that held the
// instrument once and holds none of it now has none: its transactions
// are its sold position's (Sold). Any other account's belong to the
// first holding of the instrument in the account's portfolio: a bank
// books a dividend to the cash account beside the custody account that
// holds the security. Where none of the portfolio's accounts holds it but
// one held it once, they are the sold position's too. Else they belong to
// the first holding of it at its source.
func (in *Inputs) HoldingOf(src, acct, iid string) (HoldingKey, bool) {
	if iid == "" {
		return HoldingKey{}, false
	}
	var sibling, first *HoldingKey
	portfolio := in.Accounts[AccountKey{src, acct}].Portfolio
	for i := range in.Holdings {
		h := &in.Holdings[i]
		if h.Source != src || h.Instrument != iid {
			continue
		}
		if h.Account == acct {
			return h.HoldingKey, true
		}
		if sibling == nil && portfolio != "" && in.Accounts[AccountKey{h.Source, h.Account}].Portfolio == portfolio {
			sibling = &h.HoldingKey
		}
		if first == nil {
			first = &h.HoldingKey
		}
	}
	ik := InstrumentKey{src, iid}
	switch {
	case slices.Contains(in.Held[ik], acct):
		return HoldingKey{}, false
	case sibling != nil:
		return *sibling, true
	case in.portfolioHeld(ik, portfolio):
		return HoldingKey{}, false
	case first != nil:
		return *first, true
	}
	return HoldingKey{}, false
}

// portfolioHeld says an account of portfolio held instrument ik once
// (Held).
func (in *Inputs) portfolioHeld(ik InstrumentKey, portfolio string) bool {
	return portfolio != "" && slices.ContainsFunc(in.Held[ik], func(a string) bool {
		return in.Accounts[AccountKey{ik.Source, a}].Portfolio == portfolio
	})
}

// Sold is the key of the position an account sold of an instrument, as
// `income` names a holding no longer held: its account and the
// instrument. false where neither the account nor its portfolio ever held
// the instrument (Held). It holds the transactions HoldingOf gives no
// holding.
func (in *Inputs) Sold(src, acct, iid string) (HoldingKey, bool) {
	ik := InstrumentKey{src, iid}
	return HoldingKey{src, acct, iid}, iid != "" && (slices.Contains(in.Held[ik], acct) ||
		in.portfolioHeld(ik, in.Accounts[AccountKey{src, acct}].Portfolio))
}

// poolKey names the holdings a holding's instrument pools with: those of
// its security (SecurityID), which names one security at every source,
// else those of its instrument at its source; "" for a holding of no
// instrument.
func poolKey(h Holding) string {
	switch {
	case h.SecurityID != "":
		return "security\x00" + h.SecurityID
	case h.Instrument != "":
		return "instrument\x00" + h.Source + "\x00" + h.Instrument
	}
	return ""
}

// index pools the holdings by their instruments (poolKey), for
// instrumentOf.
func (in *Inputs) index() {
	in.pools = map[string][]Holding{}
	for _, h := range in.Holdings {
		if k := poolKey(h); k != "" {
			in.pools[k] = append(in.pools[k], h)
		}
	}
}

// instrumentOf is every holding of h's instrument, at any source
// (poolKey), and the sold positions of it (Sold) at each source where it
// is held: of the accounts, and the other accounts of the portfolios,
// that held it there once and hold none of it.
func (in *Inputs) instrumentOf(h Holding) (holdings []Holding, sold []HoldingKey) {
	k := poolKey(h)
	switch {
	case k == "":
		return []Holding{h}, nil
	case in.pools != nil:
		holdings = in.pools[k]
	default:
		for _, o := range in.Holdings {
			if poolKey(o) == k {
				holdings = append(holdings, o)
			}
		}
	}
	holds := map[InstrumentKey]map[string]bool{}
	var iks []InstrumentKey
	for _, o := range holdings {
		ik := InstrumentKey{o.Source, o.Instrument}
		if holds[ik] == nil {
			holds[ik], iks = map[string]bool{}, append(iks, ik)
		}
		holds[ik][o.Account] = true
	}
	for _, ik := range iks {
		var accts []string
		portfolios := map[string]bool{}
		for _, a := range in.Held[ik] {
			accts = append(accts, a)
			if p := in.Accounts[AccountKey{ik.Source, a}].Portfolio; p != "" {
				portfolios[p] = true
			}
		}
		for a, acct := range in.Accounts {
			if a.Source == ik.Source && portfolios[acct.Portfolio] && !slices.Contains(accts, a.Account) {
				accts = append(accts, a.Account)
			}
		}
		slices.Sort(accts)
		for _, a := range accts {
			if !holds[ik][a] {
				sold = append(sold, HoldingKey{ik.Source, a, ik.Instrument})
			}
		}
	}
	return holdings, sold
}

// fxOf is the rate from ccy to the output currency, 0 where none is
// known.
func (in *Inputs) fxOf(ccy string) float64 {
	if ccy == in.Currency {
		return 1
	}
	return in.FXRates[ccy]
}

// Payment is one payment a holding received, in Currency ("" for the
// holding's own), booked to Account.
type Payment struct {
	Day      time.Time
	Amount   float64
	Currency string
	Account  string
	// Quantity is the quantity of the instrument held as the payment's
	// day began, in the units of the instrument at the instant; nil where
	// none is known. Units says it may be in the day's units: no whole
	// factor explains a change since, or no account held the instrument
	// through some span since with its units read. Before is the quantity
	// held RecordLagDays earlier, in the same units.
	Quantity, Before *float64
	Units            bool
	Type             IncomeType
}

// RecordLagDays bound the days from a dividend's record day to its pay
// day. A depositary receipt of a Japanese share pays about three months
// after its record day.
const RecordLagDays = 100

// instrumentPayments are the payments of every holding of h's instrument
// and of its sold positions (instrumentOf).
func (in *Inputs) instrumentPayments(h Holding) []Payment {
	holdings, sold := in.instrumentOf(h)
	var out []Payment
	for _, o := range holdings {
		out = append(out, in.Payments[o.HoldingKey]...)
	}
	for _, k := range sold {
		out = append(out, in.Payments[k]...)
	}
	return out
}

// inOwn is an amount in ccy ("" for the holding's own) in the holding's
// own currency: converted to the output currency at the instant's rate,
// and back at the holding's. false where no rate converts it.
func (in *Inputs) inOwn(h Holding, amount float64, ccy string) (float64, bool) {
	if ccy == "" || ccy == h.Currency {
		return amount, true
	}
	from, own := in.fxOf(ccy), h.FX()
	if from == 0 || own == 0 {
		return 0, false
	}
	return amount * from / own, true
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
