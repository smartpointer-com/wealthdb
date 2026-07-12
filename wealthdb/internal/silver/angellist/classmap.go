package angellist

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// assetClassForKind maps the silver vehicles.kind ('spv' | 'fund', which
// load.py derives from the AngelList investableGuid suffix) to the
// canonical AssetClass. Single-company special-purpose vehicles (SPVs,
// RUVs) -> spv; multi-company venture/PE funds -> private_fund. An
// unknown/empty kind defaults to private_fund (the generic private-fund
// interest) rather than over-claiming a single-company SPV.
func assetClassForKind(kind string) canonical.AssetClass {
	switch kind {
	case "spv":
		return canonical.AssetClassSPV
	case "fund":
		return canonical.AssetClassPrivateFund
	}
	return canonical.AssetClassPrivateFund
}

// taxonomyForKind maps the silver vehicles.kind to the 2-D taxonomy
// pair (exposure, vehicle). Both single-company SPVs/RUVs and
// multi-company private funds are unlisted-company ownership, so the
// exposure is private_equity for both; the vehicle carries the
// SPV-vs-fund distinction (spv -> spv, fund -> fund). An unknown/empty
// kind defaults to the generic private fund, mirroring
// assetClassForKind. The returned pair always satisfies
// canonical.ValidTaxonomyPair.
func taxonomyForKind(kind string) (canonical.AssetClass, canonical.Vehicle) {
	switch kind {
	case "spv":
		return canonical.AssetClassPrivateEquity, canonical.VehicleSPV
	case "fund":
		return canonical.AssetClassPrivateEquity, canonical.VehicleFund
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}
