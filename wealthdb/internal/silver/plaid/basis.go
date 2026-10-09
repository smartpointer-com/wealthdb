package plaid

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// holdingBasis stamps a holding's book value: the cost basis Plaid
// passes on from the institution, which says neither how it was
// computed nor whether fees are in it (docs/DESIGN.md §7.4).
var holdingBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodUnknown, Fees: canonical.BasisFeesUnknown,
}
