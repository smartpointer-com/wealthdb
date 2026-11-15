package cointracking

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// direction discriminates inbound (asset arriving / cash inflow) from
// outbound (asset leaving / cash outflow) for non-Trade silver rows. It
// signs Quantity and NetAmount.
type direction int

const (
	dirInbound  direction = +1
	dirOutbound direction = -1
)

// kindTable maps a silver `transactions.type` to its canonical kind and
// to what the row does to the coin's lots (docs/LOTS.md §6). The kind
// alone cannot tell a deposit from an airdrop: both are transfer_in.
//
//   - Deposit and Withdrawal are deposit and withdrawal for a fiat
//     currency (real cash), transfer_in and transfer_out for a coin
//     (positions moving in and out).
//   - The non-taxable income and expense are the two legs of a move
//     between wallets, except that a dust sweep ("Dust Sweeping") is
//     its own event: the dust is sold and the coin it converts into is
//     bought, both at market value.
//   - A gift is split by direction: "Gift / Tip" is one received,
//     "Gift" and "Donation" one given.
//   - Spend, Lost and Stolen are other, their type kept as the
//     description: no canonical kind names a coin that leaves with no
//     leg to balance it.
//   - "Other Expense" states an action and no kind: its kind is read
//     as for a type the table does not know (projectNonTrade).
var kindTable = []silver.KindRule{
	{Verb: "Deposit", When: silver.Cash, Kind: canonical.TxKindDeposit, Action: canonical.TxActionIn},
	{Verb: "Deposit", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIn},
	{Verb: "Withdrawal", When: silver.Cash, Kind: canonical.TxKindWithdrawal, Action: canonical.TxActionOut},
	{Verb: "Withdrawal", Kind: canonical.TxKindTransferOut, Action: canonical.TxActionOut},
	{Verb: "Other Fee", Kind: canonical.TxKindFee, Action: canonical.TxActionFee},
	{Verb: "Staking", Kind: canonical.TxKindStaking, Action: canonical.TxActionIncome},
	{Verb: "Reward / Bonus", Kind: canonical.TxKindInterest, Action: canonical.TxActionIncome},
	{Verb: "Income", Kind: canonical.TxKindInterest, Action: canonical.TxActionIncome},
	{Verb: "Income (non taxable)", Text: dustSweep, Kind: canonical.TxKindTransferIn, Action: canonical.TxActionAcquired},
	{Verb: "Income (non taxable)", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIn},
	{Verb: "Expense (non taxable)", Kind: canonical.TxKindTransferOut, Action: canonical.TxActionOut},
	{Verb: "Airdrop", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIncome},
	{Verb: "Airdrop (non taxable)", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIncome},
	{Verb: "Gift / Tip", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIncome},
	{Verb: "Gift", Kind: canonical.TxKindTransferOut, Action: canonical.TxActionGift},
	{Verb: "Donation", Kind: canonical.TxKindTransferOut, Action: canonical.TxActionGift},
	{Verb: "Spend", Kind: canonical.TxKindOther, Action: canonical.TxActionSpend},
	{Verb: "Lost", Kind: canonical.TxKindOther, Action: canonical.TxActionLost},
	{Verb: "Stolen", Kind: canonical.TxKindOther, Action: canonical.TxActionLost},
	{Verb: "Other Expense", Text: dustSweep, Action: canonical.TxActionConverted},
	{Verb: "Other Expense", Action: canonical.TxActionSpend},
}

// dustSweep matches the comment CoinTracking gives a dust sweep, read
// with its surrounding space trimmed.
var dustSweep = regexp.MustCompile(`(?i)^Dust Sweeping$`)

// directionOf is the direction of a row the table gives a kind: the
// actions that bring the coin in are inbound, the rest outbound.
func directionOf(a canonical.TxAction) direction {
	switch a {
	case canonical.TxActionIn, canonical.TxActionIncome, canonical.TxActionAcquired:
		return dirInbound
	}
	return dirOutbound
}

// fiatTickers is the same set the price fetcher uses to skip fiat
// from coin_prices. Duplicated here rather than imported because
// the Python collector's set lives in collectors/cointracking/
// binance.py and isn't accessible from Go; the lists are kept in
// sync by hand (small enough that drift would surface quickly via
// the smoke-test sums).
var fiatTickers = map[string]struct{}{
	"USD": {}, "EUR": {}, "CHF": {}, "GBP": {}, "JPY": {},
	"AUD": {}, "CAD": {}, "NZD": {}, "SGD": {}, "HKD": {},
	"SEK": {}, "NOK": {}, "DKK": {}, "PLN": {}, "CZK": {},
	"HUF": {}, "RUB": {}, "INR": {}, "CNY": {}, "KRW": {},
	"TWD": {}, "THB": {}, "BRL": {}, "MXN": {}, "ZAR": {},
	"TRY": {}, "ILS": {},
}

func isFiat(ticker string) bool {
	_, ok := fiatTickers[ticker]
	return ok
}
