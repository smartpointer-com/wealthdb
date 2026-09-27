package main

import (
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

// The two families `wealthdb categorize` asks a model about, and
// everything that differs between them.
//
// The protocol does not differ at all: the batching, the rolling
// anchors, the gauntlet, the per-batch persistence, the retry flush
// and the fence are one loop, and that is the point of a descriptor
// rather than a second command. What a family brings is a vocabulary,
// a pair of tables, and the nouns the conversation is conducted in —
// "merchant" on one side, "payer" on the other.
//
// Three members branch on `name` rather than carrying a field:
// systemPrompt and promptPreamble are paragraphs rather than
// substitutions — an income prompt says "money ARRIVING", which no
// noun swap produces — and the canaries are one family's check. A
// field holding a whole paragraph would be a worse way to say the same
// thing.
//
// The fence is NOT here, deliberately. What may leave the machine does
// not depend on the direction of the money, so TransferShaped and
// Uninformative are called the same way for both.
type categorizeFamily struct {
	// name is the positional selector a caller types, and the prefix
	// on every line this family prints.
	name string
	// counterparty is what one row of this family's store is about,
	// in the singular: "merchant", "payer". It is also the prefix of
	// the two CSV columns the model must emit, so it is what the
	// prompt's output contract, its candidate heading and the
	// gauntlet's rejection are all spelled from.
	counterparty string
	// nameExample is a synthetic name of this family's counterparty,
	// shown to the model as the casing it must answer in. It is never
	// a real name, and never one from the deployment's own data: the
	// prompt is a constant, and a name taken from gold would put one
	// household's counterparty in every other's prompt.
	nameExample string
	// valueColumn is the taxonomy column of the resolution macro and
	// of the store: `spend_detailed`, `income_detailed`.
	valueColumn string

	// The gold surfaces a run reads and writes.
	resolutionMacro string // spend_txn_categories()
	populationMacro string // spend_enrichment_population
	signatureColumn string // merchant_signature
	storeTable      string // spend_merchant_categories
	storeNameColumn string // merchant_name

	// candidateKinds restricts candidacy to these transaction kinds,
	// in EVERY backlog mode. Empty means unrestricted.
	//
	// It exists because the two families ask a model different
	// questions. Spending asks who was paid, which is a question about
	// every outflow whatever its kind. Income asks what KIND of income
	// a receipt is — a claim about the transaction — and the data
	// already makes that claim on every admitted kind but one. So the
	// income side asks about `deposit` and nothing else: a dividend
	// the kind floor placed has a signature and no stored verdict, and
	// a question about it is a round trip with a known answer whose
	// only possible outcome is a worse one.
	//
	// It is the second of two independent guards, the way the fence
	// and the context level are two. Migration 0073 reads the floor
	// OVER the store, so a verdict on a floor-kind signature cannot
	// change what that row resolves to; this keeps the verdict from
	// being bought at all. Either alone would hold; both are cheap.
	candidateKinds []string

	// modelCategories is the closed vocabulary the prompt offers, and
	// deltaCategories the list it forbids by name. Both are copies.
	modelCategories func() []canonical.SpendCategory
	deltaCategories func() []canonical.SpendCategory
	// emittable admits exactly what a model verdict may carry;
	// storable is the wider check, used only to tell a DELTA from a
	// value that is simply not of this family — the two produce
	// different feedback to the model.
	emittable func(string) bool
	storable  func(string) bool

	// categorization resolves the config block this family's model
	// tier runs on, and categorizationKey names the config key an
	// error about it should point at — which for income is the block
	// it INHERITED when it has none of its own.
	categorization    func(*config.Config) *config.SpendingCategorization
	categorizationKey func(*config.Config) string
}

var spendingCategorizeFamily = categorizeFamily{
	name:            "spending",
	counterparty:    "merchant",
	nameExample:     "Corner Market",
	valueColumn:     "spend_detailed",
	resolutionMacro: "spend_txn_categories()",
	populationMacro: "spend_enrichment_population",
	signatureColumn: "merchant_signature",
	storeTable:      "spend_merchant_categories",
	storeNameColumn: "merchant_name",
	modelCategories: canonical.ModelSpendCategories,
	deltaCategories: canonical.DeltaSpendCategories,
	emittable:       canonical.ModelSpendDetailed,
	storable:        canonical.ValidSpendDetailed,
	categorization: func(c *config.Config) *config.SpendingCategorization {
		return c.SpendCategorization()
	},
	categorizationKey: func(*config.Config) string { return "spending.categorization" },
}

var incomeCategorizeFamily = categorizeFamily{
	name:            "income",
	counterparty:    "payer",
	nameExample:     "Blue Harbour Payroll",
	valueColumn:     "income_detailed",
	resolutionMacro: "income_txn_categories()",
	populationMacro: "income_enrichment_population",
	signatureColumn: "payer_signature",
	storeTable:      "income_payer_categories",
	storeNameColumn: "payer_name",
	candidateKinds:  []string{"deposit"},
	modelCategories: canonical.ModelIncomeCategories,
	deltaCategories: canonical.DeltaIncomeCategories,
	emittable:       canonical.ModelIncomeDetailed,
	storable:        canonical.ValidIncomeDetailed,
	categorization: func(c *config.Config) *config.SpendingCategorization {
		return c.IncomeCategorization()
	},
	categorizationKey: func(c *config.Config) string { return c.IncomeCategorizationKey() },
}

// categorizeValueFlags are the flag tokens that consume the next arg on
// the two categorize commands, so the optional family positional may
// appear before or after flags — the idiom every other command with a
// positional already uses. Without the reordering, Go's flag package
// stops at the first non-flag token and every flag after the family
// name reads as another positional.
var categorizeValueFlags = map[string]bool{
	"--batch": true, "--max-attempts": true, "--max-anchors": true,
	"-f": true, "--format": true, "-d": true, "--detailed": true,
	"--forget": true,
}

// categorizeFamilies is the order a run with no positional walks:
// spending, then income. Two plans, two summaries, one model.
var categorizeFamilies = []categorizeFamily{spendingCategorizeFamily, incomeCategorizeFamily}

// resolveCategorizeFamilies reads the optional positional selector.
// Nothing named runs both; a name runs that one alone.
func resolveCategorizeFamilies(arg string) ([]categorizeFamily, bool) {
	if arg == "" {
		return categorizeFamilies, true
	}
	for _, f := range categorizeFamilies {
		if f.name == arg {
			return []categorizeFamily{f}, true
		}
	}
	return nil, false
}

// candidateKindFilter renders candidateKinds as a SQL predicate on the
// population's `kind`, or the empty string when the family admits
// every kind. The values are engine constants, never input, so they
// are spelled into the query beside the other family names rather than
// bound — a bound list would need a variadic arg list threaded through
// two call sites for no safety the constants do not already have.
func (f categorizeFamily) candidateKindFilter() string {
	if len(f.candidateKinds) == 0 {
		return ""
	}
	quoted := make([]string, len(f.candidateKinds))
	for i, k := range f.candidateKinds {
		quoted[i] = "'" + k + "'"
	}
	return "\n   AND p.kind IN (" + strings.Join(quoted, ", ") + ")"
}

// isDelta reports whether a model emitted one of this family's delta
// values — a storable value the model may never choose. It is told
// apart from "not a value at all" because the two earn different
// feedback: a delta is refused on principle and the model is told so,
// while nonsense is a spelling error.
func (f categorizeFamily) isDelta(s string) bool {
	folded := strings.ToLower(strings.TrimSpace(s))
	return f.storable(folded) && !f.emittable(folded)
}

// systemPrompt is the conversation's opening, in this family's nouns.
func (f categorizeFamily) systemPrompt() string {
	if f.name == "income" {
		return `You are a personal-finance data assistant. You are given payer signatures — short upper-cased fragments of bank statement narratives describing money RECEIVED — and you name the payer and pick the kind of income it is from a fixed taxonomy. You output CSV only — no prose, no markdown, no explanations.`
	}
	return `You are a personal-finance data assistant. You are given merchant signatures — short upper-cased fragments of card and bank statement narratives — and you name the merchant and pick its spending category from a fixed taxonomy. You output CSV only — no prose, no markdown, no explanations.`
}

// promptPreamble is the first paragraph of the user message: what the
// signatures are, and what the model is being asked for.
func (f categorizeFamily) promptPreamble() string {
	if f.name == "income" {
		return `Payer signatures below come from bank and brokerage statements, and each one describes money ARRIVING. For each, emit the payer's real-world name and the single best income type from the taxonomy.

Taxonomy — income_detailed values you may emit, with the primary bucket each belongs to and what it covers:
`
	}
	return `Merchant signatures below come from card and bank statements. For each one, emit the merchant's real-world name and the single best category from the taxonomy.

Taxonomy — spend_detailed values you may emit, with the primary bucket each belongs to and what it covers:
`
}

// plural is this family's counterparty in the plural, lower case:
// "merchants", "payers". A caller that needs it title-cased upper-cases
// the first byte itself — both of the current ones are headings, and a
// second field for the capitalisation would be a field to keep true.
func (f categorizeFamily) plural() string {
	return f.counterparty + "s"
}

// outputContract names the three CSV columns in this family's words.
//
// It is ONE string with two readers — the prompt's output-format block
// and the gauntlet's wrong-arity rejection — so a model that is told
// what to emit and then told it emitted the wrong thing hears the same
// column names both times. They used to differ in the first column
// (`signature` in the rejection, `merchant_signature` in the prompt),
// which is the smaller half of the same defect that had an income
// prompt asking for merchant columns.
func (f categorizeFamily) outputContract() string {
	return fmt.Sprintf("%[1]s_signature,%[1]s_name,%[2]s", f.counterparty, f.valueColumn)
}
