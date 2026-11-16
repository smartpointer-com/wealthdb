package canonical

import "slices"

// The roles of the account kinds and the transaction kinds: one row per
// kind, with what the kind is to the engine. Gold holds both tables as
// data (account_kinds and txn_kind_roles, stamped with the derived set),
// the Go predicates read them, and the SQL predicates at the head of the
// flow macros are held to them by a test. So a kind's role is decided
// here and nowhere else.

// AccountKindRow is one account kind and its roles.
type AccountKindRow struct {
	Kind AccountKind
	// Liability is a debt: its balance is owed rather than held.
	Liability bool
	// HoldsCash is an account that holds cash at a bank or a broker,
	// so its payload may state a deposit's rate.
	HoldsCash bool
	// FollowsSource keeps the kind its adapter states, whatever an
	// override says: an adapter signs a card's balance and books a
	// mortgage's principal by its own kind, so a restated liability
	// would read a debt as an asset, and an overlay is synthetic.
	FollowsSource bool
	// ReturnsInvisible contributes nothing to any return series, at any
	// grain (docs/RETURNS-NOTES.md, "Credit cards are
	// returns-invisible").
	ReturnsInvisible bool
}

// accountKinds are the account kinds, in the order of their constants.
var accountKinds = []AccountKindRow{
	{Kind: AccountKindBrokerage, HoldsCash: true},
	{Kind: AccountKindCash, HoldsCash: true},
	{Kind: AccountKindSafekeeping, HoldsCash: true},
	{Kind: AccountKindCustody, HoldsCash: true},
	{Kind: AccountKindOverlay, FollowsSource: true},
	{Kind: AccountKindCrypto},
	{Kind: AccountKindCryptoExchange},
	{Kind: AccountKindCryptoSelfCustody},
	{Kind: AccountKindMortgage, Liability: true, FollowsSource: true},
	{Kind: AccountKindCard, Liability: true, FollowsSource: true, ReturnsInvisible: true},
	{Kind: AccountKindDonorAdvisedFund},
	{Kind: AccountKindOther},
}

// AccountKinds returns the account kind table. A copy.
func AccountKinds() []AccountKindRow { return slices.Clone(accountKinds) }

// accountKindRow is the row of a, and whether a is a kind at all.
func accountKindRow(a AccountKind) (AccountKindRow, bool) {
	i := slices.IndexFunc(accountKinds, func(r AccountKindRow) bool { return r.Kind == a })
	if i < 0 {
		return AccountKindRow{}, false
	}
	return accountKinds[i], true
}

// Liability reports whether an account of the kind is a debt.
func (a AccountKind) Liability() bool { r, _ := accountKindRow(a); return r.Liability }

// HoldsCash reports whether an account of the kind holds cash at a bank
// or a broker.
func (a AccountKind) HoldsCash() bool { r, _ := accountKindRow(a); return r.HoldsCash }

// FollowsSource reports whether an account of the kind keeps the kind
// its adapter states (AccountKindRow.FollowsSource).
func (a AccountKind) FollowsSource() bool { r, _ := accountKindRow(a); return r.FollowsSource }

// ReturnsInvisible reports whether an account of the kind contributes
// nothing to any return series.
func (a AccountKind) ReturnsInvisible() bool { r, _ := accountKindRow(a); return r.ReturnsInvisible }

// AmountSide is the sign of amount a kind's row must carry to take a
// role: either sign, only a negative one, or only a positive one. The
// empty side is no role at all.
type AmountSide string

const (
	SideAny      AmountSide = "any"
	SideNegative AmountSide = "negative"
	SidePositive AmountSide = "positive"
)

// Admits reports whether a row of amount sign sign (-1, 0, +1) takes
// the role. A zero amount takes a signed role on neither side.
func (s AmountSide) Admits(sign int) bool {
	switch s {
	case SideAny:
		return true
	case SideNegative:
		return sign < 0
	case SidePositive:
		return sign > 0
	}
	return false
}

// TxKindRow is one transaction kind and its roles.
type TxKindRow struct {
	Kind TxKind
	// Spending and Income are the sides on which a row of the kind is
	// in the family's population: what its enrichment pass may write a
	// verdict for (docs/SPENDING.md §3, docs/INCOME.md §3). One
	// canonical kind, interest, carries two opposite events, and the
	// sign tells them apart.
	Spending, Income AmountSide
	// MatcherPool is a kind the internal-transfer matcher may pair: cash
	// moved into or out of an account, with a pinned canonical sign.
	MatcherPool bool
	// Unsigned is a kind cash flow excludes for want of a sign: gold
	// pins it none, and cash flow neither reads it by its sign nor nets
	// it inside its own class.
	Unsigned bool
	// Trade is a kind that moves capital into or out of a holding: a
	// trade, or private capital committed.
	Trade bool
	// IncomeFloor is the income type a row of the kind on its income
	// side reads where no tier placed it (income_txn_categories).
	IncomeFloor string
	// SpendFloor is the same for the spending side, where it does not
	// depend on the account (spend_txn_categories).
	SpendFloor string
	// Capital is a kind that moves owner capital across a returns
	// boundary: an external flow, where the account's source counts it
	// (returns.Regime). Nettable is a capital kind the returns netter may
	// pair; a deposit or withdrawal never nets by heuristic, because two
	// of one size can be two real movements.
	Capital, Nettable bool
}

// txKindRows are the transaction kinds, in the order of their
// constants.
var txKindRows = []TxKindRow{
	{Kind: TxKindBuy, Trade: true},
	{Kind: TxKindSell, Trade: true},
	{Kind: TxKindDividend, Income: SideAny, IncomeFloor: "INCOME_DIVIDENDS"},
	{Kind: TxKindCoupon, Income: SideAny, IncomeFloor: "INCOME_INTEREST_EARNED"},
	{Kind: TxKindCapitalGain, Income: SideAny, IncomeFloor: IncomeDetailedDistributions},
	{Kind: TxKindInterest, Spending: SideNegative, Income: SidePositive,
		IncomeFloor: "INCOME_INTEREST_EARNED", SpendFloor: "BANK_FEES_INTEREST_CHARGE"},
	{Kind: TxKindStaking, Income: SideAny, IncomeFloor: IncomeDetailedStaking},
	{Kind: TxKindContribution, Trade: true},
	{Kind: TxKindDistribution, Income: SideAny, IncomeFloor: IncomeDetailedCapitalReturn},
	{Kind: TxKindFee, Spending: SideAny},
	{Kind: TxKindTax, Spending: SideAny, SpendFloor: SpendDetailedWithholdingTax},
	{Kind: TxKindDeposit, Income: SideAny, MatcherPool: true, Capital: true},
	{Kind: TxKindWithdrawal, Spending: SideAny, MatcherPool: true, Capital: true},
	{Kind: TxKindPurchase, Spending: SideAny},
	{Kind: TxKindRefund, Spending: SideAny},
	{Kind: TxKindCardPayment, MatcherPool: true},
	{Kind: TxKindReward, Income: SideAny, IncomeFloor: IncomeDetailedRewards},
	{Kind: TxKindFx, Unsigned: true},
	{Kind: TxKindFxForward, Unsigned: true},
	{Kind: TxKindFxSwap, Unsigned: true},
	{Kind: TxKindCorporateAction, Unsigned: true},
	{Kind: TxKindTransferIn, MatcherPool: true, Capital: true, Nettable: true},
	{Kind: TxKindTransferOut, MatcherPool: true, Capital: true, Nettable: true},
	{Kind: TxKindJournal, Unsigned: true, Capital: true, Nettable: true},
	{Kind: TxKindOther, Unsigned: true},
}

// TxKindRows returns the transaction kind table. A copy.
func TxKindRows() []TxKindRow { return slices.Clone(txKindRows) }

// capitalKinds maps each capital kind to whether it is nettable.
var capitalKinds = func() map[TxKind]bool {
	out := map[TxKind]bool{}
	for _, r := range txKindRows {
		if r.Capital {
			out[r.Kind] = r.Nettable
		}
	}
	return out
}()

// CapitalKind reports whether a kind moves owner capital (TxKindRow.Capital),
// and whether the returns netter may pair it (TxKindRow.Nettable).
func CapitalKind(k TxKind) (capital, nettable bool) {
	nettable, capital = capitalKinds[k]
	return capital, nettable
}

// kindsWhere are the kinds whose row has the role, in table order.
func kindsWhere(role func(TxKindRow) bool) []TxKind {
	var out []TxKind
	for _, r := range txKindRows {
		if role(r) {
			out = append(out, r.Kind)
		}
	}
	return out
}
