package canonical

import "strings"

// Spend-category taxonomy: the two-level vocabulary behind gold's
// `spend_categories` dimension and the `spend_detailed` column of the
// spending overlay tables. The primary is the coarse bucket a report
// groups by; the detailed value is what the enrichment pass assigns
// to a transaction or to a merchant.
//
// PROVENANCE. The vendored pairs are Plaid's Personal Finance
// Category taxonomy (transactions-personal-finance-category-taxonomy.csv,
// retrieved 2026-09-04: 16 primaries, 104 detailed pairs). They are
// vendored rather than fetched so a taxonomy revision arrives as a
// reviewable diff instead of silently re-labelling history. Values and
// descriptions are copied verbatim — only trailing whitespace is
// trimmed — so a refreshed CSV diffs cleanly against this table.
//
// Only the spend side is kept: 12 primaries and 80 detailed values.
// The four dropped primaries describe flows rather than spending —
// INCOME, TRANSFER_IN, TRANSFER_OUT and LOAN_PAYMENTS. Income and the
// two transfer families belong to a future cashflow feature; spending
// covers outflows only.
//
// LOAN_PAYMENTS is the load-bearing drop: a mortgage payment is an
// own-account move to a tracked AccountKindMortgage (docs/SPENDING.md
// §2).
// TODO(cashflow): interest-versus-principal split — docs/SPENDING.md §10.
//
// Six delta values are ours rather than Plaid's and are
// primary-level (primary == detailed, so they group as their own
// bucket): `internal_transfer`, `cash_withdrawal`, `card_spend`,
// `gift`, `investment` and `other`. They keep the repo's lowercase enum
// idiom, which also marks them at a glance as not-from-Plaid.
//
// EXTENSIONS are the third class, and they differ from the deltas in
// the one way that matters: a model MAY emit them. A delta is decided
// from structure a merchant name cannot reveal — whose account the
// money went to, whether a card is itemised — so the gauntlet refuses
// one. An extension is the opposite: an ordinary merchant judgement
// for which the vendored vocabulary simply has no word yet. It is
// therefore shaped like a vendored row, `<PRIMARY>_<DETAIL>` under an
// existing primary, so that when the taxonomy does catch up the
// refreshed CSV supersedes ours as a clean diff rather than sitting
// beside it.
//
// The policy each delta encodes — what is and is not spending, which
// tier places it — is docs/SPENDING.md §2; it is not restated here.

// SpendCategory is one (primary, detailed, description) row of the
// taxonomy. Gold's `spend_categories` dimension is seeded from this
// table — migration 0040 seeded the vendored rows and the first three
// deltas, 0045/0046/0047 one delta each — and a generator-style test
// pins the migrated dimension to the table so they cannot drift. A new
// value is a row here plus a new migration; an applied migration is
// never edited.
type SpendCategory struct {
	Primary     string
	Detailed    string
	Description string
}

// The delta values. A rule, a pin, the internal-transfer matcher or
// the provider tier — where the provider's own booking type names the
// movement — assigns these; the model tier never emits them.
const (
	SpendDetailedInternalTransfer = "internal_transfer"
	SpendDetailedCashWithdrawal   = "cash_withdrawal"
	SpendDetailedCardSpend        = "card_spend"
	SpendDetailedGift             = "gift"
	SpendDetailedInvestment       = "investment"
	SpendDetailedOther            = "other"
)

// The extension values: ours, but shaped like the vendored rows and
// emittable by the model tier. See extensionSpendCategories.
const (
	SpendDetailedDigitalServices = "GENERAL_SERVICES_DIGITAL_SERVICES"
)

// vendoredSpendCategories is the Plaid subset, in the source CSV's
// order and grouped by primary as it groups them.
var vendoredSpendCategories = []SpendCategory{
	{"BANK_FEES", "BANK_FEES_ATM_FEES", "Fees incurred for out-of-network ATMs"},
	{"BANK_FEES", "BANK_FEES_FOREIGN_TRANSACTION_FEES", "Fees incurred on non-domestic transactions"},
	{"BANK_FEES", "BANK_FEES_INSUFFICIENT_FUNDS", "Fees relating to insufficient funds"},
	{"BANK_FEES", "BANK_FEES_INTEREST_CHARGE", "Fees incurred for interest on purchases, including not-paid-in-full or interest on cash advances"},
	{"BANK_FEES", "BANK_FEES_OVERDRAFT_FEES", "Fees incurred when an account is in overdraft"},
	{"BANK_FEES", "BANK_FEES_OTHER_BANK_FEES", "Other miscellaneous bank fees"},

	{"ENTERTAINMENT", "ENTERTAINMENT_CASINOS_AND_GAMBLING", "Gambling, casinos, and sports betting"},
	{"ENTERTAINMENT", "ENTERTAINMENT_MUSIC_AND_AUDIO", "Digital and in-person music purchases, including music streaming services"},
	{"ENTERTAINMENT", "ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS", "Purchases made at sporting events, music venues, concerts, museums, and amusement parks"},
	{"ENTERTAINMENT", "ENTERTAINMENT_TV_AND_MOVIES", "In home movie streaming services and movie theaters"},
	{"ENTERTAINMENT", "ENTERTAINMENT_VIDEO_GAMES", "Digital and in-person video game purchases"},
	{"ENTERTAINMENT", "ENTERTAINMENT_OTHER_ENTERTAINMENT", "Other miscellaneous entertainment purchases, including night life and adult entertainment"},

	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR", "Beer, Wine & Liquor Stores"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE", "Purchases at coffee shops or cafes"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_FAST_FOOD", "Dining expenses for fast food chains"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", "Purchases for fresh produce and groceries, including farmers' markets"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", "Dining expenses for restaurants, bars, gastropubs, and diners"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_VENDING_MACHINES", "Purchases made at vending machine operators"},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK", "Other miscellaneous food and drink, including desserts, juice bars, and delis"},

	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS", "Books, magazines, and news"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES", "Apparel, shoes, and jewelry"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_CONVENIENCE_STORES", "Purchases at convenience stores"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_DEPARTMENT_STORES", "Retail stores with wide ranges of consumer goods, typically specializing in clothing and home goods"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_DISCOUNT_STORES", "Stores selling goods at a discounted price"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ELECTRONICS", "Electronics stores and websites"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES", "Photo, gifts, cards, and floral stores"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OFFICE_SUPPLIES", "Stores that specialize in office goods"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES", "Multi-purpose e-commerce platforms such as Etsy, Ebay and Amazon"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_PET_SUPPLIES", "Pet supplies and pet food"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SPORTING_GOODS", "Sporting goods, camping gear, and outdoor equipment"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SUPERSTORES", "Superstores such as Target and Walmart, selling both groceries and general merchandise"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_TOBACCO_AND_VAPE", "Purchases for tobacco and vaping products"},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "Other miscellaneous merchandise, including toys, hobbies, and arts and crafts"},

	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_FURNITURE", "Furniture, bedding, and home accessories"},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_HARDWARE", "Building materials, hardware stores, paint, and wallpaper"},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE", "Plumbing, lighting, gardening, and roofing"},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_SECURITY", "Home security system purchases"},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT", "Other miscellaneous home purchases, including pool installation and pest control"},

	{"MEDICAL", "MEDICAL_DENTAL_CARE", "Dentists and general dental care"},
	{"MEDICAL", "MEDICAL_EYE_CARE", "Optometrists, contacts, and glasses stores"},
	{"MEDICAL", "MEDICAL_NURSING_CARE", "Nursing care and facilities"},
	{"MEDICAL", "MEDICAL_PHARMACIES_AND_SUPPLEMENTS", "Pharmacies and nutrition shops"},
	{"MEDICAL", "MEDICAL_PRIMARY_CARE", "Doctors and physicians"},
	{"MEDICAL", "MEDICAL_VETERINARY_SERVICES", "Prevention and care procedures for animals"},
	{"MEDICAL", "MEDICAL_OTHER_MEDICAL", "Other miscellaneous medical, including blood work, hospitals, and ambulances"},

	{"PERSONAL_CARE", "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS", "Gyms, fitness centers, and workout classes"},
	{"PERSONAL_CARE", "PERSONAL_CARE_HAIR_AND_BEAUTY", "Manicures, haircuts, waxing, spa/massages, and bath and beauty products"},
	{"PERSONAL_CARE", "PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING", "Wash and fold, and dry cleaning expenses"},
	{"PERSONAL_CARE", "PERSONAL_CARE_OTHER_PERSONAL_CARE", "Other miscellaneous personal care, including mental health apps and services"},

	{"GENERAL_SERVICES", "GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING", "Financial planning, and tax and accounting services"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_AUTOMOTIVE", "Oil changes, car washes, repairs, and towing"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CHILDCARE", "Babysitters and daycare"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CONSULTING_AND_LEGAL", "Consulting and legal services"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_EDUCATION", "Elementary, high school, professional schools, and college tuition"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_INSURANCE", "Insurance for auto, home, and healthcare"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_POSTAGE_AND_SHIPPING", "Mail, packaging, and shipping services"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_STORAGE", "Storage services and facilities"},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "Other miscellaneous services, including advertising and cloud storage"},

	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_DONATIONS", "Charitable, political, and religious donations"},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES", "Government departments and agencies, such as driving licences, and passport renewal"},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT", "Tax payments, including income and property taxes"},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT", "Other miscellaneous government and non-profit agencies"},

	{"TRANSPORTATION", "TRANSPORTATION_BIKES_AND_SCOOTERS", "Bike and scooter rentals"},
	{"TRANSPORTATION", "TRANSPORTATION_GAS", "Purchases at a gas station"},
	{"TRANSPORTATION", "TRANSPORTATION_PARKING", "Parking fees and expenses"},
	{"TRANSPORTATION", "TRANSPORTATION_PUBLIC_TRANSIT", "Public transportation, including rail and train, buses, and metro"},
	{"TRANSPORTATION", "TRANSPORTATION_TAXIS_AND_RIDE_SHARES", "Taxi and ride share services"},
	{"TRANSPORTATION", "TRANSPORTATION_TOLLS", "Toll expenses"},
	{"TRANSPORTATION", "TRANSPORTATION_OTHER_TRANSPORTATION", "Other miscellaneous transportation expenses"},

	{"TRAVEL", "TRAVEL_FLIGHTS", "Airline expenses"},
	{"TRAVEL", "TRAVEL_LODGING", "Hotels, motels, and hosted accommodation such as Airbnb"},
	{"TRAVEL", "TRAVEL_RENTAL_CARS", "Rental cars, charter buses, and trucks"},
	{"TRAVEL", "TRAVEL_OTHER_TRAVEL", "Other miscellaneous travel expenses"},

	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_GAS_AND_ELECTRICITY", "Gas and electricity bills"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_INTERNET_AND_CABLE", "Internet and cable bills"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_RENT", "Rent payment"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT", "Sewage and garbage disposal bills"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_TELEPHONE", "Cell phone bills"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_WATER", "Water bills"},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_OTHER_UTILITIES", "Other miscellaneous utility bills"},
}

// deltaSpendCategories are the six own values the vendored taxonomy
// has no room for: an own-account move, cash whose eventual use is
// unobservable, a bill for a card whose purchases are not itemised, a
// cash gift with no merchant behind it, capital deployed to a
// destination the product does not track, and a line nothing could
// place.
var deltaSpendCategories = []SpendCategory{
	{SpendDetailedInternalTransfer, SpendDetailedInternalTransfer,
		"Movement between two accounts the product already tracks — card payments, funding wires, mortgage payments"},
	{SpendDetailedCashWithdrawal, SpendDetailedCashWithdrawal,
		"Cash taken out at an ATM or a counter; what it was then spent on is unobservable"},
	{SpendDetailedCardSpend, SpendDetailedCardSpend,
		"Credit-card bill for a card not itemised in wealthdb — generic card spend; replaced by the card's own purchases once the card is collected"},
	{SpendDetailedGift, SpendDetailedGift,
		"Cash gift or family support — spending, with no merchant behind it; not a gift item bought in a shop, and not a donation to a non-profit"},
	{SpendDetailedInvestment, SpendDetailedInvestment,
		"Capital deployed from a cash account — a securities subscription, a deposit into a wallet — whose destination the product does not track; not consumed, and not an own-account move"},
	{SpendDetailedOther, SpendDetailedOther,
		"Spend that no rule, matcher or model could place in another category"},
}

// extensionSpendCategories are detailed values of OURS that sit under
// a vendored primary and that the model tier MAY emit.
//
// A household's software subscriptions have no home in the vendored
// vocabulary: it has ELECTRONICS for physical goods, ONLINE
// MARKETPLACES for retail and INTERNET AND CABLE for an ISP, and none
// of those is a password manager, a mailbox, an office suite or a
// model subscription. Left to itself every tier files them somewhere
// false — the model reaches for "other general services", and an
// issuer's own MCC has been seen calling one "other general
// merchandise", which is a physical-goods bucket for something that
// was never a good.
//
// Under GENERAL_SERVICES rather than as a primary of its own, which is
// the one real choice here. A delta earns its own primary because it
// is not a merchant category at all and must not fold into a
// plausible-looking one; digital services IS a merchant category, so
// it belongs beside EDUCATION, INSURANCE and STORAGE, and rolls up
// with them. It also means that if the vendored taxonomy adds this
// value it lands in the same place and the diff is a supersession.
var extensionSpendCategories = []SpendCategory{
	{"GENERAL_SERVICES", SpendDetailedDigitalServices,
		"Software and online subscriptions — SaaS, cloud storage and hosting, VPNs, password managers, AI assistants; not the internet connection itself and not a physical device"},
}

// SpendCategories is the whole taxonomy — the vendored pairs, then the
// extensions, then the deltas. Ordered for readable diffs; the seed migrations (0040,
// 0045-0047) were generated from it and TestSpendCategoriesMatchGoTable
// compares the two as sets, so the order carries no contract.
var SpendCategories = append(append(append(
	make([]SpendCategory, 0, len(vendoredSpendCategories)+len(extensionSpendCategories)+len(deltaSpendCategories)),
	vendoredSpendCategories...), extensionSpendCategories...), deltaSpendCategories...)

// modelSpendCategories is the vocabulary a model may choose from: the
// vendored rows plus the extensions, and never a delta.
var modelSpendCategories = append(append(
	make([]SpendCategory, 0, len(vendoredSpendCategories)+len(extensionSpendCategories)),
	vendoredSpendCategories...), extensionSpendCategories...)

// spendDetailedValues is the validity set: every recognised
// spend_detailed value. Unlike the hand-written `map[T]struct{}` sets
// in enums.go it is derived from the table above rather than restated
// — those vocabularies are short enough to keep honest by eye, a table
// this size is not. Only membership is kept: rolling a detailed value
// up to its primary is gold's job, through the seeded
// `spend_categories` dimension, so Go carries no such lookup.
var spendDetailedValues = indexSpendCategories(SpendCategories)

// spendPrimaryOf maps every detailed value to its primary, so a
// caller can read a value's bucket without scanning the table.
var spendPrimaryOf = indexSpendPrimaries(SpendCategories)

func indexSpendPrimaries(cats []SpendCategory) map[string]string {
	m := make(map[string]string, len(cats))
	for _, c := range cats {
		m[c.Detailed] = c.Primary
	}
	return m
}

// modelSpendDetailedValues is the same set over the rows a model may
// emit — vendored plus extension — so the delta values are absent from
// it rather than filtered out of it at every call site.
var modelSpendDetailedValues = indexSpendCategories(modelSpendCategories)

func indexSpendCategories(cats []SpendCategory) map[string]struct{} {
	m := make(map[string]struct{}, len(cats))
	for _, c := range cats {
		m[c.Detailed] = struct{}{}
	}
	return m
}

// ValidSpendDetailed reports whether s is a recognised spend_detailed
// value — a vendored Plaid detailed value, an extension of ours, or
// one of the six deltas. A primary on its own is not valid unless it
// is also a delta.
func ValidSpendDetailed(s string) bool {
	_, ok := spendDetailedValues[s]
	return ok
}

// ModelSpendDetailed is the stricter sibling: it recognises the values
// a model may emit — vendored and extension — and REFUSES the six
// deltas, because every delta is decided from structure a
// merchant-keyed verdict cannot see. The categorisation gauntlet
// validates against this predicate; ValidSpendDetailed stays the
// storage-level check. What each delta is decided from, and what one
// emitted by a model would do to a report: docs/SPENDING.md §2.
func ModelSpendDetailed(s string) bool {
	_, ok := modelSpendDetailedValues[s]
	return ok
}

// spendLabelAcronyms are the words the mechanical rule would sentence-case
// wrongly. Two, and both are initialisms the vendored taxonomy spells in
// full caps because every value is in full caps.
var spendLabelAcronyms = map[string]string{"Atm": "ATM", "Tv": "TV"}

// spendLabelOverrides are the values whose display name is not what the
// mechanical rule reads off them. One so far: `card_spend`, which the
// rule renders "Card spend" — true of every card purchase in the
// product, so among the merchant categories on a chart it reads as a
// KIND of spending rather than as the placeholder it is. The value
// stands for a bill on a card wealthdb does not itemise (migration
// 0046): real consumption whose purchases nobody has seen, in the base
// and replaced by those purchases the day the card is collected.
// "Uncategorized card spend" says both halves.
var spendLabelOverrides = map[string]string{
	SpendDetailedCardSpend: "Uncategorized card spend",
}

// SpendLabel is a detailed value's display name: the vendored value with
// its primary's prefix taken off, underscores opened out and one capital
// at the front. `FOOD_AND_DRINK_GROCERIES` reads "Groceries";
// `GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE` reads "Other general
// merchandise"; a delta, which is its own primary, reads "Internal
// transfer". A value the rule reads wrongly is in spendLabelOverrides.
//
// The label is presentation only. The value stays the join key, the name
// a rule and a pin write, and what the model gauntlet validates — so a
// taxonomy refresh still diffs against the vendored spelling, and nothing
// downstream depends on how a label reads. An unknown value is returned
// unchanged rather than guessed at.
//
// Nothing at run time calls this: the labels are SEEDED into
// spend_categories by migration 0058, so a label can be corrected by
// hand without the correction being computed away. This is the rule that
// seed was generated from, and TestSpendCategoryLabelsMatchGoTable holds
// the two together — which is why its only caller is a test.
func SpendLabel(detailed string) string {
	if label, ok := spendLabelOverrides[detailed]; ok {
		return label
	}
	primary, ok := spendPrimaryOf[detailed]
	if !ok {
		return detailed
	}
	s := detailed
	if primary != detailed {
		s = detailed[len(primary)+1:]
	}
	return humanise(s)
}

// SpendPrimaryLabel is the same for a primary: "General merchandise",
// "Rent and utilities", "Gift" — overrides included, since a delta is
// its own primary and is labelled at both levels.
func SpendPrimaryLabel(primary string) string {
	if label, ok := spendLabelOverrides[primary]; ok {
		return label
	}
	return humanise(primary)
}

func humanise(s string) string {
	words := strings.Split(strings.ToLower(s), "_")
	for i, w := range words {
		if i == 0 && w != "" {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		if fixed, ok := spendLabelAcronyms[strings.ToUpper(w[:1])+w[1:]]; ok {
			w = fixed
		}
		words[i] = w
	}
	return strings.Join(words, " ")
}

// CatchAllSpendDetailed reports whether s is a primary's own catch-all
// — the value that says only "somewhere in this primary, and nothing
// finer". Every one spells its detail part `OTHER_...`, which is the
// vendored taxonomy's own convention and the reason this can be read
// off the value rather than listed.
//
// It exists so a tier can decline a row it can only place in a
// catch-all. A catch-all is not a verdict: it carries no more
// information than the primary already did, and a tier that claims a
// row with one pre-empts a later tier that could have read the
// merchant name and done better. The six deltas are not catch-alls —
// `other` names a movement the taxonomy has no merchant word for, and
// is a deliberate verdict about what the row IS.
func CatchAllSpendDetailed(s string) bool {
	primary, ok := spendPrimaryOf[s]
	if !ok || primary == s {
		return false // unknown, or a delta, which is primary-level
	}
	return strings.HasPrefix(s[len(primary)+1:], "OTHER_")
}

// DeltaSpendCategories returns the delta rows — the values the model
// tier must never emit, with the descriptions that say why each exists.
// A copy, for the same reason VendoredSpendCategories hands one out.
func DeltaSpendCategories() []SpendCategory {
	return append(make([]SpendCategory, 0, len(deltaSpendCategories)), deltaSpendCategories...)
}

// ModelSpendCategories returns the rows a model may choose from — the
// vendored vocabulary plus the extensions — with the descriptions that
// tell it what each value means. A copy, so a caller assembling a
// prompt cannot reorder the table the seed migrations are pinned to.
func ModelSpendCategories() []SpendCategory {
	return append(make([]SpendCategory, 0, len(modelSpendCategories)), modelSpendCategories...)
}

// VendoredSpendCategories returns the vendored rows alone — what a
// taxonomy refresh diffs against. A copy, for the same reason.
func VendoredSpendCategories() []SpendCategory {
	return append(make([]SpendCategory, 0, len(vendoredSpendCategories)), vendoredSpendCategories...)
}
