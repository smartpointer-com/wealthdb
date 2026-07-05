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
