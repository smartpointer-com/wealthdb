package carta

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"

// init registers carta's lot policy (docs/LOTS.md §6): the cost basis
// of its exercised and bought shares and fund interests is what the source states, capital paid in, and
// no lot book describes it. The engine builds none and realizes a sale
// against the stated basis.
func init() {
	lots.RegisterPolicy(kindName, lots.StatedPolicy())
}
