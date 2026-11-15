package plaid

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// returnsPolicy is the kind's ReturnsPolicy: flow_complete on the
// shared bank external and transfer-like sets, every other knob at its
// default.
//
// One plaid silver is one Item, and an Item may be a bank, a broker or a
// card issuer. The engine treats each account by its kind, whatever the
// policy says:
//
//   - a card never enters returns;
//   - a cash account shows no row of its own, but still adds into every
//     aggregate;
//   - a mortgage goes to the liability line.
//
// A source-wide hidden accounts grain would also hide a broker's investment
// accounts, so the grain stays normal. A deployment states an Item's own
// regime with config `returns_policy_overrides` (docs/DESIGN.md §5.6), as
// for the synthetic kind.
func returnsPolicy() *returns.ReturnsPolicy {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	return &p
}
