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

// taxonomyFor is the 2-D (exposure, vehicle) analogue of
// assetClassFor: it derives the (asset_class, vehicle) pair from the
// same silver signals (TAXONOMY.md). This helper is invoked at every
// InstrumentChange / PositionChange the adapter builds, and the pair
// it returns always satisfies canonical.ValidTaxonomyPair.
//
// Vehicle is `fund` for every sleeve except Liquidity: the holder
// can only pick from a menu of pre-built Swisscanto index-fund
// strategies, never an individual security, so every invested sleeve
// is a pooled fund. The Liquidity sleeve is uninvested account cash
// awaiting allocation, i.e. an at-sight `demand_deposit`.
//
// Exposure follows the sleeve label: Stocks → public_equity, Bonds →
// fixed_income, Real Estate → real_estate (the fund column preserves
// the fact that it's an indirect listed vehicle, not directly-held
// property), and within Alternatives a physical-bullion fund reads as
// metal while everything else is a manager-strategy sleeve →
// hedge_fund. An unrecognised label defaults to multi_asset: a
// blended robo strategy sleeve is the most likely thing a new
// Relevate label denotes, and (multi_asset, fund) is a valid pair.
func taxonomyFor(raw, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch raw {
	case "Stocks":
		return canonical.AssetClassPublicEquity, canonical.VehicleFund
	case "Bonds":
		return canonical.AssetClassFixedIncome, canonical.VehicleFund
	case "Liquidity", "Liquidity ":
		return canonical.AssetClassCash, canonical.VehicleDemandDeposit
	case "Real Estate":
		return canonical.AssetClassRealEstate, canonical.VehicleFund
	case "Alternatives":
		if silver.NamesPhysicalMetal(name) {
			return canonical.AssetClassMetal, canonical.VehicleFund
		}
		return canonical.AssetClassHedgeFund, canonical.VehicleFund
	}
	return canonical.AssetClassMultiAsset, canonical.VehicleFund
}
