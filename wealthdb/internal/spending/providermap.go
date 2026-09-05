package spending

import (
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The provider-category tier.
//
// Providers already file the rows they publish, and that filing is
// carried into gold verbatim as `transactions.provider_category`: a
// card issuer's spend category ("Groceries", "Travel"), a bank's
// booking type ("ATM WITHDRAWAL", "NTRF"). It is free, it arrives with
// the row, and it is right often enough to be worth reading — but it
// is the PROVIDER's vocabulary, not the taxonomy the reports group by,
// so it has to be translated.
//
// The maps are per silver KIND rather than per source, because the
// vocabulary belongs to the provider's product: two logins at the same
// issuer publish the same words, and a second source of the same kind
// should not need its own table.
//
// Two shapes of vocabulary, and the shape decides what a value the map
// does not hold MEANS:
//
//   - a CATEGORICAL vocabulary (a card issuer's) files every row under
//     a spend category. The map is narrower than what the issuer can
//     publish — seasonal categories, product-specific ones, categories
//     that appear the first time a certain merchant type is used — so
//     a value it does not hold is one this build has not reviewed:
//     DRIFT, counted, and the row falls through to the model tier. It
//     is never guessed into a plausible neighbour: a wrong category is
//     invisible in a report, while an uncategorised row is visible as
//     backlog and an unmapped-value count is visible as drift.
//   - a BOOKING-TYPE vocabulary (a bank's) names how the entry was
//     booked. Most values name a payment rail — a payment order, a
//     direct debit, a credit — and a rail says nothing about what the
//     money bought; only the types whose meaning is the movement
//     itself translate. A value the map does not hold is the normal
//     case, not drift: it is not counted, and the row falls through
//     exactly as an unmapped categorical value does.
//
// A translation may be a delta as well as a vendored value. The
// provider's own word can name the MOVEMENT rather than a merchant's
// line of business — cash out of an ATM, a conversion between the
// holder's own currency accounts, a bill paid to a card — and then the
// delta is the only honest verdict (docs/SPENDING.md §3). The
// vendored-only restriction is the model tier's alone: a provider
// verdict is per transaction, placed from the provider's structured
// filing of that row, and outranked by every tier above it.

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
	// categorical marks a vocabulary in which every value is a spend
	// category, so a value outside the table is drift worth counting.
	// False for a booking-type vocabulary, where a miss is a rail.
	categorical bool
}

// chaseCardCategories translates the Chase card vocabulary.
//
// Where the provider's bucket is coarser than the taxonomy's, the
// translation targets that primary's OTHER_* value rather than its
// most common member: "Shopping" is not evidence of clothing any more
// than of electronics, and the model tier can refine a merchant later
// from its name. Where the provider's bucket is precise — "Groceries",
// "Education" — the specific value is used.
var chaseCardCategories = map[string]string{
	"Shopping":              "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Food & Drink":          "FOOD_AND_DRINK_RESTAURANT",
	"Entertainment":         "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Travel":                "TRAVEL_OTHER_TRAVEL",
	"Bills & Utilities":     "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"Groceries":             "FOOD_AND_DRINK_GROCERIES",
	"Education":             "GENERAL_SERVICES_EDUCATION",
	"Professional Services": "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Personal":              "PERSONAL_CARE_OTHER_PERSONAL_CARE",
	"Health & Wellness":     "MEDICAL_OTHER_MEDICAL",
	"Fees & Adjustments":    "BANK_FEES_OTHER_BANK_FEES",
	"Home":                  "HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT",
	"Gifts & Donations":     "GOVERNMENT_AND_NON_PROFIT_DONATIONS",
}

// ubsBookingTypes translates the UBS booking types whose meaning is
// unambiguous, across the three eras the adapter emits: the
// statement-PDF era's upper-case printed types, the MT940/PSN era's
// SWIFT :61: transaction codes, and the web export's mixed-case
// Description2 labels (docs/adapters/ubs.md §7). Lookups fold case,
// so a type the PDF and web eras spell differently has one entry,
// written in the PDF era's upper case; a type only one era publishes
// is spelled as that era spells it.
//
// Everything else falls through: a payment rail — a payment order, a
// direct debit, a credit, NTRF — names how the money moved, not what
// it bought, and the narrative or the model has to say the rest.
//
// Fees and interest charged translate to vendored values; the rest are
// the three cases where the booking type names the movement itself.
// Cash out of an ATM is `cash_withdrawal` — the atm rule reaches the
// same verdict from a narrative that names the machine, and this
// reaches the rows whose narrative is a bare bank tag. A conversion
// between the holder's own currency accounts is an own-account move,
// `internal_transfer`, which the matcher cannot reach because the two
// legs differ in currency. A bill paid to a card is `card_spend` by
// the card-payment policy (docs/SPENDING.md §2); the matcher still
// outranks this when the card's own leg is in gold.
//
// Of the FX spellings only the MT940 `NFEX` shape is ever placed: a
// withdrawal whose narrative is a bare tag. The adapter classifies the
// web and PDF eras' FX bookings — FOREX PURCHASE / SALE, the FX Spot,
// Forward and Swap legs — as `fx` kinds, which the enrichment
// population excludes by kind, so a row filed under one of them never
// reaches this tier. Their entries complete the vocabulary — every
// spelling of the bank's own FX filing has its meaning stated here,
// and a row that did reach the tier under one would be placed right —
// but they are not what keeps those rows out of spending; the kind is.
var ubsBookingTypes = map[string]string{
	// Fees.
	"UBS ADVICE":                        "BANK_FEES_OTHER_BANK_FEES",
	"CUSTODY PRICE":                     "BANK_FEES_OTHER_BANK_FEES",
	"RENTAL FEE SAFE BOX":               "BANK_FEES_OTHER_BANK_FEES",
	"ADR/GDR HANDLING FEES":             "BANK_FEES_OTHER_BANK_FEES",
	"THIRD-PARTY CHARGES":               "BANK_FEES_OTHER_BANK_FEES",
	"BALANCE CLOSING OF SERVICE PRICES": "BANK_FEES_OTHER_BANK_FEES",
	"NCHG":                              "BANK_FEES_OTHER_BANK_FEES", // MT940: charges
	"NCOM":                              "BANK_FEES_OTHER_BANK_FEES", // MT940: commission
	// Interest settled on the account. Only the charged side reaches
	// the spending population — an `interest` row is admitted when its
	// amount is negative — so interest earned is never filed as a fee.
	"INTEREST CALCULATION BALANCE": "BANK_FEES_INTEREST_CHARGE",
	// Cash.
	"ATM WITHDRAWAL":          canonical.SpendDetailedCashWithdrawal,
	"UBS BANCOMAT WITHDRAWAL": canonical.SpendDetailedCashWithdrawal,
	// FX between the holder's own currency accounts.
	"NFEX":                  canonical.SpendDetailedInternalTransfer, // MT940: foreign exchange
	"FOREX SALE":            canonical.SpendDetailedInternalTransfer,
	"FOREX PURCHASE":        canonical.SpendDetailedInternalTransfer,
	"Sale FX Spot":          canonical.SpendDetailedInternalTransfer,
	"Purchase FX Spot":      canonical.SpendDetailedInternalTransfer,
	"Sale FX Forward":       canonical.SpendDetailedInternalTransfer,
	"Purchase FX Forward":   canonical.SpendDetailedInternalTransfer,
	"Sale from FX Swap":     canonical.SpendDetailedInternalTransfer,
	"Purchase from FX Swap": canonical.SpendDetailedInternalTransfer,
	// Card bills.
	"PAYMENT TO CARD": canonical.SpendDetailedCardSpend,
}

// providerVocabularies is the registry, keyed by silver kind — the
// same string `silver_sources.silver_kind` holds. A source whose kind
// has no entry contributes no provider verdicts at all, which is the
// correct default: a source that publishes no categories, or whose
// vocabulary has never been reviewed, must not be read as if it had.
var providerVocabularies = map[string]providerVocabulary{
	"chase": {translations: chaseCardCategories, categorical: true},
	"ubs":   {translations: ubsBookingTypes, categorical: false},
}

// foldedProviderVocabularies is providerVocabularies re-keyed on the
// case-folded, space-trimmed value, built once at init so the per-row
// lookup is a single map hit.
var foldedProviderVocabularies = foldProviderVocabularies(providerVocabularies)

func foldProviderVocabularies(in map[string]providerVocabulary) map[string]providerVocabulary {
	out := make(map[string]providerVocabulary, len(in))
	for kind, v := range in {
		folded := make(map[string]string, len(v.translations))
		for value, detailed := range v.translations {
			folded[providerCategoryKey(value)] = detailed
		}
		out[kind] = providerVocabulary{translations: folded, categorical: v.categorical}
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
//	                      or the vocabulary is a list of booking types
//	                      and this value names a rail
//
// Separating the last two keeps the unmapped counter meaningful:
// without it every uncategorised row from every non-card source, and
// every payment order at a bank, would inflate a number that is
// supposed to mean "the issuer said something this build does not
// understand".
func ProviderCategory(silverKind, providerCategory string) (detailed string, ok, drift bool) {
	v, mapped := foldedProviderVocabularies[silverKind]
	if !mapped {
		return "", false, false
	}
	key := providerCategoryKey(providerCategory)
	if key == "" {
		// No value published for this row — not a vocabulary gap.
		return "", false, false
	}
	detailed, ok = v.translations[key]
	return detailed, ok, !ok && v.categorical
}
