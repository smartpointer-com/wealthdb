package manual

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// init registers this source's co-located ReturnsPolicy.
//
// NAV-only regime: the adapter emits no transactions (transactions.go) —
// every wire is already captured by the bank collectors — so the source's
// own returns come from the value series, its MWR stays n/a, and the
// regime's honesty tags stay on.
//
// The external set is not empty, though. The two ledger kinds are admitted
// because value can move into or out of this book from ANOTHER tracked
// vehicle without touching a bank: a claim that arrives from
// another source, or one released from it. The other vehicle books its half of the
// move; this book only ever gains or loses a position. Left flow-less, that
// arrival reads as performance — a mark stepping up with nothing to fund it
// — and its later release reads as a loss of the whole claim. A row in the
// equity-transfer ledger (docs/DESIGN.md §13.10) written against this
// source's account supplies the missing half as a transfer_in or
// transfer_out, and only those two kinds count. Nothing else does: no
// collected transaction reaches this source, and a ledger row cannot emit
// any other kind.
//
// No transfer-like set: a ledger leg here has no partner on this source to
// net against, and its partner on the other vehicle is a deposit or
// withdrawal, which the heuristic netter never touches by design.
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(returns.RegimeNavOnly,
			[]canonical.TxKind{canonical.TxKindTransferIn, canonical.TxKindTransferOut},
			nil),
	))
}
