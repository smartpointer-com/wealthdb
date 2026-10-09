package fidelity

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// lotBasis stamps every book value Fidelity states: a live holding's
// cost basis total, a statement holding's cost basis, a lot's, and a
// realized lot's on a 1099-B, a statement or the closed-positions
// page. Each is the sum of the holding's tax lots, with the purchase
// commissions in them (docs/DESIGN.md §7.4).
var lotBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesIncluded,
}
