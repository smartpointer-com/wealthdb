package cointracking

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// init registers cointracking's lot policy (docs/LOTS.md §6).
//
//   - Pooled at the portfolio: coins sweep between a portfolio's
//     wallets on arrival, so a wallet is no holder of lots. A withdrawal
//     and a deposit of a coin inside the portfolio cancel; what the
//     deposit falls short by is the network fee.
//   - Fees included: CoinTracking's amounts are net of a trade's fee in
//     either of the trade's own currencies, so the cost already holds
//     it. A fee in a third currency is valued and added (Classified.Fee).
//   - A fiat ticker is never a key: it is cash.
func init() {
	p := lots.TradingPolicy()
	p.Grain = lots.GrainPortfolio
	p.Fees = lots.FeesIncluded
	p.Skip = isFiat
	p.Classify = classifyLots
	lots.RegisterPolicy(kindName, p)
}

// classifyLots reads a row by its CoinTracking type, which the adapter
// keeps in the payload (lotPayload). The kind alone cannot tell a
// deposit from an airdrop: both are transfer_in.
func classifyLots(t lots.Txn) lots.Classified {
	dust := strings.EqualFold(strings.TrimSpace(t.Comment), "Dust Sweeping")
	switch t.Type {
	case "Trade":
		c := lots.DefaultClassify(t)
		// A trade without the base currency is a sell leg and a buy leg;
		// the fee goes on the buy leg.
		c.Fee = thirdCurrencyFee(t) && !strings.HasSuffix(t.ID, ":"+sellLeg)
		return c
	case "Deposit":
		return lots.Classified{Action: lots.In}
	case "Withdrawal", "Expense (non taxable)":
		return lots.Classified{Action: lots.Out}
	case "Staking", "Reward / Bonus", "Income", "Airdrop", "Airdrop (non taxable)", "Gift / Tip":
		return lots.Classified{Action: lots.Income}
	case "Income (non taxable)":
		// A dust sweep converts small balances into one coin: the coin
		// arrives at its market value. Otherwise it is one leg of a move.
		if dust {
			return lots.Classified{Action: lots.Acquired}
		}
		return lots.Classified{Action: lots.In}
	case "Other Expense":
		if dust {
			return lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeSell}
		}
		return lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeSpend}
	case "Spend":
		return lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeSpend}
	case "Other Fee":
		return lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeFee}
	case "Lost", "Stolen":
		return lots.Classified{Action: lots.Gone, Disposal: lots.DisposeLost}
	case "Gift", "Donation":
		return lots.Classified{Action: lots.Gone, Disposal: lots.DisposeGift}
	}
	return lots.DefaultClassify(t)
}

// thirdCurrencyFee reports a trade fee in neither of the trade's
// currencies, which its amounts therefore do not hold.
func thirdCurrencyFee(t lots.Txn) bool {
	f := strings.ToUpper(t.FeeCurrency)
	return t.FeeAmount != 0 && f != "" && f != strings.ToUpper(t.BuyCurrency) && f != strings.ToUpper(t.SellCurrency)
}
