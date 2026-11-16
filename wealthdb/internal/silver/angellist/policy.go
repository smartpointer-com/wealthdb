package angellist

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// returnsPolicy is the kind's ReturnsPolicy. Real funding-wallet
// ledger: deposit/withdrawal are genuine bank wires (external).
// contribution/distribution are INTERNAL (funding wallet <-> tracked deals) and
// are deliberately NOT external.
//
// transfer_in/transfer_out are external and nettable, as on every
// flow-complete source. The adapter never emits them; only the
// equity-transfer ledger does (docs/DESIGN.md §13.9),
// for an exit paid in shares: the vehicle's out-leg, dated and valued as the
// receiving brokerage's in-leg, so the vehicle realizes its gain on the day
// the shares leave and the pair nets in every aggregate holding both
// accounts. Forward knobs defaulted.
func returnsPolicy() *returns.ReturnsPolicy {
	p := returns.DefaultReturnsPolicy(returns.BankFlowPolicy())
	return &p
}
