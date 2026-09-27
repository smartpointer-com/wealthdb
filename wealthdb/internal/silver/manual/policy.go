package manual

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// init registers this source's co-located ReturnsPolicy.
//
// NAV-only regime: the adapter emits no transactions (transactions.go),
// so the source's own returns come from the value series, its MWR stays
// n/a, and the regime's honesty tags stay on.
//
// The external set admits the two equity-transfer-ledger kinds, and only
// those: value can reach this book from another tracked vehicle with no
// bank in between, and the ledger row that supplies the cash half of
// such a move lands here as a transfer_in or transfer_out (docs/DESIGN.md
// §13.10; the runbook is collectors/manual/DESIGN.md §6). No collected
// transaction can reach the source and a ledger row can emit no other
// kind, so nothing else becomes a flow. No transfer-like set: a ledger
// leg has nothing on this source to net against.
//
// Closure is ledger-exact, as on the other sources whose flows are
// hand-dated: a release leg dated the day a claim is marked to zero is a
// real exit, not a stray to be subsumed into a closing tail.
func init() {
	p := returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(returns.RegimeNavOnly,
			[]canonical.TxKind{canonical.TxKindTransferIn, canonical.TxKindTransferOut},
			nil),
	)
	p.ClosureScope = returns.ClosureLedgerExact
	returns.RegisterPolicy(kindName, p)
}
