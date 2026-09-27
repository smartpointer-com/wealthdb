package carta

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// isStockVehicleType reports whether a cap-table security_type (collector
// DESIGN.md: share / option / rsu / rsa / warrant / convertible / sar / piu /
// equity_grant) is a real share-settled ownership unit — the `stock` vehicle —
// as opposed to an option-shaped claim (option / warrant / sar) or a
// convertible. RSUs/RSAs/PIUs/equity grants settle into actual shares, so they
// join plain `share` under `stock` (TAXONOMY.md decision 5: RSUs→stock,
// warrants→option).
func isStockVehicleType(secType string) bool {
	switch secType {
	case "share", "rsu", "rsa", "piu", "equity_grant":
		return true
	}
	return false
}

// capTableTaxonomy derives the (exposure, vehicle) pair for an
// aggregated cap-table position. Every cap-table stake is private_equity EXCEPT
// a purely-convertible holding (a SAFE / pre-conversion note), which
// is private_debt held via the convertible_note vehicle. Within
// equity, a position holding any real
// share-settled unit (share / rsu / rsa / piu / equity_grant) is the `stock`
// vehicle; a position whose equity is only option-shaped claims (option /
// warrant / sar) is the `option` vehicle. The returned pair always satisfies
// canonical.ValidTaxonomyPair.
func capTableTaxonomy(hasEquity, hasStockLike bool) (canonical.AssetClass, canonical.Vehicle) {
	if !hasEquity {
		return canonical.AssetClassPrivateDebt, canonical.VehicleConvertibleNote
	}
	if hasStockLike {
		return canonical.AssetClassPrivateEquity, canonical.VehicleStock
	}
	return canonical.AssetClassPrivateEquity, canonical.VehicleOption
}
