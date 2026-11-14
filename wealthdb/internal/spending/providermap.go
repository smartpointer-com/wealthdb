package spending

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The provider-category tier: the provider's own filing of a row, carried
// into gold verbatim as `transactions.provider_category`, translated into
// the taxonomy by a map per silver KIND (the vocabulary belongs to the
// provider's product, not to a login).
//
// A CATEGORICAL vocabulary (a card issuer's) files every row, so a value the
// map does not hold is unreviewed DRIFT: counted, never guessed into a
// neighbour, and the row falls through to the model tier. A BOOKING-TYPE
// vocabulary (a bank's) mostly names a rail, so an unmapped value is the
// normal case and is not counted. A translation may be a delta where the
// provider's word names the movement rather than a merchant — cash out of an
// ATM, a bill paid to a card. docs/SPENDING.md §3 argues both shapes.

// ProvenanceProvider tags an enrichment row placed by this tier.
const ProvenanceProvider = "provider"

// providerVocabulary is one provider's translation table and the shape
// of the vocabulary it translates.
type providerVocabulary struct {
	// translations maps the provider's value, spelled as the provider
	// spells it, to a spend_detailed value. Lookups fold case and
	// surrounding space, so one entry covers every era's spelling of
	// a value and a cosmetic change on the provider's side does not
	// silently unmap it.
	translations map[string]string
	// untranslatable are the values a categorical vocabulary reviewed
	// and deliberately left to the model tier: the provider's own "I
	// could not place this" bucket — an issuer's literal "Other" — and
	// any value naming a MOVEMENT rather than a line of business, such
	// as a bank's catch-all for the card rows that transferred money
	// instead of buying something. They are REVIEWED, so a miss on one
	// is not drift; and they are not translated, because a row the
	// provider could not place, or placed under a movement, is exactly
	// the row the model tier can read from the descriptor, and any
	// verdict here would pre-empt it. Without this a categorical
	// vocabulary has only two ways to treat such a value, and both are
	// wrong: translate it and lose the row to a bucket, or omit it and
	// inflate a counter that is supposed to mean "the issuer said
	// something this build does not understand".
	untranslatable map[string]bool
	// categorical marks a vocabulary that files every row under one of
	// its categories, so a value outside the table is drift worth
	// counting. False for a booking-type vocabulary, where a miss is a
	// rail, and for one whose values are verdicts rather than buckets
	// (syntheticCategories).
	categorical bool
	// income is the same translation for the INCOME family, and it is
	// a second map rather than a second lookup into the first because
	// a provider's value can mean different things by direction. UBS
	// books both halves of an account's interest settlement under one
	// booking type: charged, it is a finance cost, and credited, it is
	// interest earned. One map would have to pick.
	//
	// The bank vocabularies carry one. So does Plaid's taxonomy, which
	// files money arriving under categories of its own. So does the
	// synthetic kind's, which is the taxonomy itself. A card issuer's does
	// not. It files what a MERCHANT sells, and a merchant category says
	// nothing about money arriving. The rows a card books inbound are
	// refunds, which are spending's to net.
	income map[string]string
	// incomeUntranslatable is `untranslatable` for the income side:
	// values reviewed and left to the model tier rather than missing.
	incomeUntranslatable map[string]bool
}

// providerVocabularies is the registry. A source with no entry
// contributes no provider verdicts at all, which is the correct
// default: a source that publishes no categories, or whose vocabulary
// has never been reviewed, must not be read as if it had.
//
// The key is the silver kind, optionally narrowed to one account kind
// with a "/" — `"ubs/card"` before `"ubs"`. A source whose products
// publish different vocabularies needs the narrower key: UBS files a
// bank account's rows by booking type, which is a payment rail, and a
// card's by merchant category, which is an MCC description. One map
// per source would have to call both the same shape, and the shape is
// what decides whether an unmapped value is drift.
//
// Lookup falls back from the narrow key to the broad one, so a source
// with one vocabulary needs only the broad entry, and an account kind
// with no entry of its own inherits it.
var providerVocabularies = map[string]providerVocabulary{
	"chase": {translations: chaseCardCategories, categorical: true},
	"amex": {translations: amexCardCategories, untranslatable: amexUncategorized,
		categorical: true},
	"ubs": {translations: ubsBookingTypes, income: ubsIncomeBookingTypes, categorical: false},
	"ubs/card": {translations: ubsCardCategories,
		untranslatable: ubsCardMoneyMovement, categorical: true},
	"raiffeisen_at": {translations: raiffeisenCategories,
		income:               raiffeisenIncomeCategories,
		untranslatable:       raiffeisenUncategorized,
		incomeUntranslatable: raiffeisenIncomeUncategorized,
		categorical:          true},
	"synthetic": {translations: syntheticCategories, income: syntheticIncomeCategories,
		categorical: false},
	"plaid": {translations: plaidCategories,
		income:               plaidIncomeCategories,
		untranslatable:       plaidUncategorized,
		incomeUntranslatable: plaidIncomeUncategorized,
		categorical:          true},
}

// foldedProviderVocabularies is providerVocabularies re-keyed on the
// case-folded, space-trimmed value, built once at init so the per-row
// lookup is a single map hit.
var foldedProviderVocabularies = foldProviderVocabularies(providerVocabularies)

func foldProviderVocabularies(in map[string]providerVocabulary) map[string]providerVocabulary {
	out := make(map[string]providerVocabulary, len(in))
	for kind, v := range in {
		out[kind] = providerVocabulary{
			translations:         foldTranslations(v.translations),
			untranslatable:       foldSkips(v.untranslatable),
			income:               foldTranslations(v.income),
			incomeUntranslatable: foldSkips(v.incomeUntranslatable),
			categorical:          v.categorical,
		}
	}
	return out
}

func foldTranslations(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for value, detailed := range in {
		out[providerCategoryKey(value)] = detailed
	}
	return out
}

func foldSkips(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for value := range in {
		out[providerCategoryKey(value)] = true
	}
	return out
}

func providerCategoryKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ProviderCategory translates one provider category to a detailed
// taxonomy value.
//
// The three results are distinct outcomes, and the caller must treat
// them as such:
//
//	(value, true, false)  translated
//	("", false, true)     a miss in a categorical vocabulary — this
//	                      build does not understand the issuer's word:
//	                      count it as drift and fall through
//	("", false, false)    nothing to count — the kind publishes no
//	                      mapped vocabulary, the row carries no value,
//	                      the value is one the vocabulary reviewed and
//	                      left untranslatable, or the vocabulary is a
//	                      list of booking types and this value names a
//	                      rail
//
// Separating the last two keeps the unmapped counter meaningful:
// without it every uncategorised row from every non-card source, and
// every payment order at a bank, would inflate a number that is
// supposed to mean "the issuer said something this build does not
// understand".
func ProviderCategory(silverKind, accountKind, providerCategory string) (detailed string, ok, drift bool) {
	v, mapped := vocabularyFor(silverKind, accountKind)
	if !mapped {
		return "", false, false
	}
	return v.translate(v.translations, v.untranslatable, providerCategory)
}

// translate looks one provider category up in one side's translations,
// with that side's reviewed-and-left set as the escape.
func (v providerVocabulary) translate(translations map[string]string, untranslatable map[string]bool, providerCategory string) (detailed string, ok, drift bool) {
	key := providerCategoryKey(providerCategory)
	if key == "" {
		// No value published for this row — not a vocabulary gap.
		return "", false, false
	}
	if untranslatable[key] {
		// A value this vocabulary reviewed and left alone: not drift,
		// and left for the model tier.
		return "", false, false
	}
	detailed, ok = translations[key]
	return detailed, ok, !ok && v.categorical
}

// ProviderIncomeCategory is ProviderCategory over the income half of
// the same vocabulary. Same drift accounting, same reviewed-value
// escape, and the same silence where a source publishes no vocabulary
// for the family: a card issuer has no income map, so an inbound card
// row translates to nothing here and is not counted as drift.
func ProviderIncomeCategory(silverKind, accountKind, providerCategory string) (detailed string, ok, drift bool) {
	v, mapped := vocabularyFor(silverKind, accountKind)
	if !mapped || v.income == nil {
		return "", false, false
	}
	return v.translate(v.income, v.incomeUntranslatable, providerCategory)
}

// ProviderIncomeCategoryClaims is ProviderCategoryClaims for the income
// vocabulary, declining a categorical vocabulary's catch-all for the
// same reason: a bank that could place a receipt no better than "other
// income" has not named the payer, and the tier that reads a payer is
// still to come.
func ProviderIncomeCategoryClaims(silverKind, accountKind, detailed string) bool {
	v, mapped := vocabularyFor(silverKind, accountKind)
	if !mapped {
		return false
	}
	return !(v.categorical && canonical.CatchAllIncomeDetailed(detailed))
}

// ProviderCategoryClaims reports whether a translated value should CLAIM
// the row or merely be recorded on it.
//
// A catch-all is not a verdict — but only where the vocabulary is
// CATEGORICAL. The two shapes fail differently:
//
//   - A card issuer's vocabulary files a merchant's line of business.
//     A catch-all there means the issuer could not place the MERCHANT,
//     and the row still carries the merchant's name in its descriptor —
//     which the model tier reads and the issuer never saw. Claiming
//     would pre-empt the one tier that can do better, so the value is
//     recorded and the row declined.
//
//   - A bank's booking-type vocabulary names how the entry was BOOKED.
//     Its catch-alls are not shrugs: "RENTAL FEE SAFE BOX" resolving to
//     BANK_FEES_OTHER_BANK_FEES is the bank saying the movement was a
//     fee of its own, which is the most any tier will ever know about
//     that row. There is no merchant name to read — and the signature
//     of such a row is fenced out of model candidacy anyway, by
//     Uninformative or by FilingOnly — so declining would not defer the
//     verdict to a better tier, it would discard it. The value claims.
func ProviderCategoryClaims(silverKind, accountKind, detailed string) bool {
	v, mapped := vocabularyFor(silverKind, accountKind)
	if !mapped {
		return false
	}
	return !(v.categorical && canonical.CatchAllSpendDetailed(detailed))
}

// vocabularyFor resolves the product-scoped vocabulary, falling back to
// the source-wide one.
func vocabularyFor(silverKind, accountKind string) (providerVocabulary, bool) {
	if accountKind != "" {
		if v, ok := foldedProviderVocabularies[silverKind+"/"+accountKind]; ok {
			return v, true
		}
	}
	v, ok := foldedProviderVocabularies[silverKind]
	return v, ok
}
