package schwab

import (
	"regexp"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// classMap maps Schwab's `instrument.assetType` value to the
// canonical AssetClass. Unrecognised values fall through to
// `other` per docs/DESIGN.md §6.8.
var classMap = map[string]canonical.AssetClass{
	"EQUITY":                canonical.AssetClassEquity,
	"ETF":                   canonical.AssetClassETF,
	"MUTUAL_FUND":           canonical.AssetClassFund,
	"COLLECTIVE_INVESTMENT": canonical.AssetClassFund,
	"BOND":                  canonical.AssetClassBond,
	"FIXED_INCOME":          canonical.AssetClassBond,
	"OPTION":                canonical.AssetClassOption,
	"FUTURE":                canonical.AssetClassFuture,
	// CASH_EQUIVALENT and CURRENCY don't appear here — those rows
	// are routed to cash_balances by snapshots.go before this map
	// is consulted.
}

// assetClassFor returns the canonical class for the given Schwab
// assetType + instrument type pair. Schwab's Trader API reports
// ETFs as assetType COLLECTIVE_INVESTMENT with the ETF-ness one
// level down in `instrument.type` (EXCHANGE_TRADED_FUND) — the
// standalone "ETF" assetType exists in the API's enum but real
// position dumps don't use it. Both spellings map to `etf`;
// COLLECTIVE_INVESTMENT with any other instrument type stays in
// the coarse `fund` bucket. Empty / unknown assetTypes become
// AssetClassOther; the raw values are preserved in the position's
// payload.
func assetClassFor(rawAssetType, rawInstrumentType string) canonical.AssetClass {
	if rawAssetType == "COLLECTIVE_INVESTMENT" &&
		rawInstrumentType == "EXCHANGE_TRADED_FUND" {
		return canonical.AssetClassETF
	}
	if c, ok := classMap[rawAssetType]; ok {
		return c
	}
	return canonical.AssetClassOther
}

// taxonomyFor is the 2-D-taxonomy counterpart of assetClassFor for
// live api positions: it returns the (exposure, vehicle) pair
// (TAXONOMY.md) that double-writes alongside the legacy 1-D
// AssetClass. assetClassFor stays the control column; this decides
// asset_class_new + vehicle. Same Schwab assetType/type signal, split
// across the two dimensions:
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
		return silver.RefineETFExposure(name), canonical.VehicleFund
	case "COLLECTIVE_INVESTMENT":
		if rawInstrumentType == "EXCHANGE_TRADED_FUND" {
			return silver.RefineETFExposure(name), canonical.VehicleETF
		}
		return silver.RefineETFExposure(name), canonical.VehicleFund
	case "BOND", "FIXED_INCOME":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "OPTION":
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case "FUTURE":
		return canonical.AssetClassPublicEquity, canonical.VehicleFuture
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// Instrument-key and description shapes for the statement-history
// classifier. Ported from fidelity's classifyHistorical (that
// source's statement PDFs share Schwab's lack of a structured type
// code, so shape heuristics are all there is), pared to the shapes
// Schwab statements actually surface: a CUSIP for bonds/options, an
// OCC option symbol, a coupon-bearing fixed-income line, a word-ETF
// or money-market description, and the 4-letter-plus-X mutual-fund
// ticker. First match wins; the fall-through is a plain stock, the
// overwhelming majority of statement lines.
var (
	// OCC option symbol: root + YYMMDD + C/P + strike.
	histOptionKeyRe  = regexp.MustCompile(`^[A-Z.]{1,6}\d{6}[CP]\d+(\.\d+)?$`)
	histOptionDescRe = regexp.MustCompile(`^(CALL|PUT)\b`)
	histCUSIPRe      = regexp.MustCompile(`^[A-Z0-9]{8}[0-9]$`)
	// Bond rows carry a coupon: "… 04.12500% 01/15/2042" / "FIXED COUPON".
	histBondDescRe = regexp.MustCompile(`(?i)\b\d{1,2}\.\d{3,5}%|FIXED COUPON`)
	// Money-market sweeps ("… GOVERNMENT MONEY MARKET", "… CASH RESERVES").
	histMoneyMktRe   = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b`)
	histETFDescRe    = regexp.MustCompile(`\bETF\b`)
	histMutualFundRe = regexp.MustCompile(`^[A-Z]{4}X$`)
)

// taxonomyHistorical derives the (exposure, vehicle) pair for a
// `historical_position_snapshots` row from its instrument key and
// statement description — the 2-D counterpart the legacy path leaves
// as (other), which the api side later overwrites whenever the same
// instrument reappears with a source-classified value. Money-market
// funds are cash × fund (TAXONOMY.md §5.8); a word-ETF / mutual-fund
// line takes its exposure from RefineETFExposure(description) inside
// the etf / fund vehicle. Every returned pair satisfies
// canonical.ValidTaxonomyPair; instrument_overrides remains the
// escape hatch for shapes these heuristics misjudge.
func taxonomyHistorical(instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	switch {
	case histOptionKeyRe.MatchString(instrumentKey),
		histOptionDescRe.MatchString(description):
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case histMoneyMktRe.MatchString(description):
		return canonical.AssetClassCash, canonical.VehicleFund
	case histCUSIPRe.MatchString(instrumentKey),
		histBondDescRe.MatchString(description):
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case histETFDescRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	case histMutualFundRe.MatchString(instrumentKey):
		return silver.RefineETFExposure(description), canonical.VehicleFund
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
	"Contributory IRA":   canonical.TaxWrapperTraditionalIRA,
	"Rollover IRA":       canonical.TaxWrapperTraditionalIRA,
	"Traditional IRA":    canonical.TaxWrapperTraditionalIRA,
	"Inherited IRA":      canonical.TaxWrapperTraditionalIRA,
	"Roth IRA":           canonical.TaxWrapperRothIRA,
	"Roth Contributory IRA": canonical.TaxWrapperRothIRA,
	"Inherited Roth IRA": canonical.TaxWrapperRothIRA,
	"SEP-IRA":            canonical.TaxWrapperSEPIRA,
	"SIMPLE IRA":         canonical.TaxWrapperSIMPLEIRA,

	// Education savings. Schwab uses the bare "Education
	// Savings" label for Coverdell ESAs on statements; 529s
	// don't typically live at Schwab retail (Schwab routes
	// 529s through state plans), so a future "529 College
	// Savings Plan" label would need adding.
	"Education Savings":          canonical.TaxWrapperCoverdellESA,
	"Coverdell ESA":              canonical.TaxWrapperCoverdellESA,
	"529 College Savings Plan":   canonical.TaxWrapper529,

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
