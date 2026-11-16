// Package canonical defines the data shapes that flow between the
// silver adapter layer and the gold-layer writer. The enums here
// constrain the categorical columns of the gold schema; see
// docs/DESIGN.md §7.2 for the SQL-side definitions.
package canonical

import "slices"

// AssetClass is the exposure dimension of the instrument taxonomy —
// what moves a holding's value. The recognised exposure values and
// the Valid method live in taxonomy.go (docs/TAXONOMY.md §2).
//
// A source's own class words (equity, bond, fund, …) are mapped to an
// (exposure, vehicle) pair at the emit boundary (TAXONOMY.md §4).
// Unrecognised source codes fall through to AssetClassOther
// (docs/DESIGN.md §6.8).
type AssetClass string

const (
	AssetClassMetal  AssetClass = "metal"
	AssetClassCrypto AssetClass = "crypto"
	// AssetClassPrivateEquity covers direct private-company equity and
	// equity-comp (shares, options, RSUs/RSAs, warrants): illiquid,
	// non-quotable holdings apart from listed securities.
	AssetClassPrivateEquity AssetClass = "private_equity"
	// AssetClassRealEstate is a directly-held property.
	AssetClassRealEstate AssetClass = "real_estate"
	AssetClassOther      AssetClass = "other"
)

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
	// (an exchange, a hardware wallet, a staking provider, …)
	// doesn't carry a reliable exchange-vs-self-custody signal,
	// and the rest of the stack values the holding the same way
	// either way. The finer-grained `crypto_exchange` /
	// `crypto_self_custody` values stay reserved for any future
	// adapter that does surface the distinction at source.
	AccountKindCrypto            AccountKind = "crypto"
	AccountKindCryptoExchange    AccountKind = "crypto_exchange"
	AccountKindCryptoSelfCustody AccountKind = "crypto_self_custody"
	// AccountKindMortgage is a real-property-backed liability
	// account. Outstanding principal lives as a negative-value
	// position (real_estate × mortgage) on this account; interest /
	// principal payments flow in as transactions debited from a
	// regular cash account with counterparty "Maturity".
	AccountKindMortgage AccountKind = "mortgage"
	// AccountKindCard is a revolving-credit liability — a credit-card
	// account. Unlike AccountKindMortgage it carries no position: the
	// outstanding balance is negative cash (the margin-debit
	// precedent), with the adapter negating the provider's
	// owed-positive figure. Spend, refunds, payments and rewards land
	// as transactions (TxKindPurchase / TxKindRefund /
	// TxKindCardPayment / TxKindReward). A card is not a conduit and
	// gets no conduit-returns treatment.
	AccountKindCard AccountKind = "card"
	// AccountKindDonorAdvisedFund is a charitable giving account
	// (e.g. a Fidelity Charitable Giving Account): assets are
	// irrevocably donated into the fund, invested in sponsor pools,
	// and granted out to charities over time. Modeled as its own
	// container kind (paired with TaxWrapperCharitable) so
	// deployments can include or exclude DAF balances from
	// net-worth calculations by kind.
	AccountKindDonorAdvisedFund AccountKind = "donor_advised_fund"
	AccountKindOther            AccountKind = "other"
)

// accountKindValues are the kinds of the account kind table
// (kindroles.go).
var accountKindValues = func() map[AccountKind]struct{} {
	m := make(map[AccountKind]struct{}, len(accountKinds))
	for _, r := range accountKinds {
		m[r.Kind] = struct{}{}
	}
	return m
}()

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

	// US charitable. The charitable wrapper marks assets held for
	// charitable purposes (donor-advised funds foremost — the
	// container itself is AccountKindDonorAdvisedFund). The wrapper
	// names the tax treatment rather than duplicating the account kind.
	TaxWrapperCharitable TaxWrapper = "charitable"

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
	TaxWrapperCharitable:    {},
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
	TxKindDistribution TxKind = "distribution"
	TxKindFee          TxKind = "fee"
	TxKindTax          TxKind = "tax"
	TxKindDeposit      TxKind = "deposit"
	TxKindWithdrawal   TxKind = "withdrawal"
	// Card kinds, all seen from the card account's own perspective
	// (its balance is negative cash — see AccountKindCard).
	// TxKindPurchase is card spend, TxKindRefund a merchant credit
	// back. TxKindCardPayment is the payment that pays the card down;
	// its counter-leg is a TxKindWithdrawal on the funding cash
	// account. TxKindReward is a rewards credit — a taxonomy value
	// only, emitted by no source today.
	TxKindPurchase        TxKind = "purchase"
	TxKindRefund          TxKind = "refund"
	TxKindCardPayment     TxKind = "card_payment"
	TxKindReward          TxKind = "reward"
	TxKindFx              TxKind = "fx"
	TxKindFxForward       TxKind = "fx_forward"
	TxKindFxSwap          TxKind = "fx_swap"
	TxKindCorporateAction TxKind = "corporate_action"
	TxKindTransferIn      TxKind = "transfer_in"
	TxKindTransferOut     TxKind = "transfer_out"
	TxKindJournal         TxKind = "journal"
	TxKindOther           TxKind = "other"
)

// txKindValues are the kinds of the transaction kind table
// (kindroles.go).
var txKindValues = func() map[TxKind]struct{} {
	m := make(map[TxKind]struct{}, len(txKindRows))
	for _, r := range txKindRows {
		m[r.Kind] = struct{}{}
	}
	return m
}()

func (t TxKind) Valid() bool {
	_, ok := txKindValues[t]
	return ok
}

// TxKinds returns every kind, sorted, so a test walking them meets a
// new kind with no edit.
func TxKinds() []TxKind {
	out := make([]TxKind, 0, len(txKindValues))
	for k := range txKindValues {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
