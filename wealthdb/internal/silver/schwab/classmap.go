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
//   - COLLECTIVE_INVESTMENT + EXCHANGE_TRADED_FUND → (RefineETFExposure(name), etf)
//   - COLLECTIVE_INVESTMENT (other) / MUTUAL_FUND  → (RefineETFExposure(name), fund)
//   - ETF (the standalone assetType enum value)  → (RefineETFExposure(name), etf)
//   - BOND / FIXED_INCOME                        → (fixed_income, bond)
//   - OPTION                                     → (public_equity, option)  [equity underlying]
//   - FUTURE                                     → (public_equity, future)  [equity underlying]
//   - anything else / empty                      → (other, other)
//
// RefineETFExposure reads the underlying exposure (crypto / metal /
// fixed_income / else public_equity) from the fund/ETF's security
// name — the wrapper (etf vs fund) is fixed by the assetType here,
// only the exposure is name-derived. Name-shy products are corrected
// via instrument_overrides. Every pair returned satisfies
// canonical.ValidTaxonomyPair.
func taxonomyFor(rawAssetType, rawInstrumentType, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch rawAssetType {
	case "EQUITY":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "ETF":
		return silver.RefineETFExposure(name), canonical.VehicleETF
	case "MUTUAL_FUND":
		return fundExposure(name), canonical.VehicleFund
	case "COLLECTIVE_INVESTMENT":
		if rawInstrumentType == "EXCHANGE_TRADED_FUND" {
			return silver.RefineETFExposure(name), canonical.VehicleETF
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

// fundExposure reads the exposure of a (non-exchange-traded) fund from
// its name, routing money-market funds to cash — a purchased money
// fund reaches the MUTUAL_FUND / COLLECTIVE_INVESTMENT path (Schwab's
// cash sweep is CASH_EQUIVALENT, filtered earlier), and it is a cash
// equivalent, not the exposure RefineETFExposure would guess from a
// bond keyword in the fund name.
func fundExposure(name string) canonical.AssetClass {
	if silver.NamesMoneyMarket(name) {
		return canonical.AssetClassCash
	}
	return silver.RefineETFExposure(name)
}

// Instrument-key and description shapes for the statement-history
// classifier. Ported from fidelity's classifyHistoricalPair (that
// source's statement PDFs share Schwab's lack of a structured type
// code, so shape heuristics are all there is), pared to the shapes
// Schwab statements actually surface: a CUSIP for bonds/options, an
// OCC option symbol, a coupon-bearing fixed-income line, a word-ETF
// or money-market description, and the 4-letter-plus-X mutual-fund
// ticker. First match wins; the fall-through is a plain stock, the
// overwhelming majority of statement lines.
// The shared, cross-adapter shapes (option / CUSIP / bond-coupon /
// money-market ticker / ETF / mutual-fund / ETF-issuer) live in
// silver.Stmt*Re. Only Schwab's own money-market description shape,
// which lacks fidelity's "NET CASH POSITION" arm, is declared here.
//
// Money-market sweeps ("… GOVERNMENT MONEY MARKET", "… CASH RESERVES").
var histMoneyMktRe = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b`)

// taxonomyHistorical derives the (exposure, vehicle) pair for a
// `historical_position_snapshots` row from its statement section,
// instrument key, and description; the api side later overwrites the
// instrument dimension whenever the same instrument reappears with a
// source-classified value. Money-market
// funds are cash × fund (TAXONOMY.md §5.8); a word-ETF / mutual-fund
// line takes its exposure from RefineETFExposure(description) inside
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
		return silver.RefineETFExposure(description), canonical.VehicleETF
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
	case silver.StmtCUSIPRe.MatchString(instrumentKey),
		silver.StmtBondDescRe.MatchString(description):
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case silver.StmtETFDescRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	case silver.StmtMutualFundRe.MatchString(instrumentKey):
		if silver.NamesMoneyMarket(description) {
			return canonical.AssetClassCash, canonical.VehicleFund
		}
		return silver.RefineETFExposure(description), canonical.VehicleFund
	case silver.StmtETFIssuerRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	}
	return canonical.AssetClassPublicEquity, canonical.VehicleStock
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

// taxWrapperFor maps the verbatim Schwab statement registration
// label (silver column `accounts.account_registration` populated
// by schwab-web from statement-PDF parsing — see that
// repo's silver migration 0003) to the canonical TaxWrapper
// enum. The Trader API doesn't expose this; statement PDFs are
// the only source on the Schwab side.
//
// Known labels harvested from observed statement sets;
// extend as new variants surface (a future load run with an
// unseen label leaves TaxWrapper nil, which renders as
// taxable_personal — the safe default).
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
	// don't typically live at Schwab retail (Schwab routes
	// 529s through state plans), so a future "529 College
	// Savings Plan" label would need adding.
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
