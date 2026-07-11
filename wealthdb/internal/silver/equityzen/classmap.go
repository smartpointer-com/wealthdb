package equityzen

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// assetClassForKind maps the silver offerings.kind ('spv' | 'private_fund',
// which load.py derives from EquityZen's assetClass: ASSET_COMPANY -> spv,
// ASSET_MULTI_COMPANY_FUND -> private_fund) to the canonical AssetClass. A
// single-company special-purpose vehicle -> spv; a multi-company fund ->
// private_fund. An unknown/empty kind defaults to private_fund rather than
// over-claiming a single-company SPV.
func assetClassForKind(kind string) canonical.AssetClass {
	switch kind {
	case "spv":
		return canonical.AssetClassSPV
	case "private_fund":
		return canonical.AssetClassPrivateFund
	}
	return canonical.AssetClassPrivateFund
}

// taxonomyForKind is the 2-D (exposure, vehicle) analogue of
// assetClassForKind for the taxonomy migration. Both EquityZen kinds are
// private-company ownership, so the exposure is always
// AssetClassPrivateEquity; the vehicle carries the packaging distinction the
// legacy class conflated into asset_class — a single-company special-purpose
// vehicle -> VehicleSPV, a multi-company fund -> VehicleFund. An unknown/empty
// kind defaults to the fund vehicle, matching assetClassForKind's
// private_fund default (don't over-claim a single-company SPV). Both pairs
// satisfy canonical.ValidTaxonomyPair.
func taxonomyForKind(kind string) (canonical.AssetClass, canonical.Vehicle) {
	if kind == "spv" {
		return canonical.AssetClassPrivateEquity, canonical.VehicleSPV
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}
