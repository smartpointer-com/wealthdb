package angellist

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// init registers angellist's co-located ReturnsPolicy. Real funding-wallet
// ledger: deposit/withdrawal are genuine bank wires (external).
// contribution/distribution are INTERNAL (funding wallet <-> tracked deals) and
// are deliberately NOT external.
//
// transfer_in/transfer_out are external and transfer-like. The adapter never
// emits them; only the equity-transfer ledger does (docs/DESIGN.md §13.10),
// for an exit paid in shares: the vehicle's out-leg, dated and valued as the
// receiving brokerage's in-leg, so the vehicle realizes its gain on the day
// the shares leave and the pair nets in every aggregate holding both
// accounts. Forward knobs defaulted.
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(
			returns.RegimeFlowComplete,
			[]canonical.TxKind{
				canonical.TxKindDeposit, canonical.TxKindWithdrawal,
				canonical.TxKindTransferIn, canonical.TxKindTransferOut,
			},
			[]canonical.TxKind{canonical.TxKindTransferIn, canonical.TxKindTransferOut},
		),
	))
}
