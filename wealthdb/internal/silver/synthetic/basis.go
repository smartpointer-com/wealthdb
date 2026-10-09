package synthetic

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// basisFor stamps a book value by the convention every synthetic writer
// keeps (docs/adapters/synthetic.md §5, docs/DESIGN.md §7.4): a holding
// at its average cost, a private fund or vehicle at the capital paid
// in, a property held directly at its price and a mortgage at its
// principal, the values on the day each was acquired. A real-estate
// fund or ETF is a holding like any other. Only crypto pays a purchase
// fee, booked as a fee transaction of its own, so a crypto book leaves
// it out.
func basisFor(ac canonical.AssetClass, veh canonical.Vehicle) canonical.Basis {
	b := canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesNone,
	}
	switch {
	case ac == canonical.AssetClassPrivateEquity && (veh == canonical.VehicleFund || veh == canonical.VehicleSPV):
		b.Method = canonical.BasisMethodPaidIn
	case ac == canonical.AssetClassRealEstate && (veh == canonical.VehiclePhysical || veh == canonical.VehicleMortgage):
		b.Method = canonical.BasisMethodAcquisitionValue
	case ac == canonical.AssetClassCrypto:
		b.Fees = canonical.BasisFeesExcluded
	}
	return b
}
