package manual

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// returnsPolicy is the kind's ReturnsPolicy.
//
// NAV-only regime: the adapter emits no cash flow of its own, only an
// exit's sell (transactions.go), so the source's own returns come from
// the value series, its MWR stays n/a, and the regime's honesty tags stay
// on.
//
// Under that regime only the equity-transfer ledger's legs count: value
// can reach this book from another tracked vehicle with no bank in
// between, and the ledger row that supplies the cash half of such a move
// lands here as a transfer_in or transfer_out (docs/DESIGN.md §13.9; the
// runbook is collectors/manual/DESIGN.md §6). A sell is the internal half
// of an exit, whose cash the banks book, so it is no flow.
//
// Closure is ledger-exact, as on the other sources whose flows are
// hand-dated: a release leg dated the day a claim is marked to zero is a
// real exit, not a stray to be subsumed into a closing tail.
func returnsPolicy() *returns.ReturnsPolicy {
	p := returns.DefaultReturnsPolicy(returns.NewFlowPolicy(returns.RegimeNavOnly))
	p.ClosureScope = returns.ClosureLedgerExact
	return &p
}
