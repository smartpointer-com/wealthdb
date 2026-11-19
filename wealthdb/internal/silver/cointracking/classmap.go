package cointracking

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// taxonomy returns the (exposure, vehicle) pair of a cointracking
// ticker. A coin or token is crypto exposure held directly in a
// wallet or exchange account (TAXONOMY.md — "coins and tokens held in
// wallets" → crypto × physical). A fiat ticker (isFiat) is money: cash
// in a demand deposit, as a wallet's fiat balance is. Both pairs
// satisfy canonical.ValidTaxonomyPair.
func taxonomy(ticker string) (canonical.AssetClass, canonical.Vehicle) {
	if isFiat(ticker) {
		return canonical.AssetClassCash, canonical.VehicleDemandDeposit
	}
	return canonical.AssetClassCrypto, canonical.VehiclePhysical
}
