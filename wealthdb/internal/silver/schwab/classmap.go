package schwab

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// taxonomyFor returns the single (exposure, vehicle) pair
// (TAXONOMY.md) emitted to gold for a live api position, from
// Schwab's `instrument.assetType` + `instrument.type` signal.
// Schwab's Trader API reports ETFs as assetType
// COLLECTIVE_INVESTMENT with the ETF-ness one level down in
// `instrument.type` (EXCHANGE_TRADED_FUND) — the standalone "ETF"
// assetType exists in the API's enum but real position dumps don't
// use it. CASH_EQUIVALENT and CURRENCY rows never reach this helper —
// they are routed to cash_balances by snapshots.go first.
//
//   - EQUITY                                     → (public_equity, stock)
//   - COLLECTIVE_INVESTMENT + EXCHANGE_TRADED_FUND → (etfExposure(name), etf)
//   - COLLECTIVE_INVESTMENT (other) / MUTUAL_FUND  → (fundExposure(name), fund)
//   - ETF (the standalone assetType enum value)  → (etfExposure(name), etf)
//   - BOND / FIXED_INCOME                        → (fixed_income, bond)
//   - OPTION                                     → (public_equity, option)  [equity underlying]
//   - FUTURE                                     → (public_equity, future)  [equity underlying]
//   - anything else / empty                      → (other, other)
//
// etfExposure reads the underlying exposure (crypto / metal /
// fixed_income / else public_equity) from the fund/ETF's security
// name — the wrapper (etf vs fund) is fixed by the assetType here,
// only the exposure is name-derived. Gold's class pass reads
// OpenFIGI's type of the security over this pair (RefineClass), and
// instrument_overrides over both. Every pair returned satisfies
// canonical.ValidTaxonomyPair.
func taxonomyFor(rawAssetType, rawInstrumentType, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch rawAssetType {
	case "EQUITY":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "ETF":
		return etfExposure(name), canonical.VehicleETF
	case "MUTUAL_FUND":
		return fundExposure(name), canonical.VehicleFund
	case "COLLECTIVE_INVESTMENT":
		if rawInstrumentType == "EXCHANGE_TRADED_FUND" {
			return etfExposure(name), canonical.VehicleETF
		}
		return fundExposure(name), canonical.VehicleFund
	case "BOND", "FIXED_INCOME":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "OPTION":
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case "FUTURE":
		return canonical.AssetClassPublicEquity, canonical.VehicleFuture
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// bondAbbrevRe matches the bond words Schwab abbreviates in a fund's
// description ("TOTAL BD MKT", "SHORT-TERM TREAS", "INFL PROT"), which
// silver.RefineETFExposure reads only spelled out.
var bondAbbrevRe = regexp.MustCompile(`(?i)\bBN?D\b|\bTREAS\b|\bGOVT\b|\bINFL(?:ATION)?[- ]PROT`)

// etfExposure is an ETF's exposure read from its name: RefineETFExposure,
// with Schwab's abbreviated bond words read as fixed income.
func etfExposure(name string) canonical.AssetClass {
	return bondByAbbrev(silver.RefineETFExposure(name), name)
}

// fundExposure is the same reading for a fund that is not exchange
// traded, a money-market fund's name read as cash first (FundExposure).
func fundExposure(name string) canonical.AssetClass {
	return bondByAbbrev(silver.FundExposure(name), name)
}

// bondByAbbrev reads a fund that the spelled-out name patterns leave as
// public equity as fixed income where its name holds an abbreviated
// bond word.
func bondByAbbrev(ac canonical.AssetClass, name string) canonical.AssetClass {
	if ac == canonical.AssetClassPublicEquity && bondAbbrevRe.MatchString(name) {
		return canonical.AssetClassFixedIncome
	}
	return ac
}

// RefineClass implements silver.ClassRefiner: gold's class pass reads
// OpenFIGI's type of a security over the pair the adapter gave it
// (docs/adapters/schwab.md §4.2). The precedence is the lookup's type
// where gold holds one, then Schwab's own instrument type, then the
// name:
//
//   - A bond's type (Govt, Corp, Muni, …) makes it (fixed_income, bond).
//   - A share's type (Common Stock, Depositary Receipt, REIT, …) makes
//     it (public_equity, stock).
//   - A fund's type keeps an ETF or a fund as Schwab types it, an ETP's
//     making a fund an ETF. Anything else becomes a fund (an ETP an
//     ETF) with its exposure read from its name: a trust Schwab types as
//     an equity, say.
//   - Any other type (an option, a warrant, an index) leaves the pair.
//
// A pair the taxonomy does not admit leaves the pair too.
func (*Adapter) RefineClass(securityType string, ac canonical.AssetClass, v canonical.Vehicle,
	name string) (canonical.AssetClass, canonical.Vehicle) {
	refined, rv := ac, v
	switch figi := canonical.OpenFIGIVehicle(securityType); figi {
	case canonical.VehicleBond:
		refined, rv = canonical.AssetClassFixedIncome, canonical.VehicleBond
	case canonical.VehicleStock:
		refined, rv = canonical.AssetClassPublicEquity, canonical.VehicleStock
	case canonical.VehicleETF, canonical.VehicleFund:
		switch {
		case v == canonical.VehicleETF:
			// OpenFIGI types an ETF "Mutual Fund": Schwab's type is the
			// finer one.
		case v == canonical.VehicleFund:
			rv = figi
		case figi == canonical.VehicleETF:
			refined, rv = etfExposure(name), canonical.VehicleETF
		default:
			refined, rv = fundExposure(name), canonical.VehicleFund
		}
	}
	if !canonical.ValidTaxonomyPair(refined, rv) {
		return ac, v
	}
	return refined, rv
}

// Instrument-key and description shapes for the statement-history
// classifier. Schwab statements carry no structured type code, so
// shape heuristics are all there is: a CUSIP for bonds/options, an
// OCC option symbol, a coupon-bearing fixed-income line, a word-ETF
// or money-market description, and the 4-letter-plus-X mutual-fund
// ticker. First match wins; the fall-through is a plain stock, the
// overwhelming majority of statement lines.
//
// The shapes shared with fidelity (option / CUSIP / bond-coupon /
// money-market ticker / ETF / mutual-fund / ETF-issuer) live in
// silver.Stmt*Re. Only Schwab's own money-market description shape,
// which lacks fidelity's "NET CASH POSITION" arm, is declared here.
//
// histMoneyMktRe matches money-market sweeps ("… GOVERNMENT MONEY
// MARKET", "… CASH RESERVES").
var histMoneyMktRe = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b`)

// taxonomyHistorical derives the (exposure, vehicle) pair for a
// `historical_position_snapshots` row from its statement section,
// instrument key, and description; the api side later overwrites the
// instrument dimension whenever the same instrument reappears with a
// source-classified value. Money-market
// funds are cash × fund (TAXONOMY.md §5.8); a word-ETF / mutual-fund
// line takes its exposure from etfExposure(description) inside
// the etf / fund vehicle. Every returned pair satisfies
// canonical.ValidTaxonomyPair; instrument_overrides remains the
// escape hatch for shapes these heuristics misjudge.
func taxonomyHistorical(section, instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	// The statement `section` is the authoritative wrapper signal —
	// it pins the vehicle where the row's name/key shape can't (a
	// name-shy ETF like "iShares Core U.S. Aggregate Bond", no "ETF"
	// token, would otherwise fall through to stock and disagree with
	// the api path). Exposure still comes from the name inside a fund
	// wrapper. Sections without a clean vehicle mapping ("Investments",
	// "Other Assets", empty) fall back to the key/description shapes.
	switch section {
	case "Equities":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "Exchange Traded Funds":
		return etfExposure(description), canonical.VehicleETF
	case "Fixed Income":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "Options":
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	}
	return taxonomyHistoricalByShape(instrumentKey, description)
}

func taxonomyHistoricalByShape(instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	switch {
	case silver.StmtOptionKeyRe.MatchString(instrumentKey),
		silver.StmtOptionDescRe.MatchString(description):
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case histMoneyMktRe.MatchString(description),
		silver.StmtMoneyMktKeyRe.MatchString(instrumentKey):
		return canonical.AssetClassCash, canonical.VehicleFund
	}
	ac, v := silver.StatementShapePair(instrumentKey, description)
	if v == canonical.VehicleETF || v == canonical.VehicleFund {
		ac = bondByAbbrev(ac, description)
	}
	return ac, v
}

// isCashAssetType reports whether a Schwab position's assetType
// indicates it should be projected as a cash balance rather than
// a security position.
func isCashAssetType(rawAssetType string) bool {
	switch rawAssetType {
	case "CASH_EQUIVALENT", "CURRENCY":
		return true
	default:
		return false
	}
}

// schwabRegistrationToWrapper maps the verbatim Schwab statement
// registration label (silver column `accounts.account_registration`,
// parsed by schwab-web from statement PDFs; that collector's silver
// migration 0003) to the canonical TaxWrapper enum. The Trader API
// doesn't expose this; statement PDFs are the only source on the
// Schwab side.
//
// An unknown label leaves TaxWrapper nil, which renders as
// taxable_personal — the safe default.
var schwabRegistrationToWrapper = map[string]canonical.TaxWrapper{
	// Taxable personal brokerage variants.
	"Schwab One International® Account": canonical.TaxWrapperTaxablePersonal,
	"Schwab One® Account":               canonical.TaxWrapperTaxablePersonal,
	"Brokerage Account":                 canonical.TaxWrapperTaxablePersonal,

	// IRA variants. Schwab's "Contributory IRA" is what they
	// call a regular traditional IRA you contribute to (as
	// opposed to a Rollover IRA, which they label separately).
	"Contributory IRA":      canonical.TaxWrapperTraditionalIRA,
	"Rollover IRA":          canonical.TaxWrapperTraditionalIRA,
	"Traditional IRA":       canonical.TaxWrapperTraditionalIRA,
	"Inherited IRA":         canonical.TaxWrapperTraditionalIRA,
	"Roth IRA":              canonical.TaxWrapperRothIRA,
	"Roth Contributory IRA": canonical.TaxWrapperRothIRA,
	"Inherited Roth IRA":    canonical.TaxWrapperRothIRA,
	"SEP-IRA":               canonical.TaxWrapperSEPIRA,
	"SIMPLE IRA":            canonical.TaxWrapperSIMPLEIRA,

	// Education savings. Schwab uses the bare "Education
	// Savings" label for Coverdell ESAs on statements; 529s
	// usually sit with state plans rather than Schwab retail,
	// and their label maps in case a statement prints it.
	"Education Savings":        canonical.TaxWrapperCoverdellESA,
	"Coverdell ESA":            canonical.TaxWrapperCoverdellESA,
	"529 College Savings Plan": canonical.TaxWrapper529,

	// Custodial-for-minors. Schwab's per-state language varies
	// but the canonical taxonomy collapses to UTMA / UGMA.
	"Schwab One® Custodial Account (UTMA)": canonical.TaxWrapperCustodialUTMA,
	"Schwab One® Custodial Account (UGMA)": canonical.TaxWrapperCustodialUGMA,
	"Custodial Account (UTMA)":             canonical.TaxWrapperCustodialUTMA,
	"Custodial Account (UGMA)":             canonical.TaxWrapperCustodialUGMA,

	// Self-employed retirement.
	"Solo 401(k)":       canonical.TaxWrapper401k,
	"Individual 401(k)": canonical.TaxWrapper401k,

	// Trusts. Schwab labels trust accounts generically; the
	// grantor / non-grantor distinction isn't in the statement
	// header. Default to non-grantor (the more common case for
	// retail Schwab trusts); override via config for grantor
	// trusts if needed.
	"Trust Account": canonical.TaxWrapperTrustNonGrantor,
}

// taxWrapperForRegistration returns the canonical TaxWrapper
// for a Schwab statement registration label, or "" if the label
// is unrecognised or empty.
func taxWrapperForRegistration(label string) canonical.TaxWrapper {
	if w, ok := schwabRegistrationToWrapper[label]; ok {
		return w
	}
	return ""
}
