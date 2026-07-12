package cointracking

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// taxonomy returns the (exposure, vehicle) pair for a cointracking
// holding. CoinTracking only ever describes digital assets held
// directly in a wallet or exchange account, so the mapping is a
// single constant: crypto exposure in the physical wrapper
// (TAXONOMY.md — "coins and tokens held in wallets" → crypto ×
// physical). The returned pair satisfies canonical.ValidTaxonomyPair.
func taxonomy() (canonical.AssetClass, canonical.Vehicle) {
	return canonical.AssetClassCrypto, canonical.VehiclePhysical
}
