package carta

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// The stamps on carta's book values (docs/DESIGN.md §7.4).
var (
	// shareBasis: a cap-table holding's book value is the sum of its
	// held lots' cost, the cash paid for each. An exercise or a
	// purchase carries no fee.
	shareBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesNone,
	}
	// fundBasis: a fund's book value is the capital contributed its
	// statement states, gross of capital paid back. Management fees
	// are drawn from that capital, so they are in it.
	fundBasis = canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
	// fundCarryBasis: before a fund's first statement, its book value
	// is the capital called so far, summed from the call notices.
	fundCarryBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
	}
)
