package canonical

import "sort"

// The cashflow vocabulary: the three-level hierarchy the statement
// groups by, and the boundary that says which accounts are the
// household's at all.
//
// Cashflow adds no categorisation tier. It reads the verdicts the
// spending and income families already store and folds them into
// section → class → group, whose leaves are those families' own
// values. The hierarchy exists because the two families' primaries
// cannot draw a cash flow statement: income has one vendored primary,
// so yield cannot be lifted out of it, and taxes hide inside a
// government-and-non-profit bucket beside donations and a passport
// renewal.
//
// WHAT IS HERE AND WHAT IS IN SQL. The resolution — which section,
// class and group a given transaction lands in — is one gold macro,
// not Go: the dashboard needs the same assignment the CLI uses,
// computed after its pickers apply, and a Go-side copy would have to
// be re-implemented in the serving view and would drift. What Go owns
// is the VOCABULARY those values are drawn from (so a config entry can
// be refused before it reaches a report) and the wrapper boundary's
// defaults (which the enrichment pass stamps into gold for the macro
// to read, on the account-scope precedent).
//
// `vehicles` is the section's name rather than `entities`, which the
// returns engine already uses for the thing a return is measured on.
// It is unrelated to canonical.Vehicle, the instrument taxonomy's
// wrapper dimension; the Go types below all carry a Cashflow prefix so
// the two cannot be confused at a call site.

// CashflowSection is the top level: the cash flow statement's own
// sections, plus the split of operating into its two halves.
//
// The statement has four sections and a residual —
// operating + investing + financing + vehicles = change in cash — but
// a NODE has to know which half of operating it belongs to, because
// wages and groceries are two nodes and not one. So operating is
// carried as two values and the summary view sums them back into one
// signed figure.
type CashflowSection string

const (
	SectionOperatingIn  CashflowSection = "operating_in"
	SectionOperatingOut CashflowSection = "operating_out"
	SectionInvesting    CashflowSection = "investing"
	SectionFinancing    CashflowSection = "financing"
	SectionVehicles     CashflowSection = "vehicles"
	// SectionCash is the residual, seen from the pool's side: the four
	// sections summed and negated, so that cash the household kept is
	// cash the pool absorbed. It is computed, never measured, which is
	// what makes the statement close by construction and what the
	// reconciliation memo exists to keep honest.
	SectionCash CashflowSection = "cash"
)

var cashflowSectionValues = map[CashflowSection]struct{}{
	SectionOperatingIn: {}, SectionOperatingOut: {},
	SectionInvesting: {}, SectionFinancing: {},
	SectionVehicles: {}, SectionCash: {},
}

// Valid reports whether the receiver is a recognised section.
func (s CashflowSection) Valid() bool {
	_, ok := cashflowSectionValues[s]
	return ok
}

// CashflowClass is the middle level: the diagram's inner nodes. What
// nature of money on the operating sections, which asset class on
// investing, which liability on financing, which pool on vehicles.
//
// The investing classes are not enumerated here. They are the
// instrument's asset class, every value of it but `cash` — a trade in
// a cash-class instrument is pool-internal — so enumerating them would
// be a second copy of assetClassValues that a new exposure value could
// silently fall out of. The macro reads the asset class directly and
// this type names only the classes cashflow invents.
type CashflowClass string

const (
	// The inflow classes: money from labour, from assets, from
	// entitlements, and everything else that arrived.
	ClassEarnings      CashflowClass = "earnings"
	ClassYield         CashflowClass = "yield"
	ClassBenefits      CashflowClass = "benefits"
	ClassOtherReceipts CashflowClass = "other_receipts"

	// The outflow classes. Fees, taxes and giving are lifted out of
	// spending so a reader sees each on its own line: for a household
	// whose money comes from wealth, the cost of being invested is
	// worth reading beside the tax, and every household-balance diagram
	// lifts taxes and giving.
	ClassConsumption CashflowClass = "consumption"
	ClassFees        CashflowClass = "fees"
	ClassTaxes       CashflowClass = "taxes"
	ClassGiving      CashflowClass = "giving"

	// ClassInvestments is what every asset class folds into under
	// `--investing whole`, the default: the household's investing drawn
	// as one movement.
	ClassInvestments CashflowClass = "investments"
	// ClassElsewhere holds the investing rows with no instrument —
	// capital deployed to, or returned from, a destination the product
	// does not track.
	ClassElsewhere CashflowClass = "elsewhere"

	// The financing classes.
	ClassMortgage CashflowClass = "mortgage"
	ClassLoans    CashflowClass = "loans"

	// The vehicle classes. The first four are the wrapper boundary's
	// own (DefaultWrapperSide); `untracked` is where an own-account
	// move goes whose far account is outside the pool or unknown.
	ClassRetirement CashflowClass = "retirement"
	ClassEducation  CashflowClass = "education"
	ClassHealth     CashflowClass = "health"
	ClassTrusts     CashflowClass = "trusts"
	ClassUntracked  CashflowClass = "untracked"

	// ClassCash is the residual's one class, as it is its one node.
	ClassCash CashflowClass = "cash"
)

// vehicleClassValues are the four pools a tax wrapper can name. The
// stamped wrapper table's `class` column holds one of these or NULL,
// and `untracked` is deliberately absent: no wrapper puts a crossing
// there, only the absence of a far account does.
var vehicleClassValues = map[CashflowClass]struct{}{
	ClassRetirement: {}, ClassEducation: {}, ClassHealth: {}, ClassTrusts: {},
}

// ValidVehicleClass reports whether c is one of the four pools a
// wrapper may be moved to.
func (c CashflowClass) ValidVehicleClass() bool {
	_, ok := vehicleClassValues[c]
	return ok
}

// WrapperSide is which side of the household boundary a tax wrapper
// sits on, and it is decided by two questions asked in order: is the
// money still the household's, and is it earmarked for a purpose or a
// stage of life the household does not treat as current money?
//
// Reversibility is deliberately NOT the test. A retirement account can
// be drawn on at any time with a penalty and nobody counts it as this
// month's money; an education plan and a health savings account are
// the same in kind. What separates them from a current account is the
// earmark, not the lock.
type WrapperSide string

const (
	// SideHousehold is the household itself: a crossing to an account
	// in such a wrapper is pool-internal and draws nothing.
	SideHousehold WrapperSide = "household"
	// SideVehicle is an earmarked pool the household pays into and
	// draws on rather than spends from. A crossing is a `vehicles`
	// line, in the row's own direction, and the wrapper's class says
	// which pool.
	SideVehicle WrapperSide = "vehicle"
	// SideGiving is money given away. The crossing is DIRECTIONAL and
	// asymmetric: an outgoing leg is `operating_out · Giving`, because
	// the transfer is irrevocable and the vehicle's later grants are
	// its own; an incoming leg is `operating_in · Other receipts`,
	// because a charitable remainder trust's annuity or a custodial
	// account drawn for the minor's costs is cash arriving and none of
	// it is negative giving.
	SideGiving WrapperSide = "giving"
)

var wrapperSideValues = map[WrapperSide]struct{}{
	SideHousehold: {}, SideVehicle: {}, SideGiving: {},
}

// Valid reports whether the receiver is a recognised side.
func (s WrapperSide) Valid() bool {
	_, ok := wrapperSideValues[s]
	return ok
}

// defaultWrapperSides is the engine's household boundary, drawn by tax
// wrapper. Every value of the TaxWrapper enum appears exactly once —
// TestDefaultWrapperSideCoversEveryWrapper holds it to that, so a
// wrapper added for a new jurisdiction cannot arrive without a side.
//
// An education plan and a custodial account for the same child land on
// different sides, which is the two-question test doing its work: the
// plan's owner can revoke it and take the money back, so it is the
// household's capital in an earmarked pool, while a custodial account
// is legally the minor's the moment it is funded, so funding it is a
// gift. A grantor trust is the household outright — tax-transparent,
// and as a rule revocable.
//
// `other` is on the household side because the product cannot know
// better. The enum covers two jurisdictions; a pension, insurance or
// company wrapper from a third can only be `other` today, and its
// contributions read as pool-internal. The remedy is a new enum value,
// not a per-wrapper override, which cannot tell two `other` accounts
// apart.
var defaultWrapperSides = map[TaxWrapper]struct {
	side  WrapperSide
	class CashflowClass
}{
	// The household: what is neither given away nor earmarked.
	TaxWrapperTaxablePersonal: {SideHousehold, ""},
	TaxWrapperTaxableJoint:    {SideHousehold, ""},
	TaxWrapperTrustGrantor:    {SideHousehold, ""},
	TaxWrapperOther:           {SideHousehold, ""},

	// Retirement, in both jurisdictions the enum covers.
	TaxWrapperTraditionalIRA: {SideVehicle, ClassRetirement},
	TaxWrapperRothIRA:        {SideVehicle, ClassRetirement},
	TaxWrapperSEPIRA:         {SideVehicle, ClassRetirement},
	TaxWrapperSIMPLEIRA:      {SideVehicle, ClassRetirement},
	TaxWrapper401k:           {SideVehicle, ClassRetirement},
	TaxWrapper403b:           {SideVehicle, ClassRetirement},
	TaxWrapper457b:           {SideVehicle, ClassRetirement},
	TaxWrapperPillar2:        {SideVehicle, ClassRetirement},
	TaxWrapperVestedBenefits: {SideVehicle, ClassRetirement},
	TaxWrapperPillar3a:       {SideVehicle, ClassRetirement},

	// Education and health: earmarked for a cost, not for spending.
	TaxWrapper529:          {SideVehicle, ClassEducation},
	TaxWrapperCoverdellESA: {SideVehicle, ClassEducation},
	TaxWrapperHSA:          {SideVehicle, ClassHealth},

	// A non-grantor trust is a separate taxpayer.
	TaxWrapperTrustNonGrantor: {SideVehicle, ClassTrusts},

	// Given away: nothing comes back by right.
	TaxWrapperCharitable:      {SideGiving, ""},
	TaxWrapperTrustCharitable: {SideGiving, ""},
	TaxWrapperFoundation:      {SideGiving, ""},
	TaxWrapperCustodialUTMA:   {SideGiving, ""},
	TaxWrapperCustodialUGMA:   {SideGiving, ""},
}

// DefaultWrapperSide is the engine's answer for one wrapper: which
// side of the household boundary it sits on, and — on the vehicle side
// alone — which pool. A wrapper the enum does not hold reads as
// household, which is the same direction an UNSET wrapper reads in
// (§3 of docs/CASHFLOW.md): the safe one, because nothing is
// silently removed from the statement. It is not free, and it is why
// `status -v` counts pooled accounts with no wrapper set.
//
// One place decides a wrapper's side. The config's `cashflow.wrappers`
// composes an override map over this, and the enrichment pass stamps
// the result into gold; nothing re-derives the boundary anywhere else.
func DefaultWrapperSide(w TaxWrapper) (WrapperSide, CashflowClass) {
	if d, ok := defaultWrapperSides[w]; ok {
		return d.side, d.class
	}
	return SideHousehold, ""
}

// WrapperBoundary is one row of the engine's household boundary: a tax
// wrapper, the side it sits on, and — on the vehicle side alone — the
// pool it names.
type WrapperBoundary struct {
	Wrapper TaxWrapper
	Side    WrapperSide
	Class   CashflowClass
}

// WrapperBoundaries returns the whole boundary, one row per tax
// wrapper, ordered by wrapper so a stamp is byte-stable across runs.
//
// It exists so the enrichment pass can write the boundary into gold
// without enumerating the enum itself. Stamping every wrapper rather
// than only the overridden ones is what keeps DefaultWrapperSide the
// single place a wrapper's side is decided: the alternative would put
// the defaults in SQL a second time, where a wrapper added for a new
// jurisdiction would take two edits to reach the statement.
func WrapperBoundaries() []WrapperBoundary {
	out := make([]WrapperBoundary, 0, len(defaultWrapperSides))
	for w, d := range defaultWrapperSides {
		out = append(out, WrapperBoundary{w, d.side, d.class})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Wrapper < out[j].Wrapper })
	return out
}

// The destinations `cashflow.wrappers` may name — where a crossing to
// an account in that wrapper lands. They are spelled as the reader
// thinks of them, one word per place money goes, rather than as the
// (side, class) pair gold stores: `household` means no crossing at all,
// `giving` is the irrevocable transfer, and the four pools name
// themselves.
const (
	WrapperDestHousehold  = "household"
	WrapperDestRetirement = "retirement"
	WrapperDestEducation  = "education"
	WrapperDestHealth     = "health"
	WrapperDestTrusts     = "trusts"
	WrapperDestGiving     = "giving"
)

// WrapperDestinations is the destination vocabulary in the order an
// error message should list it, so a rejected entry can be fixed from
// the message alone.
var WrapperDestinations = []string{
	WrapperDestHousehold, WrapperDestRetirement, WrapperDestEducation,
	WrapperDestHealth, WrapperDestTrusts, WrapperDestGiving,
}

// ParseWrapperDestination turns a configured destination into the
// (side, class) pair gold stores, reporting whether the word names one
// at all. `untracked` is refused with every other unknown word: it is
// where a crossing goes when there is no far account to read a wrapper
// off, so a wrapper cannot be moved to it.
func ParseWrapperDestination(s string) (WrapperSide, CashflowClass, bool) {
	switch s {
	case WrapperDestHousehold:
		return SideHousehold, "", true
	case WrapperDestGiving:
		return SideGiving, "", true
	case WrapperDestRetirement:
		return SideVehicle, ClassRetirement, true
	case WrapperDestEducation:
		return SideVehicle, ClassEducation, true
	case WrapperDestHealth:
		return SideVehicle, ClassHealth, true
	case WrapperDestTrusts:
		return SideVehicle, ClassTrusts, true
	}
	return "", "", false
}

// VehicleTransferDetailed is the delta that stands for a crossing to a
// vehicle whose far side the product does not hold, for each of the
// four pools. It is the bridge between the wrapper boundary and the
// taxonomy: a rule places the value, and the resolution reads the value
// back as the class it names.
//
// Reported per class rather than as a lookup table so that a class
// without a delta — `untracked`, which no rule can place — is a
// compile-time-visible absence rather than a missing map entry.
func VehicleTransferDetailed(c CashflowClass) (string, bool) {
	switch c {
	case ClassRetirement:
		return DetailedRetirementTransfer, true
	case ClassEducation:
		return DetailedEducationTransfer, true
	case ClassHealth:
		return DetailedHealthTransfer, true
	case ClassTrusts:
		return DetailedTrustTransfer, true
	}
	return "", false
}
