package raiffeisenat

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy, beside the
// silver.Register(&Adapter{}) call. Flow-complete deposit banking: the ledger
// is the complete cash history, so the standard bank external set applies (the
// adapter emits only deposit/withdrawal/interest/fee; the set's unused
// transfer/journal kinds are harmless). AccountsGrainHidden: a deposit
// account is pure cash plumbing — money passes through it between other
// sources — so its rows are noise at every grain (a drained-then-refunded
// account chains a permanent −100%) and none are emitted; the balances and
// flows still enter every aggregate, where transfer legs against tracked
// sources cancel.
func init() {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	p.AccountsGrain = returns.AccountsGrainHidden
	returns.RegisterPolicy(kindName, p)
}
