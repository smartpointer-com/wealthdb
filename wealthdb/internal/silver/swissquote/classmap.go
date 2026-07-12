package swissquote

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

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

// taxonomyFor maps a Swissquote XLS section header (stored in
// positions.payload.asset_class; the set of headers is small and
// stable) to the (exposure, vehicle) pair emitted to gold, per
// TAXONOMY.md. Observed headers in real silver: "ETFs", "Bonds";
// the rest are Swissquote UI sections expected eventually.
//
// The vehicle is pinned by the section header; the exposure is fixed
// except for the two collective-vehicle sections ("ETFs", "Funds"),
// whose exposure is refined from the security name via
// silver.RefineETFExposure (crypto / metal / fixed_income, else the
// public_equity default). Options are the underlying's exposure —
// Swissquote surfaces equity options, so public_equity. Structured
// products default to equity exposure inside the structured_product
// wrapper (the same convention the ubs adapter uses for certificate
// CFIs). Unknown headers fall through to (other, other); a name-shy
// holding that lands there is corrected by config
// instrument_overrides, not here.
//
// Every pair returned satisfies canonical.ValidTaxonomyPair.
func taxonomyFor(xlsHeader, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch xlsHeader {
	case "Shares", "Stocks":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "ETFs":
		return silver.RefineETFExposure(name), canonical.VehicleETF
	case "Bonds":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "Funds":
		return silver.RefineETFExposure(name), canonical.VehicleFund
	case "Options":
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case "Precious Metals":
		return canonical.AssetClassMetal, canonical.VehiclePhysical
	case "Structured Products":
		return canonical.AssetClassPublicEquity, canonical.VehicleStructuredProduct
	default:
		return canonical.AssetClassOther, canonical.VehicleOther
	}
}
