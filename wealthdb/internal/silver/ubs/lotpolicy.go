package ubs

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"

// init registers ubs's lot policy: shadow at the average method. The
// source states an average cost for its positions and no lots, so the
// replay fills nothing; `gains check` compares its ledger with the
// stated cost (docs/LOTS.md §6, §9).
func init() {
	lots.RegisterPolicy(kindName, lots.ShadowPolicy())
}
