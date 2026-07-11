package cointracking

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// taxonomyV2 returns the 2-D (exposure, vehicle) pair for a
// cointracking holding. CoinTracking only ever describes digital
// assets held directly in a wallet or exchange account, so the
// mapping is a single constant: crypto exposure in the physical
// wrapper (TAXONOMY.md — "coins and tokens held in wallets" →
// crypto × physical). It sits next to the legacy 1-D control value
// (canonical.AssetClassCrypto, set unchanged at the call sites) so
// the double-write stays in lockstep; the returned pair satisfies
// canonical.ValidTaxonomyPair.
func taxonomyV2() (canonical.AssetClass, canonical.Vehicle) {
	return canonical.AssetClassCrypto, canonical.VehiclePhysical
}
