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
	"maps"
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
	// instrument a holding pools (instrumentOf), its amount on those days
	// in the positions that are no holding at the instant, such as an
	// account's that sold it: one Daily per currency of those positions.
	Daily       map[HoldingKey]Daily
	FormerDaily map[InstrumentKey][]Daily
	// Held are the accounts whose positions held each instrument at some
	// snapshot to the instant, sorted. HeldSecurity is the security key of
	// each, where it has one, as a holding's (SecurityID). Both name an
	// instrument by the id the matching folds it into (Fold).
	Held         map[InstrumentKey][]string
	HeldSecurity map[InstrumentKey]string
	// Aliases are the ids a source keys an instrument by that the matching
	// reads as another (Fold): a holding's alias for one account or for
	// every account of the source (Account ""), and the CUSIP id of an
	// instrument its source also keys by its ticker, which reads as the
	// ticker.
	Aliases map[AliasKey]string
	// pools are the holdings of each instrument (poolKey), bySecurity the
	// instruments of each security Held names, sorted, and folds each
	// holding's instrument as the matching reads it, by its place in
	// Holdings: what Index fills.
	pools      map[string][]Holding
	bySecurity map[string][]InstrumentKey
	folds      []string
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

// AliasKey names an id an account of a source keys an instrument by;
// Account "" names it for every account of the source.
type AliasKey struct{ Source, Account, ID string }

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

// Daily is a holding's amount on each of the Days of the twelve months
// to the instant: the sum of its spans. A day no span covers was not
// held. Currency is the currency of the amounts: a holding's own, or that
// of the positions a FormerDaily sums; "" where none is stated, which
// reads as that of a holding of the instrument.
type Daily struct {
	Days     int
	Currency string
	Spans    []Span
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

// Fold is the id the transactions and the holdings meet by, of an
// instrument account acct of source src keys by iid: iid's alias for the
// account, else for every account of the source (Aliases), else iid
// itself. A holding keeps its own id (Instrument): only the matching
// folds.
func (in *Inputs) Fold(src, acct, iid string) string {
	if len(in.Aliases) == 0 || iid == "" {
		return iid
	}
	if id, ok := in.Aliases[AliasKey{src, acct, iid}]; ok {
		return id
	}
	if id, ok := in.Aliases[AliasKey{src, "", iid}]; ok {
		return id
	}
	return iid
}

// instrumentKey is a holding's instrument as the matching reads it
// (Fold).
func (in *Inputs) instrumentKey(h Holding) InstrumentKey {
	return InstrumentKey{h.Source, in.Fold(h.Source, h.Account, h.Instrument)}
}

// holdingFold is holding i's instrument as the matching reads it
// (instrumentKey): Index's, else folded as it is read.
func (in *Inputs) holdingFold(i int) string {
	if i < len(in.folds) {
		return in.folds[i]
	}
	h := &in.Holdings[i]
	return in.Fold(h.Source, h.Account, h.Instrument)
}

// HoldingOf is the holding a transaction belongs to: the holding of the
// instrument it names in its account. The two meet where the line's id
// folds into the holding's (Fold), under the line's account, or, for a
// holding of the line's portfolio, under the holding's: an account's own
// alias covers its own lines, and the lines its portfolio's other
// accounts book for it. An account that held the
// instrument once and holds none of it now has none: its transactions
// are its sold position's (Sold). Any other account's belong to the first
// holding of the instrument in the account's portfolio: a bank books a
// dividend to the cash account beside the custody account that holds the
// security. Where none of the portfolio's accounts holds it but one held
// it once, they are the sold position's too. Else they belong to the
// first holding of it at its source.
func (in *Inputs) HoldingOf(src, acct, iid string) (HoldingKey, bool) {
	if iid == "" {
		return HoldingKey{}, false
	}
	id, wide := in.Fold(src, acct, iid), in.Fold(src, "", iid)
	var sibling, first *HoldingKey
	portfolio := in.Accounts[AccountKey{src, acct}].Portfolio
	for i := range in.Holdings {
		h := &in.Holdings[i]
		if h.Source != src {
			continue
		}
		sib := portfolio != "" && in.Accounts[AccountKey{h.Source, h.Account}].Portfolio == portfolio
		if f := in.holdingFold(i); f != id && !(sib && f == in.foldUnder(src, h.Account, iid, wide)) {
			continue
		}
		if h.Account == acct {
			return h.HoldingKey, true
		}
		if sibling == nil && sib {
			sibling = &h.HoldingKey
		}
		if first == nil {
			first = &h.HoldingKey
		}
	}
	if slices.Contains(in.Held[InstrumentKey{src, id}], acct) {
		return HoldingKey{}, false
	}
	if sibling != nil {
		return *sibling, true
	}
	if _, held := in.portfolioHeld(src, acct, iid); held || first == nil {
		return HoldingKey{}, false
	}
	return *first, true
}

// foldUnder is Fold(src, acct, iid), given wide, iid's fold for every
// account of the source: one lookup.
func (in *Inputs) foldUnder(src, acct, iid, wide string) string {
	if id, ok := in.Aliases[AliasKey{src, acct, iid}]; ok {
		return id
	}
	return wide
}

// portfolioHeld is the id under which an account of acct's portfolio
// held the instrument iid names once (Held), iid folded under that
// account (Fold): its fold under acct first, else the least. false where
// none did.
func (in *Inputs) portfolioHeld(src, acct, iid string) (string, bool) {
	portfolio := in.Accounts[AccountKey{src, acct}].Portfolio
	if portfolio == "" {
		return "", false
	}
	own, wide := in.Fold(src, acct, iid), in.Fold(src, "", iid)
	found := ""
	for a, x := range in.Accounts {
		if a.Source != src || x.Portfolio != portfolio {
			continue
		}
		if id := in.foldUnder(src, a.Account, iid, wide); slices.Contains(in.Held[InstrumentKey{src, id}], a.Account) {
			if id == own {
				return own, true
			}
			if found == "" || id < found {
				found = id
			}
		}
	}
	return found, found != ""
}

// Sold is the key of the position an account sold of an instrument, as
// `income` names a holding no longer held: its account and the
// instrument, by the id the matching folds it into (Fold), under the
// account or, where an account of its portfolio held it, under that
// account. Where neither the account nor its portfolio ever held the
// instrument (Held), and one account of its source alone did, it is that
// account's: a bank may book a dividend in another portfolio, as the
// units reader reads its quantity. false where none of these held it. It
// holds the transactions HoldingOf gives no holding.
func (in *Inputs) Sold(src, acct, iid string) (HoldingKey, bool) {
	if iid == "" {
		return HoldingKey{}, false
	}
	id := in.Fold(src, acct, iid)
	accts := in.Held[InstrumentKey{src, id}]
	if slices.Contains(accts, acct) {
		return HoldingKey{src, acct, id}, true
	}
	if held, ok := in.portfolioHeld(src, acct, iid); ok {
		return HoldingKey{src, acct, held}, true
	}
	if len(accts) == 1 {
		return HoldingKey{src, accts[0], id}, true
	}
	return HoldingKey{src, acct, id}, false
}

// poolKey names the holdings a holding's instrument pools with: those of
// its security (SecurityID), which names one security at every source,
// else those of its instrument at its source (instrumentKey); "" for a
// holding of no instrument.
func (in *Inputs) poolKey(h Holding) string {
	switch {
	case h.SecurityID != "":
		return "security\x00" + h.SecurityID
	case h.Instrument != "":
		ik := in.instrumentKey(h)
		return "instrument\x00" + ik.Source + "\x00" + ik.Instrument
	}
	return ""
}

// Index folds each holding's instrument (holdingFold), pools the holdings
// by their instruments (poolKey), and lists the instruments of each
// security Held names (HeldSecurity), for HoldingOf and instrumentOf.
// Compute calls it first; a reader that matches many lines calls it once
// it has read the holdings. After a change of Holdings, the matching
// reads the folds of the last Index until the next one.
func (in *Inputs) Index() {
	in.folds = make([]string, len(in.Holdings))
	for i, h := range in.Holdings {
		in.folds[i] = in.Fold(h.Source, h.Account, h.Instrument)
	}
	in.pools = map[string][]Holding{}
	for _, h := range in.Holdings {
		if k := in.poolKey(h); k != "" {
			in.pools[k] = append(in.pools[k], h)
		}
	}
	in.bySecurity = map[string][]InstrumentKey{}
	for _, ik := range slices.SortedFunc(maps.Keys(in.HeldSecurity), compareInstruments) {
		s := in.HeldSecurity[ik]
		in.bySecurity[s] = append(in.bySecurity[s], ik)
	}
}

// pooled are the holdings of pool key k and the instruments Held names of
// security s (bySecurity), sorted: Index's, else read from the inputs.
func (in *Inputs) pooled(k, s string) (holdings []Holding, security []InstrumentKey) {
	if in.pools != nil {
		return in.pools[k], in.bySecurity[s]
	}
	for _, h := range in.Holdings {
		if in.poolKey(h) == k {
			holdings = append(holdings, h)
		}
	}
	for _, ik := range slices.SortedFunc(maps.Keys(in.HeldSecurity), compareInstruments) {
		if s != "" && in.HeldSecurity[ik] == s {
			security = append(security, ik)
		}
	}
	return holdings, security
}

// instrumentOf is what a holding's instrument pools (poolKey). Its
// holdings are every holding of the instrument, at any source. Its
// instruments are those of the holdings, and with a security every one
// Held names of it at any source, each by the id the matching folds it
// into (instrumentKey). Its sold positions are those of the instruments
// (Sold): of the accounts, and the other accounts of their portfolios,
// that held one once and hold none of it. So a security moved from one
// source to another pools what the first paid and held.
func (in *Inputs) instrumentOf(h Holding) (holdings []Holding, instruments []InstrumentKey, sold []HoldingKey) {
	k := in.poolKey(h)
	if k == "" {
		return []Holding{h}, nil, nil
	}
	holdings, security := in.pooled(k, h.SecurityID)
	holds := map[InstrumentKey]map[string]bool{}
	for _, o := range holdings {
		ik := in.instrumentKey(o)
		if holds[ik] == nil {
			holds[ik], instruments = map[string]bool{}, append(instruments, ik)
		}
		holds[ik][o.Account] = true
	}
	for _, ik := range security {
		if holds[ik] == nil {
			instruments = append(instruments, ik)
		}
	}
	for _, ik := range instruments {
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
	return holdings, instruments, sold
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

// paymentKeys are the holdings of h's instrument and its sold positions
// (instrumentOf), whose payments are the instrument's: a payment is of
// its key's source.
func (in *Inputs) paymentKeys(h Holding) []HoldingKey {
	holdings, _, sold := in.instrumentOf(h)
	keys := make([]HoldingKey, 0, len(holdings)+len(sold))
	for _, o := range holdings {
		keys = append(keys, o.HoldingKey)
	}
	return append(keys, sold...)
}

// instrumentPayments are the payments of every holding of h's instrument
// and of its sold positions (paymentKeys).
func (in *Inputs) instrumentPayments(h Holding) []Payment {
	var out []Payment
	for _, k := range in.paymentKeys(h) {
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
