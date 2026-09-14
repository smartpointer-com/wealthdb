package spending

import "github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"

// The two families this package enriches, and everything that differs
// between them.
//
// One pass writes both. The expensive parts — the signature
// normaliser, the internal-transfer matcher, the precedence lattice,
// the re-assert, the signature-version re-key — are direction-agnostic
// and are written once; what a family brings is a vocabulary, a set of
// tables to write it into, and which tiers have anything to say about
// it. A descriptor rather than a second copy of the pass, because the
// thing most worth protecting is that the ORDER of the tiers is
// written down once: two copies drift, and the drift would be silent
// on whichever side is read less.
//
// The rule for what belongs here: a field, if the two families differ
// by a NAME (a table, a column, a macro); a hook, if they differ by
// BEHAVIOUR. A hook left nil is a tier that family does not have —
// spending's card rule writes an issuer label and income has no
// analogue, so income leaves the label column empty rather than
// carrying a column it would never fill.
type family struct {
	// name prefixes every error this family's phases raise, and names
	// the block `wealthdb load` prints for it.
	name string

	// populationMacro is the layered macro the pass reads its
	// candidates from — what this family MAY write a verdict for,
	// before any tier has decided anything.
	populationMacro string

	// scopeTable is the account scope the config stamps into gold.
	// Separate per family: an account excluded from spending because
	// its outflows double-count something is not thereby an account
	// whose inflows are not income.
	scopeTable string

	// The overlay: one row per transaction the pass reached, rewritten
	// whole on every run. labelCol is empty for a family with no label
	// to write.
	overlayTable string
	signatureCol string
	detailedCol  string
	providerCol  string
	labelCol     string

	// The verdict store: global, keyed by signature, written by the
	// model tier rather than by this pass, and re-keyed by it when
	// SignatureVersion moves.
	storeTable        string
	storeSignatureCol string
	storeNameCol      string
	storeDetailedCol  string

	// valid reports whether a value may be stored for this family. It
	// gates the pins ledger, which is the one input a person writes by
	// hand.
	valid func(string) bool

	// builtinRule is the engine's own rule tier: structural facts
	// about the product's account graph that no provider can know. It
	// returns the value it places and, where the family has one, a
	// label describing the counterparty of the verdict it placed.
	builtinRule func(signature, counterparty, description, providerCategory string) (detailed, label string, ok bool)

	// providerCategory translates the source's own filing of a row,
	// and providerClaims says whether that translation is a verdict or
	// merely a record. Both are per (silver kind, account kind): a
	// bank files an account's rows by booking type and a card's by
	// merchant category, and only the account kind says which.
	providerCategory func(silverKind, accountKind, providerCategory string) (detailed string, ok, drift bool)
	providerClaims   func(silverKind, accountKind, detailed string) bool

	// emitOutsidePopulation says whether a matched leg the population
	// does not hold still earns an overlay row.
	//
	// Spending needs it: a card payment and the deposit half of a
	// funding wire are own-account moves that the spending population
	// excludes by kind, and both halves of a pair must carry the
	// verdict or a later widening of the population would start
	// counting one of them as real money movement. Income does not: a
	// matched deposit leg is IN the income population already, and a
	// matched withdrawal leg is spending's question, not income's.
	emitOutsidePopulation bool
}

// internalTransfer is what the matcher writes on both legs of a pair,
// in either family. One row, one meaning, read from either side — the
// delta is FamilyBoth in the taxonomy for exactly this reason.
const internalTransfer = canonical.SpendDetailedInternalTransfer

// spendingFamily is the outflow side: what was bought, and from whom.
var spendingFamily = family{
	name:              "spending",
	populationMacro:   "spend_enrichment_population",
	scopeTable:        "spend_account_scope",
	overlayTable:      "spend_txn_enrichment",
	signatureCol:      "merchant_signature",
	detailedCol:       "spend_detailed",
	providerCol:       "provider_spend_detailed",
	labelCol:          "merchant_label",
	storeTable:        "spend_merchant_categories",
	storeSignatureCol: "merchant_signature",
	storeNameCol:      "merchant_name",
	storeDetailedCol:  "spend_detailed",
	valid:             canonical.ValidSpendDetailed,
	builtinRule:       RuleCategory,
	providerCategory:  ProviderCategory,
	providerClaims:    ProviderCategoryClaims,

	emitOutsidePopulation: true,
}

// familyInput is everything one family's phases read that the pass
// computed once: the shared matcher's verdicts and its narratives, the
// silver kinds, and this family's own config.
type familyInput struct {
	include, exclude map[string][]string
	rules            []Rule
	pins             []Pin
	kinds            map[string]string
	matched          map[txKey]bool
	pool             map[txKey]candidate
	now              int64
}

// incomeFamily is the inflow side: what was received, and from whom.
//
// Three differences from its twin, each a decision rather than an
// omission. There is no label column, because the issuer label is the
// card rule's alone. There is one built-in rule, because almost every
// structural inflow fact is the matcher's already (IncomeRuleCategory).
// And a matched leg outside the population earns no row, because this
// family's half of every pair is already in its population.
var incomeFamily = family{
	name:              "income",
	populationMacro:   "income_enrichment_population",
	scopeTable:        "income_account_scope",
	overlayTable:      "income_txn_enrichment",
	signatureCol:      "payer_signature",
	detailedCol:       "income_detailed",
	providerCol:       "provider_income_detailed",
	labelCol:          "",
	storeTable:        "income_payer_categories",
	storeSignatureCol: "payer_signature",
	storeNameCol:      "payer_name",
	storeDetailedCol:  "income_detailed",
	valid:             canonical.ValidIncomeDetailed,
	builtinRule:       IncomeRuleCategory,
	providerCategory:  ProviderIncomeCategory,
	providerClaims:    ProviderIncomeCategoryClaims,

	emitOutsidePopulation: false,
}
