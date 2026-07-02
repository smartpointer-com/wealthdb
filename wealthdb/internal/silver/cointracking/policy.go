package cointracking

import (
	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/returns"
)

// init registers cointracking's co-located ReturnsPolicy. Crypto: only FIAT
// deposit/withdrawal are real external capital. Crypto transfer_in/out are an
// unclassifiable mix (wallet-to-wallet internal + airdrops/gifts which are
// return) with no discriminator surviving to gold -> excluded (flag
// crypto_unclassified_transfers). No transfer-like set.
//
// Two source-scoped knobs depart from the defaults:
//   - OnboardScope=OnboardNone: cold-storage wallets debut mid-window funded by
//     within-entity crypto transfer_in legs already excluded from flows (their
//     fiat was counted once at the exchange), so per-constituent onboarding would
//     book a phantom +debut-value inflow and drag the aggregate TWR negative.
//   - AccountsGrainMeaningless=true: per-wallet (accounts-grain) return rows are
//     meaningless (coins sweep between wallets on arrival); the portfolios grain
//     and up stay valid, so returns are reported per portfolio and above.
func init() {
	p := returns.DefaultReturnsPolicy(
		returns.NewFlowPolicy(
			returns.RegimeCryptoPartial,
			[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal},
			nil,
		),
	)
	p.OnboardScope = returns.OnboardNone
	p.AccountsGrainMeaningless = true
	returns.RegisterPolicy(kindName, p)
}
