// Package canonical defines the data shapes that flow between the
// silver adapter layer and the gold-layer writer. The enums here
// constrain the categorical columns of the gold schema; see
// docs/DESIGN.md §7.2 for the SQL-side definitions.
package canonical

// AssetClass is the canonical asset-class taxonomy for `positions.asset_class`
// and `instruments.asset_class`. Adapters map their source-specific
// type codes onto these values; unrecognised codes fall through to
// AssetClassOther per docs/DESIGN.md §6.8.
type AssetClass string

const (
	AssetClassEquity        AssetClass = "equity"
	AssetClassETF           AssetClass = "etf"
	AssetClassFund          AssetClass = "fund"
	AssetClassBond          AssetClass = "bond"
	AssetClassOption        AssetClass = "option"
	AssetClassFuture        AssetClass = "future"
	AssetClassFxForward     AssetClass = "fx_forward"
	AssetClassFxOption      AssetClass = "fx_option"
	AssetClassMoneyMarket   AssetClass = "money_market"
	AssetClassOTCDerivative AssetClass = "otc_derivative"
	AssetClassMetal         AssetClass = "metal"
	AssetClassOther         AssetClass = "other"
)

var assetClassValues = map[AssetClass]struct{}{
	AssetClassEquity: {}, AssetClassETF: {}, AssetClassFund: {},
	AssetClassBond: {}, AssetClassOption: {}, AssetClassFuture: {},
	AssetClassFxForward: {}, AssetClassFxOption: {},
	AssetClassMoneyMarket: {}, AssetClassOTCDerivative: {},
	AssetClassMetal: {}, AssetClassOther: {},
}

// Valid reports whether the receiver is one of the recognised
// AssetClass values. The gold writer calls this before insert.
func (a AssetClass) Valid() bool {
	_, ok := assetClassValues[a]
	return ok
}

// AccountKind discriminates `accounts.account_kind`.
type AccountKind string

const (
	AccountKindBrokerage   AccountKind = "brokerage"
	AccountKindCash        AccountKind = "cash"
	AccountKindSafekeeping AccountKind = "safekeeping"
	AccountKindCustody     AccountKind = "custody"
	// AccountKindOverlay is the synthetic per-portfolio account
	// that holds positions the bank attributes to the portfolio
	// directly rather than to any sub-account (UBS forward
	// contracts, money-market contracts, OTC contracts). One
	// overlay account per portfolio, lazily emitted when the
	// portfolio has at least one such position.
	AccountKindOverlay AccountKind = "overlay"
	AccountKindOther   AccountKind = "other"
)

var accountKindValues = map[AccountKind]struct{}{
	AccountKindBrokerage: {}, AccountKindCash: {},
	AccountKindSafekeeping: {}, AccountKindCustody: {},
	AccountKindOverlay: {}, AccountKindOther: {},
}

func (a AccountKind) Valid() bool {
	_, ok := accountKindValues[a]
	return ok
}

// TxKind discriminates `transactions.kind`.
type TxKind string

const (
	TxKindBuy             TxKind = "buy"
	TxKindSell            TxKind = "sell"
	TxKindDividend        TxKind = "dividend"
	TxKindCoupon          TxKind = "coupon"
	TxKindCapitalGain     TxKind = "capital_gain"
	TxKindInterest        TxKind = "interest"
	TxKindFee             TxKind = "fee"
	TxKindTax             TxKind = "tax"
	TxKindDeposit         TxKind = "deposit"
	TxKindWithdrawal      TxKind = "withdrawal"
	TxKindFxSpot          TxKind = "fx_spot"
	TxKindFxForward       TxKind = "fx_forward"
	TxKindCorporateAction TxKind = "corporate_action"
	TxKindTransferIn      TxKind = "transfer_in"
	TxKindTransferOut     TxKind = "transfer_out"
	TxKindJournal         TxKind = "journal"
	TxKindOther           TxKind = "other"
)

var txKindValues = map[TxKind]struct{}{
	TxKindBuy: {}, TxKindSell: {}, TxKindDividend: {}, TxKindCoupon: {},
	TxKindCapitalGain: {}, TxKindInterest: {}, TxKindFee: {}, TxKindTax: {},
	TxKindDeposit: {}, TxKindWithdrawal: {}, TxKindFxSpot: {},
	TxKindFxForward: {}, TxKindCorporateAction: {}, TxKindTransferIn: {},
	TxKindTransferOut: {}, TxKindJournal: {}, TxKindOther: {},
}

func (t TxKind) Valid() bool {
	_, ok := txKindValues[t]
	return ok
}

// BalanceKind discriminates `cash_balances.balance_kind`. The union
// covers all banks' balance taxonomies — UBS opening/closing/available,
// Schwab initial/current/projected/aggregated, plus a generic
// `closing` that Swissquote uses for its per-currency totals.
type BalanceKind string

const (
	BalanceKindOpening    BalanceKind = "opening"
	BalanceKindClosing    BalanceKind = "closing"
	BalanceKindAvailable  BalanceKind = "available"
	BalanceKindInitial    BalanceKind = "initial"
	BalanceKindCurrent    BalanceKind = "current"
	BalanceKindProjected  BalanceKind = "projected"
	BalanceKindAggregated BalanceKind = "aggregated"
)

var balanceKindValues = map[BalanceKind]struct{}{
	BalanceKindOpening: {}, BalanceKindClosing: {},
	BalanceKindAvailable: {}, BalanceKindInitial: {},
	BalanceKindCurrent: {}, BalanceKindProjected: {},
	BalanceKindAggregated: {},
}

func (b BalanceKind) Valid() bool {
	_, ok := balanceKindValues[b]
	return ok
}

// FxMode controls how `wealthdb positions -x <currency>` resolves
// the FX rate for each row: at the position's snapshot time
// (historic, interpolated) or at the most-recent available rate
// (current).
type FxMode string

const (
	FxModeHistoric FxMode = "historic"
	FxModeCurrent  FxMode = "current"
)

func (f FxMode) Valid() bool {
	return f == FxModeHistoric || f == FxModeCurrent
}
