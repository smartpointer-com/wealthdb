package schwab

import "github.com/ptu/wealthdb/internal/canonical"

// classMap maps Schwab's `instrument.assetType` value to the
// canonical AssetClass. Unrecognised values fall through to
// `other` per docs/DESIGN.md §6.8.
var classMap = map[string]canonical.AssetClass{
	"EQUITY":                canonical.AssetClassEquity,
	"ETF":                   canonical.AssetClassETF,
	"MUTUAL_FUND":           canonical.AssetClassFund,
	"COLLECTIVE_INVESTMENT": canonical.AssetClassFund,
	"BOND":                  canonical.AssetClassBond,
	"FIXED_INCOME":          canonical.AssetClassBond,
	"OPTION":                canonical.AssetClassOption,
	"FUTURE":                canonical.AssetClassFuture,
	// CASH_EQUIVALENT and CURRENCY don't appear here — those rows
	// are routed to cash_balances by snapshots.go before this map
	// is consulted.
}

// assetClassFor returns the canonical class for the given Schwab
// assetType. Empty / unknown values become AssetClassOther; the
// raw assetType is preserved in the position's payload.
func assetClassFor(rawAssetType string) canonical.AssetClass {
	if c, ok := classMap[rawAssetType]; ok {
		return c
	}
	return canonical.AssetClassOther
}

// isCashAssetType reports whether a Schwab position's assetType
// indicates it should be projected as a cash balance rather than
// a security position.
func isCashAssetType(rawAssetType string) bool {
	switch rawAssetType {
	case "CASH_EQUIVALENT", "CURRENCY":
		return true
	default:
		return false
	}
}
