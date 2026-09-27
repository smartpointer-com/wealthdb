package relevate

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// taxonomyFor derives the (asset_class, vehicle) pair from the
// silver signals (TAXONOMY.md). This helper is invoked at every
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
