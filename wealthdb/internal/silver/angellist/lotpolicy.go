package angellist

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"

// init registers angellist's lot policy (docs/LOTS.md §6): the cost basis
// of its SPV stakes and fund interests is what the source states, capital paid in, and
// no lot book describes it. The engine builds none and realizes a sale
// against the stated basis.
func init() {
	lots.RegisterPolicy(kindName, lots.StatedPolicy())
}
