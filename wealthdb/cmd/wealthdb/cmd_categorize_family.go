package main

import (
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
)

// The two families `wealthdb categorize` asks a model about, and
// everything that differs between them.
//
// The protocol does not differ at all: the batching, the rolling
// anchors, the gauntlet, the per-batch persistence, the retry flush
// and the fence are one loop, and that is the point of a descriptor
// rather than a second command. What a family brings is a vocabulary,
// a pair of tables, and the nouns the conversation is conducted in —
// "merchant" and "spending category" on one side, "payer" and "income
// type" on the other.
//
// The fence is NOT here, deliberately. What may leave the machine does
// not depend on the direction of the money, so TransferShaped and
// Uninformative are called the same way for both.
type categorizeFamily struct {
	// name is the positional selector a caller types, and the prefix
	// on every line this family prints.
	name string
	// counterparty is what one row of this family's store is about,
	// in the singular: "merchant", "payer".
	counterparty string
	// vocabularyNoun is what the model is asked to choose, in the
	// words the rest of the surface uses: "spending category",
	// "income type".
	vocabularyNoun string
	// valueColumn is the taxonomy column of the resolution macro and
	// of the store: `spend_detailed`, `income_detailed`.
	valueColumn string

	// The gold surfaces a run reads and writes.
	resolutionMacro string // spend_txn_categories()
	populationMacro string // spend_enrichment_population
	signatureColumn string // merchant_signature
	storeTable      string // spend_merchant_categories
	storeNameColumn string // merchant_name

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
	vocabularyNoun:  "spending category",
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
	vocabularyNoun:  "income type",
	valueColumn:     "income_detailed",
	resolutionMacro: "income_txn_categories()",
	populationMacro: "income_enrichment_population",
	signatureColumn: "payer_signature",
	storeTable:      "income_payer_categories",
	storeNameColumn: "payer_name",
	modelCategories: canonical.ModelIncomeCategories,
	deltaCategories: canonical.DeltaIncomeCategories,
	emittable:       canonical.ModelIncomeDetailed,
	storable:        canonical.ValidIncomeDetailed,
	categorization: func(c *config.Config) *config.SpendingCategorization {
		return c.IncomeCategorization()
	},
	categorizationKey: func(c *config.Config) string { return c.IncomeCategorizationKey() },
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

// outputContract names the three CSV columns in this family's words.
func (f categorizeFamily) outputContract() string {
	return fmt.Sprintf("signature,%s_name,%s", f.counterparty, f.valueColumn)
}
