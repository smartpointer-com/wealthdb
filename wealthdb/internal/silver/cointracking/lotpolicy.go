package cointracking

import (
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotPolicy is the lot policy (docs/LOTS.md §6). What a row does to the
// lots is its CoinTracking type's action (kindTable).
//
//   - Pooled at the portfolio: coins sweep between a portfolio's
//     wallets on arrival, so a wallet is no holder of lots. A withdrawal
//     and a deposit of a coin inside the portfolio cancel; what the
//     deposit falls short by is the network fee.
//   - Fees included: CoinTracking's amounts are net of a trade's fee in
//     either of the trade's own currencies, so the cost already holds
//     it. A fee in a third currency is valued and added
//     (canonical.TxActionThirdCurrencyFee).
//   - A fiat ticker is never a key: it is cash.
func lotPolicy() *lots.Policy {
	p := lots.TradingPolicy()
	p.Grain = lots.GrainPortfolio
	p.Fees = lots.FeesIncluded
	p.Skip = isFiat
	return &p
}

// thirdCurrencyFee reports a trade fee in neither of the trade's
// currencies, which its amounts therefore do not hold.
func thirdCurrencyFee(feeAmt sql.NullString, feeCcy, buyCcy, sellCcy string) bool {
	f := strings.ToUpper(feeCcy)
	if f == "" || f == strings.ToUpper(buyCcy) || f == strings.ToUpper(sellCcy) {
		return false
	}
	d, err := decimalOrNil(feeAmt)
	return err == nil && d != nil && !d.IsZero()
}
