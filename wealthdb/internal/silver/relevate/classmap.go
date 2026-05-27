package relevate

import "github.com/ptu/wealthdb/internal/canonical"

// assetClassFor maps Relevate's `positions.asset_class` string
// (silver mirrors security.assetClass.name verbatim) to the
// canonical AssetClass. Every Relevate position is technically
// a Swisscanto index-fund holding, but the silver-side label
// names the UNDERLYING exposure class, which is what wealthdb
// asset-class queries actually want.
//
// Observed values in the current silver: "Stocks" / "Bonds" /
// "Liquidity " (trailing space is literal in Relevate's API) /
// "Real Estate" / "Alternatives". Unknown values fall through
// to AssetClassOther with the raw label preserved in payload.
func assetClassFor(raw string) canonical.AssetClass {
	switch raw {
	case "Stocks":
		return canonical.AssetClassEquity
	case "Bonds":
		return canonical.AssetClassBond
	case "Liquidity", "Liquidity ":
		return canonical.AssetClassMoneyMarket
	}
	// "Real Estate", "Alternatives", and anything new the
	// Relevate API introduces.
	return canonical.AssetClassOther
}
