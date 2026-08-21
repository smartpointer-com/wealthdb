package manual

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy. NAV-only: manual
// emits no transactions at all — every wire is already captured by the bank
// collectors (transactions.go) — so returns come from the value series alone;
// no usable flows (empty external + transfer-like sets).
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(returns.RegimeNavOnly, nil, nil),
	))
}
