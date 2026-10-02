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
// `internal_transfer`; the matcher's amount pass cannot reach one,
// because the two legs differ in currency and it partitions on that,
// its reference pass reaches only the conversions whose two legs the
// bank stamped with one transaction number, and its stated-counter
// pass only those where one leg's narrative states the other's
// currency and figure. This entry is what places the rest — and unlike
// a pairing it places a verdict without a far account, so the
// statement draws such a row as leaving the pool.
//
// A bill paid to a card is `card_spend` by the card-payment policy
// (docs/SPENDING.md §2); the matcher still outranks this when the
// card's own leg is in gold.
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

	// Values the issuer has published since the map was last reviewed.
	// Each is the issuer's own MCC description; where its bucket is
	// coarser than the taxonomy's the translation targets that
	// primary's OTHER_* value, on the same rule as the block above.

	"Barber or beauty shops":                      "PERSONAL_CARE_HAIR_AND_BEAUTY",
	"Spas - health and beauty":                    "PERSONAL_CARE_HAIR_AND_BEAUTY",
	"Cleaning - laundry and garment services":     "PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING",
	"Freezer and locker meat provisioners":        "FOOD_AND_DRINK_GROCERIES",
	"Caterers":                                    "FOOD_AND_DRINK_RESTAURANT",
	"Package stores - beer":                       "FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR",
	"Catalog order stores / Mail order houses":    "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES",
	"Home supply warehouse stores":                "HOME_IMPROVEMENT_HARDWARE",
	"Garden and hardware center":                  "HOME_IMPROVEMENT_HARDWARE",
	"Floor coverings, Rugs":                       "HOME_IMPROVEMENT_FURNITURE",
	"Charitable and Social Service Organizations": "GOVERNMENT_AND_NON_PROFIT_DONATIONS",
	"Civic, Social, and Fraternal Associations":   "GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT",
	"Automobile dealers / Truck dealers":          "GENERAL_SERVICES_AUTOMOTIVE",
	"Automobile services":                         "GENERAL_SERVICES_AUTOMOTIVE",
	"Car component":                               "GENERAL_SERVICES_AUTOMOTIVE",
	"Courier services - air or ground":            "GENERAL_SERVICES_POSTAGE_AND_SHIPPING",
	"Postal Services":                             "GENERAL_SERVICES_POSTAGE_AND_SHIPPING",
	"Data processing services":                    "GENERAL_SERVICES_DIGITAL_SERVICES",
	"Direct marketing insurance services":         "GENERAL_SERVICES_INSURANCE",
	"Camp grounds":                                "TRAVEL_LODGING",
	"Airlines":                                    "TRAVEL_FLIGHTS",
	"Airports, Airport terminals":                 "TRAVEL_OTHER_TRAVEL",
	// Card schemes assign codes to individual airlines and hotel chains,
	// so a few of the issuer's descriptions are brand names rather than
	// lines of business. They are ordinary published vocabulary — the
	// block above has held several since it was written — and are
	// translated like any other value.
	"The Ritz Carlton Hotels":                 "TRAVEL_LODGING",
	"Westin Hotels":                           "TRAVEL_LODGING",
	"Hoteles Melia":                           "TRAVEL_LODGING",
	"Icelandair":                              "TRAVEL_FLIGHTS",
	"Cathay":                                  "TRAVEL_FLIGHTS",
	"Florists":                                "GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES",
	"Leather goods":                           "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"Furriers and fur shops":                  "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"Clock or jewelry or watch stores":        "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"Telecommunications equipment":            "GENERAL_MERCHANDISE_ELECTRONICS",
	"News Dealer & Newsstands":                "GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS",
	"Office supplies":                         "GENERAL_MERCHANDISE_OFFICE_SUPPLIES",
	"Record Stores":                           "ENTERTAINMENT_MUSIC_AND_AUDIO",
	"Bands, Orchestras & Music Entertainment": "ENTERTAINMENT_MUSIC_AND_AUDIO",
	"Doctors and Physicians":                  "MEDICAL_PRIMARY_CARE",
	"Dentists and Orthodontists":              "MEDICAL_DENTAL_CARE",
	"Hospitals":                               "MEDICAL_OTHER_MEDICAL",
	"Medical laboratories":                    "MEDICAL_OTHER_MEDICAL",
	"Orthopedic goods prosthetic devices":     "MEDICAL_OTHER_MEDICAL",
	"Drugstore products":                      "MEDICAL_PHARMACIES_AND_SUPPLEMENTS",
	// Coarser than the taxonomy: these name a trade, a channel or a
	// membership rather than a line of business, so they resolve to
	// their primary's OTHER_* value and, being catch-alls, decline the
	// row rather than claiming it (see ProviderCategoryClaims).
	"Club Membership": "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Membership Organizations - Not Elsewhere Classified": "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Business services":                     "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Estate agency":                         "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Blueprinting or photocopying services": "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Photograpgic studios":                  "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Art dealers / Art galleries":           "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Durable Goods (B2B)":                   "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Continuity / Subscription Merchant":    "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
}

// ubsCardMoneyMovement is the bank's own catch-all for a card row that
// is a money movement rather than a purchase — a mobile-payment
// transfer, a card top-up. It names no line of business, so a guess
// here would file person-to-person transfers as shopping. It is
// REVIEWED, so a miss on it is not drift, and it is left for the model
// tier, which sees the descriptor the category withholds.
var ubsCardMoneyMovement = map[string]bool{
	"Banks - merchandise and services":               true,
	"POI Funding Transactions (Excluding MoneySend)": true,
}

// ubsIncomeBookingTypes is the inflow half of the UBS booking-type
// vocabulary: the types that NAME what arrived.
//
// Left out deliberately, and they are most of the volume: the bank's
// generic credit types — a plain credit, an e-banking credit, a SEPA
// credit, an instant-payment credit, a mobile-payment credit. Each
// says a rail and nothing else, which is exactly the row the model
// tier reads a payer out of. Translating them would claim the whole
// deposit population for a value that means "money arrived".
//
// Also left out: the call-deposit and fixed-term repayment types. Those
// name the holder's own money coming back from a product that may
// itself be a tracked account, in which case the MATCHER owns the row
// and has seen both legs; where it is not tracked, the row is
// `capital_return`, and a rule can say so with knowledge this tier does
// not have.
var ubsIncomeBookingTypes = map[string]string{
	// Employment.
	"SALARY PAYMENT": "INCOME_WAGES",
	// Investment income. The `dividend` and `capital_gain` KINDS floor
	// to the same values, so these agree with the floor rather than
	// overruling it — but the bank did say it, and the provider tier
	// records what the bank said.
	"DIVIDEND":           "INCOME_DIVIDENDS",
	"COMP. DIV. PAYMENT": "INCOME_DIVIDENDS",
	"CAPITAL GAIN":       "INCOME_DISTRIBUTIONS",
	// Interest credited. The same booking type the spending map reads
	// as a finance charge: one type, two directions, which is why the
	// two maps are separate.
	"INTEREST CALCULATION BALANCE":        "INCOME_INTEREST_EARNED",
	"CALL DEPOSIT INTEREST PAYMENT":       "INCOME_INTEREST_EARNED",
	"FIXED TERM DEPOSIT INTEREST PAYMENT": "INCOME_INTEREST_EARNED",
	// Capital the holder put in, coming back out: not income.
	"REPAYMENT OF PAID-IN CAPITAL": canonical.IncomeDetailedCapitalReturn,
	"RETURN OF CAPITAL":            canonical.IncomeDetailedCapitalReturn,
}

// raiffeisenIncomeCategories is the inflow half of a CATEGORICAL
// vocabulary, so a value outside it counts as drift. The vocabulary's
// own inbound bucket is a catch-all and is translated as one: recorded,
// and left for the tier that can read a payer.
var raiffeisenIncomeCategories = map[string]string{
	"income_other": "INCOME_OTHER_INCOME",
}

// raiffeisenIncomeUncategorized is the income side's reviewed-and-left
// set. It is `raiffeisenUncategorized` LESS `income_other`, plus every
// value the categorical vocabulary spends on the outflow side.
//
// Two things it has to be, and they pull in opposite directions.
//
// `income_other` is left OUT, which is why this is not simply the other
// map. On the OUTFLOW side that token names a direction rather than a
// spend category and belongs to no spend value at all, so it is skipped
// before the lookup runs; read from the inflow side it is the
// vocabulary's own catch-all, which is a translation this family has a
// word for. It is translated in raiffeisenIncomeCategories above and
// declined by ProviderIncomeCategoryClaims — sharing one map between
// the two sides made that translation unreachable.
//
// Everything else the vocabulary publishes is IN, and that is the
// second requirement. The vocabulary is categorical, so a value outside
// the reviewed set counts as drift — "the issuer's vocabulary has
// moved", a signal the run report prints for someone to act on. But
// this map translates ONE value by design, and the bank can file an
// admitted inflow under its ordinary spend tokens: a `deposit` filed
// `real_estate_other`, or a refund or a reversal under `supermarket`
// or `bank_fee`. Reviewed on the spending
// side and unlisted here, each would report as drift on every load —
// a permanent false alarm that makes the real signal unreadable.
//
// So the two sets are derived from one another rather than written
// twice: a token added to the spending review is reviewed here too, and
// only a token NEITHER side has seen is drift.
var raiffeisenIncomeUncategorized = raiffeisenIncomeReviewed()

func raiffeisenIncomeReviewed() map[string]bool {
	out := map[string]bool{}
	for k := range raiffeisenUncategorized {
		if k == "income_other" {
			continue // translated on this side; see above
		}
		out[k] = true
	}
	for k := range raiffeisenCategories {
		out[k] = true
	}
	return out
}

// raiffeisenCategories translates the Mein ELBA vocabulary, which the
// bank publishes as lower-case tokens rather than MCC prose.
//
// The bank's "*_other" tokens are not all coarse in the same way, and
// the token's spelling does not decide whether the row is claimed —
// the TRANSLATION does. `electronics_shop_other` and `insurance_other`
// name a real line of business and resolve to one, so they claim;
// `shopping_other` and `utility` resolve to their primary's catch-all
// and so decline, leaving the merchant name to the model. What the
// vocabulary declines outright is in raiffeisenUncategorized below.
var raiffeisenCategories = map[string]string{
	"supermarket":                   "FOOD_AND_DRINK_GROCERIES",
	"tv_phone_internet":             "RENT_AND_UTILITIES_INTERNET_AND_CABLE",
	"utility":                       "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"bank_fee":                      "BANK_FEES_OTHER_BANK_FEES",
	"insurance_other":               "GENERAL_SERVICES_INSURANCE",
	"government_service":            "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES",
	"airline":                       "TRAVEL_FLIGHTS",
	"electronics_shop_other":        "GENERAL_MERCHANDISE_ELECTRONICS",
	"tobacco_smoking_related_store": "GENERAL_MERCHANDISE_TOBACCO_AND_VAPE",
	"shopping_other":                "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	// A movement, not a line of business: the bank names the rail.
	"atm_withdrawal": canonical.SpendDetailedCashWithdrawal,
}

// raiffeisenUncategorized are the values Mein ELBA publishes when it
// has placed nothing: its own "not categorised" token, its bucket for
// a payment it could not read, and its income token — which names a
// direction rather than a category and belongs to no spend value at
// all. `real_estate_other` joins them because it can cover both a
// rent payment and a property purchase, and the two are a utility bill
// and a capital movement.
var raiffeisenUncategorized = map[string]bool{
	"not_categorized":   true,
	"payment_other":     true,
	"income_other":      true,
	"real_estate_other": true,
}

// syntheticCategories and syntheticIncomeCategories translate the synthetic
// kind's vocabulary, which is the taxonomy itself: its provider_category is a
// spend_detailed value on a row the spending tier reads (a refund included,
// though it is an inflow) and an income_detailed value on a row the income
// tier reads, stamped outright. Each map is the identity over its family,
// built from canonical.SpendCategories rather than written out, so the
// vocabulary is exactly the table the migrations seed and cannot drift from
// it.
//
// The vocabulary is NOT categorical, for two reasons. A value here is the
// provider's verdict, not an issuer's coarse bucket: a catch-all it states —
// BANK_FEES_OTHER_BANK_FEES — is the answer, as a booking type's catch-all
// is, and claims the row rather than deferring it to a model tier that could
// know no more (ProviderCategoryClaims). And a value outside the taxonomy is
// not an issuer's vocabulary moving, so it is not drift worth counting; it
// falls through untranslated, as a rail does.
var (
	syntheticCategories       = taxonomyIdentity(canonical.FamilySpending)
	syntheticIncomeCategories = taxonomyIdentity(canonical.FamilyIncome)
)

// plaidPFCv2 is every detailed value of Plaid's personal finance category
// taxonomy, version 2, verbatim and in Plaid's order. The collector asks
// Plaid for version 2 on every read, so this is the whole vocabulary a
// plaid row can carry. It is the reviewed list. Each side translates a
// value or leaves it untranslated. Only a value outside the list is drift,
// which is what a revision of the taxonomy looks like.
var plaidPFCv2 = []string{
	"INCOME_CHILD_SUPPORT",
	"INCOME_CONTRACTOR",
	"INCOME_DIVIDENDS",
	"INCOME_GIG_ECONOMY",
	"INCOME_INTEREST_EARNED",
	"INCOME_LONG_TERM_DISABILITY",
	"INCOME_MILITARY",
	"INCOME_RENTAL",
	"INCOME_RETIREMENT_PENSION",
	"INCOME_SALARY",
	"INCOME_TAX_REFUND",
	"INCOME_UNEMPLOYMENT",
	"INCOME_OTHER",

	"LOAN_DISBURSEMENTS_AUTO",
	"LOAN_DISBURSEMENTS_CASH_ADVANCES",
	"LOAN_DISBURSEMENTS_EWA",
	"LOAN_DISBURSEMENTS_MORTGAGE",
	"LOAN_DISBURSEMENTS_PERSONAL",
	"LOAN_DISBURSEMENTS_STUDENT",
	"LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT",

	"LOAN_PAYMENTS_BNPL",
	"LOAN_PAYMENTS_CAR_PAYMENT",
	"LOAN_PAYMENTS_CASH_ADVANCES",
	"LOAN_PAYMENTS_CREDIT_CARD_PAYMENT",
	"LOAN_PAYMENTS_EWA",
	"LOAN_PAYMENTS_MORTGAGE_PAYMENT",
	"LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT",
	"LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT",
	"LOAN_PAYMENTS_OTHER_PAYMENT",

	"TRANSFER_IN_ACCOUNT_TRANSFER",
	"TRANSFER_IN_DEPOSIT",
	"TRANSFER_IN_INVESTMENT_AND_RETIREMENT_FUNDS",
	"TRANSFER_IN_SAVINGS",
	"TRANSFER_IN_TRANSFER_IN_FROM_APPS",
	"TRANSFER_IN_WIRE",
	"TRANSFER_IN_OTHER_TRANSFER_IN",

	"TRANSFER_OUT_ACCOUNT_TRANSFER",
	"TRANSFER_OUT_CRYPTO",
	"TRANSFER_OUT_INVESTMENT_AND_RETIREMENT_FUNDS",
	"TRANSFER_OUT_SAVINGS",
	"TRANSFER_OUT_TRANSFER_OUT_FROM_APPS",
	"TRANSFER_OUT_WIRE",
	"TRANSFER_OUT_WITHDRAWAL",
	"TRANSFER_OUT_OTHER_TRANSFER_OUT",

	"BANK_FEES_ATM_FEES",
	"BANK_FEES_INSUFFICIENT_FUNDS",
	"BANK_FEES_INTEREST_CHARGE",
	"BANK_FEES_FOREIGN_TRANSACTION_FEES",
	"BANK_FEES_OVERDRAFT_FEES",
	"BANK_FEES_LATE_FEES",
	"BANK_FEES_CASH_ADVANCE",
	"BANK_FEES_OTHER_BANK_FEES",

	"ENTERTAINMENT_CASINOS_AND_GAMBLING",
	"ENTERTAINMENT_MUSIC_AND_AUDIO",
	"ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS",
	"ENTERTAINMENT_TV_AND_MOVIES",
	"ENTERTAINMENT_VIDEO_GAMES",
	"ENTERTAINMENT_OTHER_ENTERTAINMENT",

	"FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR",
	"FOOD_AND_DRINK_COFFEE",
	"FOOD_AND_DRINK_FAST_FOOD",
	"FOOD_AND_DRINK_GROCERIES",
	"FOOD_AND_DRINK_RESTAURANT",
	"FOOD_AND_DRINK_VENDING_MACHINES",
	"FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK",

	"GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS",
	"GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES",
	"GENERAL_MERCHANDISE_CONVENIENCE_STORES",
	"GENERAL_MERCHANDISE_DEPARTMENT_STORES",
	"GENERAL_MERCHANDISE_DISCOUNT_STORES",
	"GENERAL_MERCHANDISE_ELECTRONICS",
	"GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES",
	"GENERAL_MERCHANDISE_OFFICE_SUPPLIES",
	"GENERAL_MERCHANDISE_ONLINE_MARKETPLACES",
	"GENERAL_MERCHANDISE_PET_SUPPLIES",
	"GENERAL_MERCHANDISE_SPORTING_GOODS",
	"GENERAL_MERCHANDISE_SUPERSTORES",
	"GENERAL_MERCHANDISE_TOBACCO_AND_VAPE",
	"GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",

	"HOME_IMPROVEMENT_FURNITURE",
	"HOME_IMPROVEMENT_HARDWARE",
	"HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE",
	"HOME_IMPROVEMENT_SECURITY",
	"HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT",

	"MEDICAL_DENTAL_CARE",
	"MEDICAL_EYE_CARE",
	"MEDICAL_NURSING_CARE",
	"MEDICAL_PHARMACIES_AND_SUPPLEMENTS",
	"MEDICAL_PRIMARY_CARE",
	"MEDICAL_VETERINARY_SERVICES",
	"MEDICAL_OTHER_MEDICAL",

	"PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS",
	"PERSONAL_CARE_HAIR_AND_BEAUTY",
	"PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING",
	"PERSONAL_CARE_OTHER_PERSONAL_CARE",

	"GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING",
	"GENERAL_SERVICES_AUTOMOTIVE",
	"GENERAL_SERVICES_CHILDCARE",
	"GENERAL_SERVICES_CONSULTING_AND_LEGAL",
	"GENERAL_SERVICES_EDUCATION",
	"GENERAL_SERVICES_INSURANCE",
	"GENERAL_SERVICES_POSTAGE_AND_SHIPPING",
	"GENERAL_SERVICES_STORAGE",
	"GENERAL_SERVICES_OTHER_GENERAL_SERVICES",

	"GOVERNMENT_AND_NON_PROFIT_DONATIONS",
	"GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES",
	"GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT",
	"GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT",

	"TRANSPORTATION_BIKES_AND_SCOOTERS",
	"TRANSPORTATION_GAS",
	"TRANSPORTATION_PARKING",
	"TRANSPORTATION_PUBLIC_TRANSIT",
	"TRANSPORTATION_TAXIS_AND_RIDE_SHARES",
	"TRANSPORTATION_TOLLS",
	"TRANSPORTATION_OTHER_TRANSPORTATION",

	"TRAVEL_FLIGHTS",
	"TRAVEL_LODGING",
	"TRAVEL_RENTAL_CARS",
	"TRAVEL_OTHER_TRAVEL",

	"RENT_AND_UTILITIES_GAS_AND_ELECTRICITY",
	"RENT_AND_UTILITIES_INTERNET_AND_CABLE",
	"RENT_AND_UTILITIES_RENT",
	"RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT",
	"RENT_AND_UTILITIES_TELEPHONE",
	"RENT_AND_UTILITIES_WATER",
	"RENT_AND_UTILITIES_OTHER_UTILITIES",

	"OTHER_OTHER",
}

// plaidCategories is the spending side. Every value that is a wealthdb
// spend value already translates to itself: the taxonomy vendors Plaid's
// version 1, and version 2 kept those spellings. The rest is below.
//
// Two bank fees are new in version 2, and the taxonomy has nothing finer
// than the catch-all for them. A card bill names the movement, so it is
// `card_spend` (the matcher outranks it when the card's own leg is in
// gold). A mortgage instalment is `mortgage_transfer`. A student,
// personal, cash-advance or car payment is `debt_repayment`. Each is the
// delta for its lender. The other loan payments stay untranslated
// (plaidUncategorized). A car payment can be a lease, which is
// consumption, and `debt_repayment` takes it out of the base. Plaid files
// loans and leases under one value. A config rule on the lessor's name,
// scoped to the paying account, puts a lease back.
var plaidCategories = plaidTranslations(canonical.ValidSpendDetailed, map[string]string{
	"BANK_FEES_LATE_FEES":                 "BANK_FEES_OTHER_BANK_FEES",
	"BANK_FEES_CASH_ADVANCE":              "BANK_FEES_OTHER_BANK_FEES",
	"LOAN_PAYMENTS_CREDIT_CARD_PAYMENT":   canonical.SpendDetailedCardSpend,
	"LOAN_PAYMENTS_MORTGAGE_PAYMENT":      canonical.DetailedMortgageTransfer,
	"LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT":  canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT": canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_CASH_ADVANCES":         canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_CAR_PAYMENT":           canonical.SpendDetailedDebtRepayment,
})

// plaidIncomeCategories is the income side. It translates:
//
//   - each vendored income value to itself;
//   - Plaid's version 2 names to the taxonomy's;
//   - money borrowed arriving to `loan_proceeds`, and a mortgage tranche to
//     `mortgage_transfer`.
//
// Gig pay is wages, a contractor is self-employed, and military and
// long-term disability benefits are state transfers. INCOME_OTHER is the
// catch-all, recorded and declined.
//
// Version 2 widened INCOME_RETIREMENT_PENSION to payouts from plans such as
// a 401(k). Such a payout from a plan gold does not track is still placed
// as pension income; a config rule on the plan's name places it as
// `retirement_transfer`.
var plaidIncomeCategories = plaidTranslations(canonical.ValidIncomeDetailed, map[string]string{
	"INCOME_SALARY":                         "INCOME_WAGES",
	"INCOME_GIG_ECONOMY":                    "INCOME_WAGES",
	"INCOME_CONTRACTOR":                     canonical.IncomeDetailedSelfEmployment,
	"INCOME_CHILD_SUPPORT":                  canonical.IncomeDetailedAlimonyAndChildSupport,
	"INCOME_RENTAL":                         canonical.IncomeDetailedRent,
	"INCOME_MILITARY":                       canonical.IncomeDetailedGovernmentBenefits,
	"INCOME_LONG_TERM_DISABILITY":           canonical.IncomeDetailedGovernmentBenefits,
	"INCOME_OTHER":                          "INCOME_OTHER_INCOME",
	"LOAN_DISBURSEMENTS_AUTO":               canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_CASH_ADVANCES":      canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_PERSONAL":           canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_STUDENT":            canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT": canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_MORTGAGE":           canonical.DetailedMortgageTransfer,
})

// plaidUncategorized and plaidIncomeUncategorized are each side's reviewed
// and untranslated values: the rest of plaidPFCv2. A transfer's category
// does not say whose account the far side is, which is the matcher's to
// find and a rule's to state. A buy-now-pay-later instalment is the only
// trace of a purchase, and OTHER_PAYMENT can be a card bill or a loan
// payment. An early wage advance (LOAN_DISBURSEMENTS_EWA) is usually wages
// paid early and taken back from the next pay, and sometimes a loan. It
// stays in the income base as a visible receipt, and its repayment
// (LOAN_PAYMENTS_EWA) in the spending base, so the two legs offset. The
// other direction's values arrive on refunds and reversals. OTHER_OTHER
// stays here, never `other`, which is a verdict and would claim the row.
var (
	plaidUncategorized       = plaidRest(plaidCategories)
	plaidIncomeUncategorized = plaidRest(plaidIncomeCategories)
)

// plaidTranslations is every plaidPFCv2 value that `valid` admits, to
// itself, with `overrides` on top.
func plaidTranslations(valid func(string) bool, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for _, v := range plaidPFCv2 {
		if valid(v) {
			out[v] = v
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// plaidRest is every plaidPFCv2 value `translated` leaves out.
func plaidRest(translated map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, v := range plaidPFCv2 {
		if _, ok := translated[v]; !ok {
			out[v] = true
		}
	}
	return out
}

// taxonomyIdentity maps every detailed value of one family to itself.
func taxonomyIdentity(family canonical.Family) map[string]string {
	out := map[string]string{}
	for _, c := range canonical.SpendCategories {
		if c.Family.InFamily(family) {
			out[c.Detailed] = c.Detailed
		}
	}
	return out
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
	key := providerCategoryKey(providerCategory)
	if key == "" {
		// No value published for this row — not a vocabulary gap.
		return "", false, false
	}
	if v.untranslatable[key] {
		// A value this vocabulary reviewed and left alone: not drift,
		// and left for the model tier.
		return "", false, false
	}
	detailed, ok = v.translations[key]
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
	key := providerCategoryKey(providerCategory)
	if key == "" {
		return "", false, false
	}
	if v.incomeUntranslatable[key] {
		return "", false, false
	}
	detailed, ok = v.income[key]
	return detailed, ok, !ok && v.categorical
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
//     Its catch-alls are not shrugs: "CUSTODY PRICE" resolving to
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
