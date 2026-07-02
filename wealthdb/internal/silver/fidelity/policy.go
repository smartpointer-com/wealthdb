package fidelity

import "github.com/ptu/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy in the returns registry,
// beside the silver.Register(&Adapter{}) call. Flow-complete bank/pension:
// standard bank external + transfer-like sets. Forward knobs defaulted (no-op).
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(returns.BankFlowPolicy()))
}
