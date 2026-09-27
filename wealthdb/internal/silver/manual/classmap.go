package manual

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// assetClassFor maps a manual position `kind` to its coarse canonical
// AssetClass — the intermediate 1-D class the collector's `kind`
// vocabulary mirrors (collectors/manual/DESIGN.md §6): real_estate,
// private_equity, convertible_note, private_fund, spv, mortgage,
// other. This is an IDENTITY map with a safety net: a kind the coarse
// vocabulary doesn't recognise (a class added to the collector's
// open-ended POSITION_KINDS but not yet to internal/canonical/enums.go)
// falls through to AssetClassOther. Callers use the result as a signal
// (e.g. the mortgage-negation check); the exposure written to gold
// comes from exposureForKind.
func assetClassFor(kind string) canonical.AssetClass {
	ac := canonical.AssetClass(kind)
	if ac.ValidCoarse() {
		return ac
	}
	return canonical.AssetClassOther
}

// taxonomyFor maps a manual position (kind + the row's `vehicle` column) to the
// 2-D taxonomy pair (exposure, vehicle). The exposure is derived from the
// `kind`; the vehicle comes straight from silver — the collector stores
// a canonical Vehicle string there (Stage 1 migration) — with a kind-derived
// default for rows that predate the column (empty/unknown value). The returned
// pair always satisfies canonical.ValidTaxonomyPair.
func taxonomyFor(kind, vehicle string) (canonical.AssetClass, canonical.Vehicle) {
	v := canonical.Vehicle(vehicle)
	if !v.Valid() {
		v = defaultVehicleForKind(kind)
	}
	return exposureForKind(kind, v), v
}

// exposureForKind derives the asset-class (exposure) from the kind.
// For the catch-all `other` kind the exposure follows the wrapper: a private
// loan or an escrow receivable is private_debt, everything else is other.
func exposureForKind(kind string, v canonical.Vehicle) canonical.AssetClass {
	switch kind {
	case "real_estate", "mortgage":
		return canonical.AssetClassRealEstate
	case "private_equity", "private_fund", "spv":
		return canonical.AssetClassPrivateEquity
	case "convertible_note":
		return canonical.AssetClassPrivateDebt
	case "other":
		if v == canonical.VehicleLoan || v == canonical.VehicleEscrow {
			return canonical.AssetClassPrivateDebt
		}
		return canonical.AssetClassOther
	default:
		return canonical.AssetClassOther
	}
}

// defaultVehicleForKind supplies a wrapper for silver rows that predate the
// `vehicle` column, keeping the emitted (exposure, vehicle) pair valid.
func defaultVehicleForKind(kind string) canonical.Vehicle {
	switch kind {
	case "real_estate":
		return canonical.VehiclePhysical
	case "mortgage":
		return canonical.VehicleMortgage
	case "private_equity":
		return canonical.VehicleStock
	case "private_fund":
		return canonical.VehicleFund
	case "spv":
		return canonical.VehicleSPV
	case "convertible_note":
		return canonical.VehicleConvertibleNote
	default:
		return canonical.VehicleOther
	}
}
