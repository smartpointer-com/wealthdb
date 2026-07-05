package cointracking

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// Direction discriminates inbound (asset arriving / cash inflow)
// from outbound (asset leaving / cash outflow) for non-Trade
// silver rows. Used by the adapter to sign Quantity, and to pick
// between the inbound/outbound variant of canonical TxKinds that
// have one of each (Deposit vs Withdrawal, Transfer_In vs
// Transfer_Out).
type direction int

const (
	dirInbound  direction = +1
	dirOutbound direction = -1
)

// classification carries everything the adapter needs to project
// one silver non-Trade row into a canonical TransactionChange:
// the kind to stamp (varying per fiat-vs-crypto + base-vs-non-base),
// the direction so Quantity gets the right sign, and a flag for
// whether to preserve the raw CT type in the row's payload (the
// canonical taxonomy collapses several CT types into one TxKind
// for "Other"; the raw value is the discriminator a reader needs
// to reconstruct the original event).
type classification struct {
	kind        canonical.TxKind
	dir         direction
	preserveRaw bool
}

// classifyCTType maps a silver `transactions.type` value to its
// canonical kind + direction. Returns (classification, false) for
// types the adapter doesn't recognise yet — the caller falls
// through to TxKindOther so a new CT type ships as "unmapped"
// rather than silently misclassified.
//
// `currencyIsFiat` tells the mapper whether the currency that
// moved is a fiat (USD, EUR, …). It only affects Deposit /
// Withdrawal: fiat → deposit/withdrawal (real cash), crypto →
// transfer_in/transfer_out (positions moving in/out).
func classifyCTType(ctType string, currencyIsFiat bool) (classification, bool) {
	switch ctType {
	// --- inbound, fiat-vs-crypto branches Deposit only ---
	case "Deposit":
		if currencyIsFiat {
			return classification{kind: canonical.TxKindDeposit, dir: dirInbound}, true
		}
		return classification{kind: canonical.TxKindTransferIn, dir: dirInbound}, true
	case "Withdrawal":
		if currencyIsFiat {
			return classification{kind: canonical.TxKindWithdrawal, dir: dirOutbound}, true
		}
		return classification{kind: canonical.TxKindTransferOut, dir: dirOutbound}, true

	// --- fee ---
	case "Other Fee":
		return classification{kind: canonical.TxKindFee, dir: dirOutbound}, true

	// --- staking (its own canonical kind) ---
	case "Staking":
		return classification{kind: canonical.TxKindStaking, dir: dirInbound}, true

	// --- interest-like inbound (recurring yield / income) ---
	case "Reward / Bonus", "Income":
		return classification{kind: canonical.TxKindInterest, dir: dirInbound}, true

	// --- non-taxable internal-transfer-pair markers ---
	case "Income (non taxable)":
		return classification{kind: canonical.TxKindTransferIn, dir: dirInbound}, true
	case "Expense (non taxable)":
		return classification{kind: canonical.TxKindTransferOut, dir: dirOutbound}, true

	// --- airdrops (one-off inbound) ---
	case "Airdrop", "Airdrop (non taxable)":
		return classification{kind: canonical.TxKindTransferIn, dir: dirInbound}, true

	// --- gifts: directionally split per CT (Gift / Tip is
	//      inbound — "received a tip"; Gift is outbound — "I
	//      gave something") ---
	case "Gift / Tip":
		return classification{kind: canonical.TxKindTransferIn, dir: dirInbound}, true
	case "Gift", "Donation":
		return classification{kind: canonical.TxKindTransferOut, dir: dirOutbound}, true

	// --- losses without a compensating leg. The canonical taxonomy
	//      doesn't have a dedicated kind for these (they're cost-
	//      basis-zeroing events, not really transfers), so they
	//      collapse to TxKindOther with the raw CT type preserved
	//      in payload for downstream tax tooling. ---
	case "Spend", "Lost", "Stolen":
		return classification{kind: canonical.TxKindOther, dir: dirOutbound, preserveRaw: true}, true
	}
	return classification{}, false
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
