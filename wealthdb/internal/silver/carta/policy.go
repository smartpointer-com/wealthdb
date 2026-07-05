package carta

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy. NAV-only: manual emits
// no transactions; carta/equityzen emit synthetic balanced double-entries on a
// 0-pinned sentinel. No usable flows (empty external + transfer-like sets).
// Forward knobs defaulted (DefaultReturnsPolicy sets NavOnly from the regime).
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(returns.RegimeNavOnly, nil, nil),
	))
}
