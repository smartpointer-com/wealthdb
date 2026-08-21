package chase

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy, beside the
// silver.Register(&Adapter{}) call. Flow-complete deposit banking: the ledger
// is the complete cash history, so the standard bank external set applies (the
// adapter emits only deposit/withdrawal/interest/fee; the set's unused
// transfer/journal kinds are harmless). AccountsGrainMeaningless: a deposit
// account is a cash conduit — money passes through it between other sources —
// so a single account's TWR/MWR is noise (a drained-then-refunded account
// chains a permanent −100%); the coarse grains aggregate real flows and stay
// valid.
func init() {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	p.AccountsGrainMeaningless = true
	returns.RegisterPolicy(kindName, p)
}
