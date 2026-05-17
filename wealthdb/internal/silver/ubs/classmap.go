package ubs

import "github.com/ptu/wealthdb/internal/canonical"

// assetClassForCFI maps an ISO 10962 CFI code's first character
// to a canonical AssetClass. The first character of the CFI code
// designates the asset category:
//
//	E - Equities                → equity
//	C - Collective investment   → fund
//	D - Debt instruments        → bond
//	O - Options                 → option
//	F - Futures                 → future
//	M - Others (mostly MM)      → money_market
//	T - Structured / cash collateral → other
//	R - Entitlements (rights)   → other
//
// Empty CFI codes (UBS does ship some instruments with no CFI
// populated — typically money-market funds) fall through to other.
func assetClassForCFI(cfi string) canonical.AssetClass {
	if cfi == "" {
		return canonical.AssetClassOther
	}
	switch cfi[0] {
	case 'E':
		return canonical.AssetClassEquity
	case 'C':
		return canonical.AssetClassFund
	case 'D':
		return canonical.AssetClassBond
	case 'O':
		return canonical.AssetClassOption
	case 'F':
		return canonical.AssetClassFuture
	case 'M':
		return canonical.AssetClassMoneyMarket
	default:
		// 'T' structured, 'R' rights, 'S' spot/forward FX (rare in
		// instruments), anything new.
		return canonical.AssetClassOther
	}
}
