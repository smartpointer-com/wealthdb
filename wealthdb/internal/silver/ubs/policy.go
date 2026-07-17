package ubs

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// init registers this source's co-located ReturnsPolicy. UBS is a CONDUIT
// relationship: external cash lands in a cash account and funds securities in a
// DIFFERENT account inside the same banking relationship. The knobs below
// make the engine count that capital exactly once and are source-scoped — they
// apply ONLY to UBS constituents (resolved by kind), so every other source keeps
// the default no-op behavior and stays byte-identical, even at the merged global
// grain.
//
//   - OnboardScope = OnboardPerEntityOnce: onboard the relationship's inception
//     value ONCE, not once per account. An internal cash->securities move within
//     the relationship is then just internal (no second onboarding), so the
//     conduit double-count never forms and there is nothing to net back out.
//   - ConduitKinds = [cash]: UBS cash/current accounts are plumbing — they feed
//     the value spine but emit no per-account return/onboarding.
//   - ExternalOnly = true: only boundary-crossing flows count. Internal churn
//     (cash<->securities settlements, inter-account transfers, FX, mandate
//     funding) is not a flow. The external/internal split is emitted PRE-TAGGED
//     on the transaction in web_reader.go (pdfCashIsExternal, PII-free own-IBAN
//     rule): internal rows are demoted to a non-flow kind and never reach the
//     flow set, so no ClassifyFlow hook / FlowCtx payload is needed here.
//   - Inception = InceptionFirstRealSnapshot: anchor UBS's return window at its
//     first real position snapshot rather than the sparse cash-only pre-history,
//     killing the tiny-base artifact of a window opened years early.
func init() {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	p.OnboardScope = returns.OnboardPerEntityOnce
	p.ConduitKinds = []canonical.AccountKind{canonical.AccountKindCash}
	p.ExternalOnly = true
	p.Inception = returns.InceptionFirstRealSnapshot
	returns.RegisterPolicy(kindName, p)
}
