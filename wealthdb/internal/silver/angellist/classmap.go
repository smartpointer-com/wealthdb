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
