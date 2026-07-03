package canonical

// Canonical sign convention enforcement for transaction amounts.
// See TransactionChange.NetAmount in types.go for the contract.
//
// Some TxKind values have a fixed canonical sign by definition:
// a Withdrawal is always money leaving the account, a Sell is
// always money arriving from selling shares, and so on. The
// canonicalSign table below pins those.
//
// Other kinds depend on context: Interest can be received
// (positive, cash sweep / coupon-like) or paid (negative,
// margin / overdraft). CapitalGain can be a realised gain
// (positive) or loss (negative). Fx / FxForward / FxSwap have one
// leg in each currency. CorporateAction can be cash-positive
// (cash dividend), zero (stock split), or negative (cash
// merger). Journal / Other are catch-alls. For those, the
// adapter passes through whatever signed amount the source
// supplied.

// canonicalSign returns +1 or -1 for kinds with a fixed
// canonical direction, and 0 for kinds where the source-supplied
// sign should be preserved.
func canonicalSign(k TxKind) int {
	switch k {
	case TxKindBuy, TxKindWithdrawal, TxKindFee, TxKindTax, TxKindTransferOut, TxKindContribution:
		return -1
	case TxKindSell, TxKindDeposit, TxKindDividend, TxKindCoupon, TxKindTransferIn, TxKindDistribution:
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
