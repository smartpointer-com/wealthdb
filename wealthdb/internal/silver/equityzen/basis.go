package equityzen

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// costBasis stamps equityzen's book value: the remaining shares at the
// price paid, without the execution fee EquityZen charges on top
// (docs/DESIGN.md §7.4).
var costBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesExcluded,
}
