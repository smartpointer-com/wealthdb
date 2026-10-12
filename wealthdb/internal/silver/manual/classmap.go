package manual

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// taxonomyFor maps a manual position (kind + the row's `vehicle` column) to the
// 2-D taxonomy pair (exposure, vehicle). The exposure is derived from the
// `kind`; the vehicle comes straight from silver, where the collector stores
// a canonical Vehicle string, with a kind-derived default for an empty or
// unknown value (a row that predates the column). The returned
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
