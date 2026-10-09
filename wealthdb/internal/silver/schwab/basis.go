package schwab

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// statementBasis stamps a statement holding's book value: the cost
// basis Schwab prints, the sum of the holding's tax lots with the
// commissions in them (docs/DESIGN.md §7.4).
var statementBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesIncluded,
}
