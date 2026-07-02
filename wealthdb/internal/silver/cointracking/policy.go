package cointracking

import (
	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/returns"
)

// init registers cointracking's co-located ReturnsPolicy. Crypto: only FIAT
// deposit/withdrawal are real external capital. Crypto transfer_in/out are an
// unclassifiable mix (wallet-to-wallet internal + airdrops/gifts which are
// return) with no discriminator surviving to gold -> excluded (flag
// crypto_unclassified_transfers). No transfer-like set. Forward knobs defaulted.
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(
			returns.RegimeCryptoPartial,
			[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal},
			nil,
		),
	))
}
