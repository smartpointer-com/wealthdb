package swissquote

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// taxWrapperFor maps the silver-side `accounts.account_product`
// label (Swissquote's per-account product designation, scraped
// from the eBanking account-overview page) to the canonical
// TaxWrapper enum. Mapping is documented in the swissquote
// README's "Gold-layer integration" section; keep the two
// aligned when either side changes.
//
// Returns an empty TaxWrapper when the product is unrecognised
// or empty — the caller should leave AccountChange.TaxWrapper
// nil in that case so a config-side override can supply a value
// (or gold's default-aware display renders 'taxable_personal').
func taxWrapperFor(accountProduct string) canonical.TaxWrapper {
	switch accountProduct {
	case "Trading", "Savings":
		return canonical.TaxWrapperTaxablePersonal
	case "Säule 3a":
		return canonical.TaxWrapperPillar3a
	case "Freizügigkeit":
		return canonical.TaxWrapperVestedBenefits
	}
	return ""
}

// assetClassFor maps Swissquote's XLS section-header string
// (stored in positions.payload.asset_class) to the canonical
// AssetClass. The set of headers is small and stable; unknown
// values fall through to AssetClassOther with the raw header
// preserved in payload.
//
// Observed headers in real silver: "ETFs", "Bonds". Others
// listed below are from Swissquote UI sections we expect to
// see eventually.
func assetClassFor(xlsHeader string) canonical.AssetClass {
	switch xlsHeader {
	case "Shares", "Stocks":
		return canonical.AssetClassEquity
	case "ETFs":
		return canonical.AssetClassETF
	case "Bonds":
		return canonical.AssetClassBond
	case "Funds":
		return canonical.AssetClassFund
	case "Options":
		return canonical.AssetClassOption
	case "Precious Metals":
		return canonical.AssetClassMetal
	default:
		// "Structured Products", anything new from a Swissquote
		// UI redesign, or empty.
		return canonical.AssetClassOther
	}
}
