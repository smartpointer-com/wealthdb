package synthetic

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// basisFor stamps a book value by the convention every synthetic writer
// keeps (docs/adapters/synthetic.md §5, docs/DESIGN.md §7.4): a holding
// at its average cost, a private fund or vehicle at the capital paid
// in. Only crypto pays a purchase fee, booked as a fee transaction of
// its own, so a crypto book leaves it out.
func basisFor(ac canonical.AssetClass, veh canonical.Vehicle) canonical.Basis {
	b := canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesNone,
	}
	switch {
	case ac == canonical.AssetClassPrivateEquity && (veh == canonical.VehicleFund || veh == canonical.VehicleSPV):
		b.Method = canonical.BasisMethodPaidIn
	case ac == canonical.AssetClassCrypto:
		b.Fees = canonical.BasisFeesExcluded
	}
	return b
}
