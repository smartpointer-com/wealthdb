package synthetic

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// basisFor stamps a book value by the convention every synthetic writer
// keeps (docs/adapters/synthetic.md §5, docs/DESIGN.md §7.4): a holding
// at its average cost, or at the sum of its open lots where it states
// lots, a private fund or vehicle at the capital paid in, a property
// held directly at its price and a mortgage at its principal, the
// values on the day each was acquired. A real-estate fund or ETF is a
// holding like any other.
//
// lots says the book value is a sum of lots, or one lot's cost: a
// position that states open lots, or a realized lot. Only crypto pays
// a purchase fee. It is booked as a fee transaction of its own, so an
// average cost leaves it out and a lot's cost includes it.
func basisFor(ac canonical.AssetClass, veh canonical.Vehicle, lots bool) canonical.Basis {
	b := canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesNone,
	}
	switch {
	case ac == canonical.AssetClassPrivateEquity && (veh == canonical.VehicleFund || veh == canonical.VehicleSPV):
		b.Method = canonical.BasisMethodPaidIn
	case ac == canonical.AssetClassRealEstate && (veh == canonical.VehiclePhysical || veh == canonical.VehicleMortgage):
		b.Method = canonical.BasisMethodAcquisitionValue
	case lots:
		b.Method = canonical.BasisMethodLots
	}
	if ac == canonical.AssetClassCrypto {
		b.Fees = canonical.BasisFeesExcluded
		if lots {
			b.Fees = canonical.BasisFeesIncluded
		}
	}
	return b
}
