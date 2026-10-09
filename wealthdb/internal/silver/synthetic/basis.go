package synthetic

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// basisFor stamps a demo book value. The generator books every holding
// at its average cost, with no fees, and a private fund or vehicle at
// the capital paid in (docs/DESIGN.md §7.4).
func basisFor(ac canonical.AssetClass, veh canonical.Vehicle) canonical.Basis {
	m := canonical.BasisMethodAverage
	if ac == canonical.AssetClassPrivateEquity && (veh == canonical.VehicleFund || veh == canonical.VehicleSPV) {
		m = canonical.BasisMethodPaidIn
	}
	return canonical.Basis{Origin: canonical.BasisStated, Method: m, Fees: canonical.BasisFeesNone}
}
