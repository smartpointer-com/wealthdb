package relevate

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// assetClassFor maps Relevate's `positions.asset_class` string
// (silver mirrors security.assetClass.name verbatim) to the
// canonical AssetClass, using the instrument name to sharpen
// labels that are too coarse on their own.
//
// Every Relevate position is structurally a Swisscanto index-
// fund holding — the holder can't buy individual securities,
// only pick from a small menu of pre-built strategies whose
// constituents are funds. So the canonical default for any
// Relevate position is AssetClassFund. We override to a more
// specific underlying-exposure class when the silver-side
// label maps cleanly to one wealthdb already has a dedicated
// enum for ("Stocks" -> Equity, "Bonds" -> Bond, "Liquidity"
// -> MoneyMarket), and within "Alternatives" when the fund
// name reads as physical bullion (a physical-gold index
// sleeve -> Metal). "Real Estate" stays Fund: the canonical
// `real_estate` class is reserved for directly-held property,
// and the Swisscanto sleeve is an *indirect* listed-vehicle
// index fund. Anything else Relevate may introduce also stays
// Fund — correct and more informative than Other.
//
// Trade-off: an unrecognised label no longer surfaces as
// "other" in wealthdb positions, so it can't act as a tripwire
// for "Relevate added a new strategy class we should re-check
// the mapping for". The canonical case is held by Stocks /
// Bonds / Liquidity; if those ever start losing rows in silver
// while the position count stays steady, that's the signal a
// label renamed and the explicit case needs updating.
func assetClassFor(raw, name string) canonical.AssetClass {
	switch raw {
	case "Stocks":
		return canonical.AssetClassEquity
	case "Bonds":
		return canonical.AssetClassBond
	case "Liquidity", "Liquidity ":
		return canonical.AssetClassMoneyMarket
	case "Alternatives":
		if silver.NamesPhysicalMetal(name) {
			return canonical.AssetClassMetal
		}
	}
	return canonical.AssetClassFund
}
