package angellist

import (
	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/returns"
)

// init registers angellist's co-located ReturnsPolicy. Real funding-wallet
// ledger: deposit/withdrawal are genuine bank wires (external).
// contribution/distribution are INTERNAL (funding wallet <-> tracked deals) and
// are deliberately NOT external; no transfer-like set. Forward knobs defaulted.
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(
			returns.RegimeFlowComplete,
			[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal},
			nil,
		),
	))
}
