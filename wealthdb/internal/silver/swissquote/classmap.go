package swissquote

import "github.com/ptu/wealthdb/internal/canonical"

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
