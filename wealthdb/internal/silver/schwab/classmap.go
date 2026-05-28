package schwab

import "github.com/ptu/wealthdb/internal/canonical"

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
// assetType. Empty / unknown values become AssetClassOther; the
// raw assetType is preserved in the position's payload.
func assetClassFor(rawAssetType string) canonical.AssetClass {
	if c, ok := classMap[rawAssetType]; ok {
		return c
	}
	return canonical.AssetClassOther
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
