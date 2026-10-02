package spending

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

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
// BEHAVIOUR. An empty labelCol is a column that family does not have —
// spending's card rule writes an issuer label and income has no
// analogue, so income leaves it out of the insert rather than carrying
// a column it would never fill.
//
// The validity predicates are deliberately NOT here. They gate the
// pins ledger and the config rules, and both are read at CONFIG load,
// where the family is already known from the key that was written; a
// copy on the descriptor would be a second place for them to disagree.
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

	// builtinRule is the engine's own rule tier: structural facts
	// about the product's account graph that no provider can know. It
	// returns the value it places, a label describing the counterparty
	// of that verdict where the family has one, and the cashflow class
	// the rule stands for where it placed an own-account move without
	// pairing anything.
	//
	// It takes the transaction KIND because one family's rules are
	// gated on it: income's single built-in reads a narrative, and on
	// this side a narrative may only speak for the one kind the floor
	// does not (rules.go, IncomeRuleCategory). Spending's are ungated —
	// an outflow's kind says how money left, never what it bought.
	//
	// `filed` is the provider's filing of the row as the provider tier
	// translated it, whether or not it claimed the row; empty where it has
	// no translation. A rule may stand down for it
	// (spendRule.yieldsToProvider).
	builtinRule func(kind, signature, counterparty, description, providerCategory, filed string) (detailed, label, farClass string, ok bool)

	// providerCategory translates the source's own filing of a row,
	// and providerClaims says whether that translation is a verdict or
	// merely a record. Both are per (silver kind, account kind): a
	// bank files an account's rows by booking type and a card's by
	// merchant category, and only the account kind says which.
	providerCategory func(silverKind, accountKind, providerCategory string) (detailed string, ok, drift bool)
	providerClaims   func(silverKind, accountKind, detailed string) bool

	// farCols are the columns recording the OTHER end of an own-account
	// move — the partner leg's account, and the class a rule stands for
	// where it placed the move without pairing anything. Empty on the
	// family that does not write them, exactly as an empty labelCol is
	// a column that family does not have.
	//
	// Spending's alone, for the same reason emitOutsidePopulation is:
	// it writes a row for every matched leg whether or not the leg is
	// in its own population, so a card bill's own leg — a pair's far
	// side as often as not — has a row here to carry the fact and none
	// on the income overlay.
	farCols []string

	// investingValue is this family's one verdict whose cash flow
	// section is `investing`, and so the only one an exposure may
	// accompany. The pass reads it to count the backlog: rows placed
	// there that still say nothing about what the capital went into.
	investingValue string
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
	builtinRule:       spendingBuiltinRule,
	investingValue:    canonical.SpendDetailedInvestment,
	providerCategory:  ProviderCategory,
	providerClaims:    ProviderCategoryClaims,
	farCols: []string{
		"far_silver_source_id", "far_account_external_id", "far_class",
	},

	emitOutsidePopulation: true,
}

// familyInput is everything one family's phases read that the pass
// computed once: the shared matcher's verdicts and its narratives, the
// silver kinds, and this family's own config.
type familyInput struct {
	include, exclude map[string][]string
	rules            []Rule
	// farRules are the OTHER family's rules that name a far account,
	// handed to the family that owns the far columns: an inbound leg
	// an income rule places to a declared account is that family's
	// verdict, but where it came from is written on the spending
	// overlay, which is the one overlay a far account has a column on.
	// Empty for the family without far columns.
	farRules []Rule
	// otherPins are the other family's pins, for the same reason: a row
	// that family pinned is that family's whole answer, and a far
	// account written beside it would name a move the pin says is
	// something else.
	otherPins []Pin
	pins      []Pin
	kinds     map[string]string
	matched   map[txKey]gold.TransferLeg
	// stated is the far account the SOURCE named, for the rows the
	// matcher could not pair. Read once for both families, as `matched`
	// is and for the same reason.
	stated map[txKey]farAccount
	pool   map[txKey]candidate
	now    int64
}

// incomeFamily is the inflow side: what was received, and from whom.
//
// Four differences from its twin, each a decision rather than an
// omission. There is no label column, because the issuer label is the
// card rule's alone. There is one built-in rule, because almost every
// structural inflow fact is the matcher's already (IncomeRuleCategory).
// A matched leg outside the population earns no row, because this
// family's half of every pair is already in its population — and for
// that same reason there are no far-account columns: the overlay that
// holds every matched leg is the one that can record where each went.
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
	builtinRule:       incomeBuiltinRule,
	investingValue:    canonical.IncomeDetailedCapitalReturn,
	providerCategory:  ProviderIncomeCategory,
	providerClaims:    ProviderIncomeCategoryClaims,

	emitOutsidePopulation: false,
}

// spendingBuiltinRule adapts the outflow rule table to the kind-taking
// hook. The outflow rules read narratives and account graphs, never the
// kind: a card bill is a card bill whether the adapter kinded it
// `card_payment` or `withdrawal`, and gating them would make the rule
// tier depend on how well each source kinds its rows.
func spendingBuiltinRule(_, signature, counterparty, description, providerCategory, filed string) (detailed, label, farClass string, ok bool) {
	return rulePlacement(builtinRules, signature, counterparty, description, providerCategory, filed)
}

// incomeBuiltinRule adapts IncomeRuleCategory to the same hook. It
// places no label and no far class: the label column is the card
// rule's alone, and the one inflow built-in names cash paid in over a
// counter, which is not an own-account move at all.
func incomeBuiltinRule(kind, signature, counterparty, description, providerCategory, _ string) (detailed, label, farClass string, ok bool) {
	detailed, label, ok = IncomeRuleCategory(kind, signature, counterparty, description, providerCategory)
	return detailed, label, "", ok
}
