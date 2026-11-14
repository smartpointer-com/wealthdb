package spending

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

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
	// What an account pays for advice, for custody or for a security it
	// holds is an investment fee (docs/SPENDING.md §2, Extensions), as
	// the rule tier files it; the bank's other charges are not.
	"UBS ADVICE":                        canonical.SpendDetailedInvestmentFees,
	"CUSTODY PRICE":                     canonical.SpendDetailedInvestmentFees,
	"ADR/GDR HANDLING FEES":             canonical.SpendDetailedInvestmentFees,
	"RENTAL FEE SAFE BOX":               "BANK_FEES_OTHER_BANK_FEES",
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
// keep the issuer's spelling rather than a corrected one.
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

	// More of the issuer's own MCC descriptions. Where a bucket is
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
	// so some of the descriptions are brand names rather than
	// lines of business. They are ordinary published vocabulary, as the
	// brand block above is, and are translated like any other value.
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
	"SALARY PAYMENT": "INCOME_SALARY",
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
