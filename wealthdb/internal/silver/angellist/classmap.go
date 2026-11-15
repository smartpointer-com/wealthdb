package angellist

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// taxonomyForKind maps the silver offerings.kind to the 2-D taxonomy
// pair (exposure, vehicle). Both single-company SPVs/RUVs and
// multi-company private funds are unlisted-company ownership, so the
// exposure is private_equity for both; the vehicle carries the
// SPV-vs-fund distinction (spv -> spv, fund -> fund). An unknown/empty
// kind defaults to the generic private fund. The returned pair always
// satisfies canonical.ValidTaxonomyPair.
func taxonomyForKind(kind string) (canonical.AssetClass, canonical.Vehicle) {
	if kind == "spv" {
		return canonical.AssetClassPrivateEquity, canonical.VehicleSPV
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleFund
}
