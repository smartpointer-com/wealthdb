package equityzen

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// taxonomyForKind derives the (exposure, vehicle) pair from the silver
// offerings.kind ('spv' | 'private_fund', which load.py derives from
// EquityZen's assetClass: ASSET_COMPANY -> spv, ASSET_MULTI_COMPANY_FUND ->
// private_fund). Both kinds are private-company ownership, so the exposure
// is always AssetClassPrivateEquity; the vehicle carries the packaging — a
// single-company special-purpose vehicle -> VehicleSPV, a multi-company
// fund -> VehicleFund. An unknown/empty kind defaults to the fund vehicle
// rather than over-claiming a single-company SPV. Both pairs satisfy
// canonical.ValidTaxonomyPair.
func taxonomyForKind(kind string) (canonical.AssetClass, canonical.Vehicle) {
	if kind == "spv" {
		return canonical.AssetClassPrivateEquity, canonical.VehicleSPV
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}
