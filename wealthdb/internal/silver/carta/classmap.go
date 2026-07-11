package carta

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// capTableAssetClass classifies a cap-table position from the security types it
// aggregates. A holding that is PURELY convertible instruments (SAFEs /
// convertible notes still pre-conversion) is a convertible_note — carried at
// principal and kept distinct from equity until it converts, mirroring the
// manual collector's convertible notes. Anything with real equity (shares,
// options, RSUs/RSAs, SARs, PIUs, warrants, equity grants) — including a
// convertible that has partly converted into shares — is private_equity: all
// illiquid private-company stakes in one bucket, the security type staying
// queryable in the position payload. Fund LP interests are classified
// separately (private_fund), off the fund path.
func capTableAssetClass(hasEquity bool) canonical.AssetClass {
	if hasEquity {
		return canonical.AssetClassPrivateEquity
	}
	return canonical.AssetClassConvertibleNote
}

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

// capTableTaxonomy is the 2-D-taxonomy (exposure, vehicle) counterpart of the
// legacy capTableAssetClass, for an aggregated cap-table position. Every
// cap-table stake is private_equity EXCEPT a purely-convertible holding (a
// SAFE / pre-conversion note), which is private_debt held via the
// convertible_note vehicle — mirroring the legacy private_equity vs
// convertible_note split. Within equity, a position holding any real
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
