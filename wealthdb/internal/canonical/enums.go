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
	AssetClassCrypto        AssetClass = "crypto"
	// Private-market classes. Distinct from the public-market
	// AssetClassEquity / AssetClassFund so portfolio queries can
	// separate illiquid, non-quotable private holdings (Carta
	// cap-table stakes, AngelList SPV / fund LP interests) from
	// listed securities. AssetClassPrivateEquity covers direct
	// private-company equity and equity-comp (shares, options,
	// RSUs/RSAs, warrants); AssetClassSPV covers an LP
	// interest in a single-company special-purpose vehicle;
	// AssetClassPrivateFund covers an LP interest in a multi-company
	// venture/PE fund.
	AssetClassPrivateEquity AssetClass = "private_equity"
	AssetClassSPV           AssetClass = "spv"
	AssetClassPrivateFund   AssetClass = "private_fund"
	// Further private classes (mostly manual, hand-maintained, no
	// source UI — see collectors/manual). AssetClassRealEstate is a
	// directly-held property. AssetClassConvertibleNote is an early-stage
	// convertible loan / note or SAFE (typically 0% interest, expected to
	// convert to equity at the next round or be written to zero); kept
	// distinct from the public-market `bond` and from `private_equity` until
	// it converts. Emitted by the manual collector and by Carta (a
	// pre-conversion SAFE / convertible holding). Other manual kinds reuse
	// existing classes (private_equity / private_fund / spv) or fall through
	// to `other`.
	AssetClassRealEstate      AssetClass = "real_estate"
	AssetClassConvertibleNote AssetClass = "convertible_note"
	// AssetClassMortgage is a real-property-backed liability:
	// the outstanding principal sits as a negative MarketValue on
	// a synthetic per-mortgage Position. Paired 1:1 with
	// AccountKindMortgage because UBS (and most banks) expose
	// each mortgage as its own account.
	AssetClassMortgage AssetClass = "mortgage"
	AssetClassOther    AssetClass = "other"
)

var assetClassValues = map[AssetClass]struct{}{
	AssetClassEquity: {}, AssetClassETF: {}, AssetClassFund: {},
	AssetClassBond: {}, AssetClassOption: {}, AssetClassFuture: {},
	AssetClassFxForward: {}, AssetClassFxOption: {},
	AssetClassMoneyMarket: {}, AssetClassOTCDerivative: {},
	AssetClassMetal: {}, AssetClassCrypto: {},
	AssetClassPrivateEquity: {}, AssetClassSPV: {}, AssetClassPrivateFund: {},
	AssetClassRealEstate: {}, AssetClassConvertibleNote: {},
	AssetClassMortgage: {},
	AssetClassOther:    {},
}

// Valid reports whether the receiver is one of the recognised
// AssetClass values. The gold writer calls this before insert.
func (a AssetClass) Valid() bool {
	_, ok := assetClassValues[a]
	return ok
}

// AccountKind discriminates `accounts.account_kind`. This is the
// "technical container" dimension — what the bank's UI calls the
// account regardless of its tax treatment or management style
// (those live in `tax_wrapper` and `management_style`).
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
	// Crypto kinds. The cointracking adapter uses the single
	// `crypto` bucket — CT's wallet-display-name vocabulary
	// (an exchange, a hardware wallet, a staking provider, …) doesn't carry a
	// reliable exchange-vs-self-custody signal, and the rest of
	// the stack values the holding the same way either way. The
	// finer-grained `crypto_exchange` / `crypto_self_custody`
	// values stay reserved for any future adapter that does
	// surface the distinction at source.
	AccountKindCrypto            AccountKind = "crypto"
	AccountKindCryptoExchange    AccountKind = "crypto_exchange"
	AccountKindCryptoSelfCustody AccountKind = "crypto_self_custody"
	// AccountKindMortgage is a real-property-backed liability
	// account. Outstanding principal lives as a negative-value
	// position (AssetClassMortgage) on this account; interest /
	// principal payments flow in as transactions debited from a
	// regular cash account with counterparty "Maturity".
	AccountKindMortgage AccountKind = "mortgage"
	AccountKindOther    AccountKind = "other"
)

var accountKindValues = map[AccountKind]struct{}{
	AccountKindBrokerage: {}, AccountKindCash: {},
	AccountKindSafekeeping: {}, AccountKindCustody: {},
	AccountKindOverlay:        {},
	AccountKindCrypto:         {},
	AccountKindCryptoExchange: {}, AccountKindCryptoSelfCustody: {},
	AccountKindMortgage: {},
	AccountKindOther:    {},
}

func (a AccountKind) Valid() bool {
	_, ok := accountKindValues[a]
	return ok
}

// TaxWrapper discriminates `accounts.tax_wrapper` — the tax /
// regulatory registration of the account, independent of the
// technical container (account_kind) and management style.
// Defaults to TaxWrapperTaxablePersonal when neither the adapter
// nor a config override has more specific information.
//
// Jurisdictional coverage: US (the IRA / 529 / ESA / DAF / trust
// / custodial families) and Switzerland (the BVG/LPP "pillar"
// families). Other jurisdictions can be added by extending this
// enum and the migration's CHECK constraint without disturbing
// existing rows.
type TaxWrapper string

const (
	// Generic / cross-jurisdictional.
	TaxWrapperTaxablePersonal TaxWrapper = "taxable_personal"
	TaxWrapperTaxableJoint    TaxWrapper = "taxable_joint"
	TaxWrapperFoundation      TaxWrapper = "foundation" // CH Stiftung, US private foundation

	// US retirement.
	TaxWrapperTraditionalIRA TaxWrapper = "traditional_ira"
	TaxWrapperRothIRA        TaxWrapper = "roth_ira"
	TaxWrapperSEPIRA         TaxWrapper = "sep_ira"
	TaxWrapperSIMPLEIRA      TaxWrapper = "simple_ira"
	TaxWrapper401k           TaxWrapper = "401k"
	TaxWrapper403b           TaxWrapper = "403b"
	TaxWrapper457b           TaxWrapper = "457b"

	// US education / health.
	TaxWrapper529          TaxWrapper = "529"
	TaxWrapperCoverdellESA TaxWrapper = "coverdell_esa"
	TaxWrapperHSA          TaxWrapper = "hsa"

	// US charitable.
	TaxWrapperDAF TaxWrapper = "daf" // donor-advised fund

	// US custodial-for-minors.
	TaxWrapperCustodialUTMA TaxWrapper = "custodial_utma"
	TaxWrapperCustodialUGMA TaxWrapper = "custodial_ugma"

	// US trust.
	TaxWrapperTrustGrantor    TaxWrapper = "trust_grantor"
	TaxWrapperTrustNonGrantor TaxWrapper = "trust_non_grantor"
	TaxWrapperTrustCharitable TaxWrapper = "trust_charitable"

	// Switzerland — three-pillar system (private pension piece
	// of the picture; AHV/IV state pension isn't an account you
	// can hold).
	TaxWrapperPillar2        TaxWrapper = "pillar_2"        // BVG/LPP occupational
	TaxWrapperVestedBenefits TaxWrapper = "vested_benefits" // Freizügigkeitskonto (pillar 2 in transit)
	TaxWrapperPillar3a       TaxWrapper = "pillar_3a"       // tax-advantaged private pension

	TaxWrapperOther TaxWrapper = "other"
)

var taxWrapperValues = map[TaxWrapper]struct{}{
	TaxWrapperTaxablePersonal: {}, TaxWrapperTaxableJoint: {}, TaxWrapperFoundation: {},
	TaxWrapperTraditionalIRA: {}, TaxWrapperRothIRA: {},
	TaxWrapperSEPIRA: {}, TaxWrapperSIMPLEIRA: {},
	TaxWrapper401k: {}, TaxWrapper403b: {}, TaxWrapper457b: {},
	TaxWrapper529: {}, TaxWrapperCoverdellESA: {}, TaxWrapperHSA: {},
	TaxWrapperDAF:           {},
	TaxWrapperCustodialUTMA: {}, TaxWrapperCustodialUGMA: {},
	TaxWrapperTrustGrantor: {}, TaxWrapperTrustNonGrantor: {}, TaxWrapperTrustCharitable: {},
	TaxWrapperPillar2: {}, TaxWrapperVestedBenefits: {}, TaxWrapperPillar3a: {},
	TaxWrapperOther: {},
}

func (t TaxWrapper) Valid() bool {
	_, ok := taxWrapperValues[t]
	return ok
}

// ManagementStyle discriminates `accounts.management_style` — who
// places the trades. Orthogonal to AccountKind and TaxWrapper.
type ManagementStyle string

const (
	// ManagementStyleSelfDirected: the account holder places all
	// trades. Default when neither the adapter nor a config
	// override says otherwise.
	ManagementStyleSelfDirected ManagementStyle = "self_directed"
	// ManagementStyleAdvisory: an advisor recommends trades but
	// the account holder approves each one (CH Anlageberatung,
	// US non-discretionary advisory).
	ManagementStyleAdvisory ManagementStyle = "advisory"
	// ManagementStyleDiscretionary: an advisor places trades
	// under a limited power of attorney without per-trade
	// approval (CH Vermögensverwaltung, US discretionary
	// managed accounts).
	ManagementStyleDiscretionary ManagementStyle = "discretionary"
	// ManagementStyleAutomated: algorithmic / robo-advisor.
	ManagementStyleAutomated ManagementStyle = "automated"
)

var managementStyleValues = map[ManagementStyle]struct{}{
	ManagementStyleSelfDirected:  {},
	ManagementStyleAdvisory:      {},
	ManagementStyleDiscretionary: {},
	ManagementStyleAutomated:     {},
}

func (m ManagementStyle) Valid() bool {
	_, ok := managementStyleValues[m]
	return ok
}

// TxKind discriminates `transactions.kind`.
type TxKind string

const (
	TxKindBuy         TxKind = "buy"
	TxKindSell        TxKind = "sell"
	TxKindDividend    TxKind = "dividend"
	TxKindCoupon      TxKind = "coupon"
	TxKindCapitalGain TxKind = "capital_gain"
	TxKindInterest    TxKind = "interest"
	// TxKindStaking is for proof-of-stake reward distributions
	// (and the equivalents from delegated-staking / liquid-staking
	// platforms). Distinct from TxKindInterest because most tax
	// jurisdictions treat the two differently — staking rewards
	// are taxed at receipt as ordinary income in some, as capital
	// gains in others; the gold layer keeps them separate so
	// downstream tax tooling can apply the right rule.
	TxKindStaking TxKind = "staking"
	// TxKindContribution is capital the holder commits INTO a private
	// fund / SPV / LP interest (a capital call / funding a deal) — cash
	// out of the funding account. Negative by canonical convention; a
	// reversal (an over-subscribed commitment refunded back to the
	// funding account) is the same kind with a positive, source-signed
	// amount (adapters bypass ApplyCanonicalSign for those — see sign.go).
	TxKindContribution TxKind = "contribution"
	// TxKindDistribution is a cash distribution from a private fund /
	// SPV / LP interest to the holder (return of capital + realized
	// gains) — cash into the funding account. Distinct from TxKindDividend
	// (public-equity income): an LP distribution blends basis return and
	// gain. Positive (cash in) by canonical convention. The pair
	// contribution/distribution is the private-market analogue of buy/sell
	// and is reused across AngelList / EquityZen / Carta.
	TxKindDistribution    TxKind = "distribution"
	TxKindFee             TxKind = "fee"
	TxKindTax             TxKind = "tax"
	TxKindDeposit         TxKind = "deposit"
	TxKindWithdrawal      TxKind = "withdrawal"
	TxKindFx              TxKind = "fx"
	TxKindFxForward       TxKind = "fx_forward"
	TxKindFxSwap          TxKind = "fx_swap"
	TxKindCorporateAction TxKind = "corporate_action"
	TxKindTransferIn      TxKind = "transfer_in"
	TxKindTransferOut     TxKind = "transfer_out"
	TxKindJournal         TxKind = "journal"
	TxKindOther           TxKind = "other"
)

var txKindValues = map[TxKind]struct{}{
	TxKindBuy: {}, TxKindSell: {}, TxKindDividend: {}, TxKindCoupon: {},
	TxKindCapitalGain: {}, TxKindInterest: {}, TxKindStaking: {},
	TxKindContribution: {}, TxKindDistribution: {}, TxKindFee: {}, TxKindTax: {},
	TxKindDeposit: {}, TxKindWithdrawal: {}, TxKindFx: {},
	TxKindFxForward: {}, TxKindFxSwap: {}, TxKindCorporateAction: {}, TxKindTransferIn: {},
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

// FxMode controls how the `-x <currency>` reports resolve the FX
// rate for each row: the nearest rate at or before the row's
// snapshot time (historic) or the most-recent available rate
// (current).
type FxMode string

const (
	FxModeHistoric FxMode = "historic"
	FxModeCurrent  FxMode = "current"
)

func (f FxMode) Valid() bool {
	return f == FxModeHistoric || f == FxModeCurrent
}
