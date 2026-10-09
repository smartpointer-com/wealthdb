package angellist

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// paidInBasis stamps angellist's book value: the contributed capital
// the portal states, gross of capital paid back, with the setup fees
// the portal folds into it (docs/DESIGN.md §7.4).
var paidInBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
}
