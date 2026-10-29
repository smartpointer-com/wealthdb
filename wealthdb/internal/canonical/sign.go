package canonical

import "slices"

// Canonical sign convention enforcement for transaction amounts.
// See TransactionChange.NetAmount in types.go for the contract.
//
// Some TxKind values have a fixed canonical sign by definition:
// a Withdrawal is always money leaving the account, a Sell is
// always money arriving from selling shares, and so on. The
// canonicalSign table below pins those. The card kinds are pinned
// from the card account's own perspective — its balance is carried
// as negative cash, so a Purchase drives it further negative (−1)
// while a Refund, CardPayment or Reward pays it back up (+1).
//
// Other kinds depend on context: Interest can be received
// (positive, cash sweep / coupon-like) or paid (negative,
// margin / overdraft). CapitalGain can be a realised gain
// (positive) or loss (negative). Fx / FxForward / FxSwap have one
// leg in each currency. CorporateAction can bring cash in (cash in
// lieu), move none (a stock split), or move shares valued at a price
// (a merger exchange). Journal / Other are catch-alls. For those, the
// adapter passes through whatever signed amount the source
// supplied.

// canonicalSign returns +1 or -1 for kinds with a fixed
// canonical direction, and 0 for kinds where the source-supplied
// sign should be preserved.
func canonicalSign(k TxKind) int {
	switch k {
	case TxKindBuy, TxKindWithdrawal, TxKindFee, TxKindTax, TxKindTransferOut,
		TxKindContribution, TxKindPurchase:
		return -1
	case TxKindSell, TxKindDeposit, TxKindDividend, TxKindCoupon, TxKindTransferIn,
		TxKindDistribution, TxKindRefund, TxKindCardPayment, TxKindReward:
		return +1
	}
	// TxKindInterest, TxKindStaking, TxKindCapitalGain,
	// TxKindFx, TxKindFxForward, TxKindFxSwap, TxKindCorporateAction,
	// TxKindJournal, TxKindOther: source-dependent. Staking is
	// almost always inbound (+) but slashing penalties on
	// proof-of-stake chains can yield a negative, so the canonical
	// sign isn't pinned.
	return 0
}

// cashSignedKinds names, per silver kind, the kinds with no canonical
// sign whose rows that move no quantity state the cash they moved, signed
// as every amount is: positive into the account. Such a row is a cash leg
// of a corporate action: cash in lieu of a fraction, a return of capital,
// the cash for an unissued right. A row of the same kind that moves shares
// is not one: its amount may value the shares rather than pay for them,
// so the pin reaches only the rows that move none. The sources pinned here
// are the ones whose adapters pass the statement's or the API's signed
// cash amount through for these rows: the fidelity kind (which the svb
// families load as) and schwab. A source not listed keeps its unsigned
// kinds unsigned (docs/CASHFLOW.md §4). The list stays ordered by silver
// kind and kind, the order the cash flow pass stamps.
var cashSignedKinds = []SignedKindPin{
	{"fidelity", TxKindCorporateAction},
	{"schwab", TxKindCorporateAction},
}

// cashflowUnsignedKinds are the six kinds cash flow excludes for want of
// a sign: canonicalSign's zero-signed set less interest, staking and
// capital_gain, which cash flow reads by their sign or nets inside their
// own class. A sign pin names one of them.
var cashflowUnsignedKinds = []TxKind{
	TxKindFx, TxKindFxForward, TxKindFxSwap, TxKindCorporateAction, TxKindJournal, TxKindOther,
}

// CashflowUnsignedKinds returns the six. A copy. A test accessor: the
// sign pins and cashflow_txn_nodes' exclusion are both held to it.
func CashflowUnsignedKinds() []TxKind {
	return slices.Clone(cashflowUnsignedKinds)
}

// SignedKindPin is one sign pin: a silver kind and a kind.
type SignedKindPin struct {
	SilverKind string
	Kind       TxKind
}

// CashSignedKinds lists the pins, in their order.
func CashSignedKinds() []SignedKindPin {
	return slices.Clone(cashSignedKinds)
}

// ApplyCanonicalSign returns `amount` with its sign normalised
// to the canonical convention for `kind`. Behaviour:
//
//   - nil in → nil out (NULL passes through).
//   - Zero amount: returned unchanged.
//   - Kind has no fixed canonical sign (canonicalSign returns 0):
//     amount returned unchanged so the source-supplied sign wins.
//   - Kind has a fixed canonical sign: the absolute value of
//     `amount` is taken and given the canonical sign. This
//     handles both "raw was positive" sources (UBS MT940 with
//     debit flag) and "raw was already signed" sources (Schwab
//     API) uniformly.
//
// Reversal handling: this helper unconditionally forces the
// canonical sign, which is wrong for explicit reversal rows
// (e.g. a `Dividend;Reversal` MT940 entry where the bank is
// clawing back a duplicate-booked dividend — the source sign is
// already correctly negative, and forcing positive would mask
// the correction). Adapters that recognise a reversal marker in
// their silver should bypass this helper for those rows and
// assign the source-signed amount directly.
//
// Adapters should call this in their transaction builder before
// assigning to GrossAmount and NetAmount.
func ApplyCanonicalSign(kind TxKind, amount *Decimal) *Decimal {
	if amount == nil {
		return nil
	}
	sign := canonicalSign(kind)
	if sign == 0 || amount.IsZero() {
		return amount
	}
	out := amount.Abs()
	if sign < 0 {
		out = out.Neg()
	}
	return &out
}
