package viac

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// acquisitionBasis stamps viac's book value: the units at the average
// acquisition price VIAC states, in CHF at the trade-date rate. VIAC
// charges no purchase fee; its fees are account-level
// (docs/DESIGN.md §7.4).
var acquisitionBasis = canonical.Basis{
	Origin: canonical.BasisDerived, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesNone,
}
