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
	// untranslatable are the provider's own "I could not place this"
	// values — an issuer's literal "Other" bucket. They are REVIEWED,
	// so a miss on one is not drift; and they are deliberately not
	// translated, because a row the provider could not place is
	// exactly the row the model tier can, and any verdict here would
	// pre-empt it. Without this a categorical vocabulary has only two
	// ways to treat such a value, and both are wrong: translate it and
	// lose the row to a bucket, or omit it and inflate a counter that
	// is supposed to mean "the issuer said something this build does
	// not understand".
	untranslatable map[string]bool
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

// amexCardCategories translates the American Express card vocabulary —
// the categories Amex files a modern-era card row under when it files one
// at all, which the collector resolves from the code → label map the
// activity payload ships beside the rows, so gold receives the label
// rather than a code. The deep era, read from statement PDFs, carries no
// category and falls to the model tier.
//
// Same rule as chase's table: where the issuer's bucket is coarser
// than the taxonomy's, the translation targets that primary's OTHER_*
// value rather than its most common member, and the model tier can
// refine a merchant later from its name. Amex's buckets are coarse
// almost throughout — nine categories for everything a card can buy —
// so most entries land on an OTHER_*.
//
// "Communications" is the one that reads oddly: Amex files phone,
// internet and cable under it, which the taxonomy splits across
// RENT_AND_UTILITIES. OTHER_UTILITIES is the honest parent of that
// split, and picking TELEPHONE would assert a device the category
// never named.
var amexCardCategories = map[string]string{
	"Business Services":      "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Communications":         "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"Entertainment":          "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Fees & Adjustments":     "BANK_FEES_OTHER_BANK_FEES",
	"Merchandise & Supplies": "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Restaurants":            "FOOD_AND_DRINK_RESTAURANT",
	"Transportation":         "TRANSPORTATION_OTHER_TRANSPORTATION",
	"Travel":                 "TRAVEL_OTHER_TRAVEL",
}

// amexUncategorized is Amex's own residual bucket. It is reviewed and
// deliberately left untranslated: it says the ISSUER could not place
// the row, which is the case the model tier exists for.
var amexUncategorized = map[string]bool{"other": true}

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

// ubsCardCategories translates the merchant categories a UBS card
// ledger carries. They are ISO 18245 MCC descriptions in UBS's own
// spelling — including its typos and its habit of naming an airline or
// hotel chain where the standard names a line of business — so entries
// are written exactly as observed rather than corrected.
//
// The vocabulary is CATEGORICAL: every card row is filed under one, so a
// value the map lacks is one this build has not reviewed, and is counted
// as drift. That is the opposite reading from the same source's booking
// types (ubsBookingTypes), which are payment rails where a miss is the
// normal case — which is why the two are separate vocabularies keyed by
// the product rather than one map keyed by the source.
//
// Where UBS's bucket is coarser than the taxonomy's, the translation
// targets that primary's OTHER_* value rather than its most common
// member; where it is precise, the specific value is used. A brand name
// is translated by what the brand sells.
var ubsCardCategories = map[string]string{
	// Food and drink.
	"Restaurants":           "FOOD_AND_DRINK_RESTAURANT",
	"Fast-Food Restaurants": "FOOD_AND_DRINK_FAST_FOOD",
	"Fast Food Restaurant":  "FOOD_AND_DRINK_FAST_FOOD",
	"Grocery stores":        "FOOD_AND_DRINK_GROCERIES",
	"Bakeries":              "FOOD_AND_DRINK_GROCERIES",
	"Dairy products stores": "FOOD_AND_DRINK_GROCERIES",
	"Candy and nut stores":  "FOOD_AND_DRINK_GROCERIES",
	// Transport.
	"Commuter transportation":   "TRANSPORTATION_PUBLIC_TRANSIT",
	"Passenger railways":        "TRANSPORTATION_PUBLIC_TRANSIT",
	"Bus lines, Tour buses":     "TRANSPORTATION_PUBLIC_TRANSIT",
	"Taxicabs":                  "TRANSPORTATION_TAXIS_AND_RIDE_SHARES",
	"Parking & Garages":         "TRANSPORTATION_PARKING",
	"Gasoline service stations": "TRANSPORTATION_GAS",
	"Toll and bridge fees":      "TRANSPORTATION_TOLLS",
	"Delivery services - local": "TRANSPORTATION_OTHER_TRANSPORTATION",
	// Travel.
	"Hotels":          "TRAVEL_LODGING",
	"Aparments":       "TRAVEL_LODGING",
	"Travel agencies": "TRAVEL_OTHER_TRAVEL",
	"Rent-a-car":      "TRAVEL_RENTAL_CARS",
	"Cruise lines":    "TRAVEL_OTHER_TRAVEL",
	"Duty free shop":  "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	// ISO 18245 assigns brand-specific codes to airlines (3000-3350)
	// and hotel chains (3501-3999), so a card ledger's category can be
	// a company name where the rest of the vocabulary is a line of
	// business. The two families translate wholesale — an airline code
	// is a flight and a hotel code is lodging, whichever carrier or
	// chain it names — so the entries below cover the common members
	// rather than the ones any one ledger happens to contain. An entry
	// that never appears costs nothing; a family member that is missing
	// reads as drift, which is the signal to add it.
	"Air Canada":                    "TRAVEL_FLIGHTS",
	"Air France":                    "TRAVEL_FLIGHTS",
	"Alitalia":                      "TRAVEL_FLIGHTS",
	"American Airlines":             "TRAVEL_FLIGHTS",
	"Austrian":                      "TRAVEL_FLIGHTS",
	"British Airways":               "TRAVEL_FLIGHTS",
	"Delta":                         "TRAVEL_FLIGHTS",
	"easyJet":                       "TRAVEL_FLIGHTS",
	"Emirates":                      "TRAVEL_FLIGHTS",
	"Iberia":                        "TRAVEL_FLIGHTS",
	"KLM":                           "TRAVEL_FLIGHTS",
	"Lufthansa":                     "TRAVEL_FLIGHTS",
	"Qantas":                        "TRAVEL_FLIGHTS",
	"Ryanair":                       "TRAVEL_FLIGHTS",
	"SAS":                           "TRAVEL_FLIGHTS",
	"Singapore Airlines":            "TRAVEL_FLIGHTS",
	"Swiss International Air Lines": "TRAVEL_FLIGHTS",
	"Turkish Airlines":              "TRAVEL_FLIGHTS",
	"United Airlines":               "TRAVEL_FLIGHTS",
	"Accor":                         "TRAVEL_LODGING",
	"Best Western":                  "TRAVEL_LODGING",
	"Hilton":                        "TRAVEL_LODGING",
	"Holiday Inn":                   "TRAVEL_LODGING",
	"Hyatt":                         "TRAVEL_LODGING",
	"Ibis":                          "TRAVEL_LODGING",
	"Marriott":                      "TRAVEL_LODGING",
	"Novotel":                       "TRAVEL_LODGING",
	"Radisson":                      "TRAVEL_LODGING",
	"Sheraton":                      "TRAVEL_LODGING",
	// Retail.
	"Department stores":                        "GENERAL_MERCHANDISE_DEPARTMENT_STORES",
	"Retail business":                          "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Clothing store":                           "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"Clothing - sports":                        "GENERAL_MERCHANDISE_SPORTING_GOODS",
	"Shoe stores":                              "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"Book stores":                              "GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS",
	"Books & newspapers (B2B)":                 "GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS",
	"Games and hobby stores":                   "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Music stores":                             "ENTERTAINMENT_MUSIC_AND_AUDIO",
	"Electronics Stores":                       "GENERAL_MERCHANDISE_ELECTRONICS",
	"Computer":                                 "GENERAL_MERCHANDISE_ELECTRONICS",
	"Camera and photographic supply stores":    "GENERAL_MERCHANDISE_ELECTRONICS",
	"Household appliance stores":               "HOME_IMPROVEMENT_FURNITURE",
	"Furniture":                                "HOME_IMPROVEMENT_FURNITURE",
	"Hardware stores":                          "HOME_IMPROVEMENT_HARDWARE",
	"Office supply stores":                     "GENERAL_MERCHANDISE_OFFICE_SUPPLIES",
	"Card or gift or novelty or souvenir shop": "GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES",
	"Antique shops":                            "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Numismatic or philatelic supplies":        "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Bicycle shops - sales and service":        "GENERAL_MERCHANDISE_SPORTING_GOODS",
	"Cosmetic stores":                          "PERSONAL_CARE_OTHER_PERSONAL_CARE",
	"Other direct Marketers":                   "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES",
	"Non-durable Goods (B2B)":                  "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	// Health.
	"Pharmacies": "MEDICAL_PHARMACIES_AND_SUPPLEMENTS",
	"Optician":   "MEDICAL_EYE_CARE",
	// Entertainment and recreation.
	"Amusement park":                        "ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS",
	"Tourist Attractions and Exhibits":      "ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS",
	"Recreation Services":                   "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Cinema":                                "ENTERTAINMENT_TV_AND_MOVIES",
	"Theather Production / Ticket Agencies": "ENTERTAINMENT_TV_AND_MOVIES",
	"Swimming pool":                         "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	// Services and utilities.
	"Computer network/Information services":            "RENT_AND_UTILITIES_INTERNET_AND_CABLE",
	"Telecomminication service":                        "RENT_AND_UTILITIES_TELEPHONE",
	"Telegraph services":                               "RENT_AND_UTILITIES_TELEPHONE",
	"Digital goods":                                    "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Computer software stores":                         "GENERAL_MERCHANDISE_ELECTRONICS",
	"Schools and Educational Services":                 "GENERAL_SERVICES_EDUCATION",
	"Government Services":                              "GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT",
	"Advertising services":                             "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Misc. publishing and printing services":           "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Equipment rental and leasing services":            "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Professional Services - Not Elsewhere Classified": "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	// The bank's own catch-all for a card row that is a money movement
	// rather than a purchase — a mobile-payment transfer, a card
	// top-up. It names no line of business, so it is deliberately
	// LEFT UNMAPPED: a guess here would file person-to-person
	// transfers as shopping. Listed so the omission reads as a
	// decision rather than an oversight.
	//   "Banks - merchandise and services"
}

// providerVocabularies is the registry. A source with no entry
// contributes no provider verdicts at all, which is the correct
// default: a source that publishes no categories, or whose vocabulary
// has never been reviewed, must not be read as if it had.
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
	"ubs":      {translations: ubsBookingTypes, categorical: false},
	"ubs/card": {translations: ubsCardCategories, categorical: true},
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
		var skip map[string]bool
		if len(v.untranslatable) > 0 {
			skip = make(map[string]bool, len(v.untranslatable))
			for value := range v.untranslatable {
				skip[providerCategoryKey(value)] = true
			}
		}
		out[kind] = providerVocabulary{translations: folded,
			untranslatable: skip, categorical: v.categorical}
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
//	                      the value is the issuer's own reviewed
//	                      residual bucket, or the vocabulary is a list
//	                      of booking types and this value names a rail
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
	key := providerCategoryKey(providerCategory)
	if key == "" {
		// No value published for this row — not a vocabulary gap.
		return "", false, false
	}
	if v.untranslatable[key] {
		// The issuer's own residual bucket: reviewed, so not drift, and
		// left for the model tier.
		return "", false, false
	}
	detailed, ok = v.translations[key]
	return detailed, ok, !ok && v.categorical
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
