package canonical

import "strings"

// The two-level category vocabulary behind gold's `spend_categories`
// dimension: the `spend_detailed` column of the spending overlay and
// the `income_detailed` column of the income one. The primary is the
// coarse bucket a report groups by; the detailed value is what the
// enrichment pass assigns to a transaction, to a merchant or to a
// payer.
//
// TWO FAMILIES, ONE TABLE. Every row carries a Family — `spending`,
// `income`, or `both` for the deltas that mean the same thing read
// from either side. Every predicate and accessor below is derived from
// that column rather than from a restated list, so a value added here
// is admitted or refused everywhere at once, and a spending rule
// cannot start accepting an income value because the income side grew.
//
// PROVENANCE. The vendored pairs are Plaid's Personal Finance
// Category taxonomy (transactions-personal-finance-category-taxonomy.csv,
// retrieved 2026-09-04: 16 primaries, 104 detailed pairs). They are
// vendored rather than fetched so a taxonomy revision arrives as a
// reviewable diff instead of silently re-labelling history. Values and
// descriptions are copied verbatim — only trailing whitespace is
// trimmed — so a refreshed CSV diffs cleanly against this table.
//
// Thirteen of the sixteen primaries are kept. Twelve outflow primaries
// (80 detailed values) are vendored into the spending family, INCOME
// (7 values) into the income family. The three dropped describe movements neither
// family books as its own: TRANSFER_IN and TRANSFER_OUT are the
// own-account move the matcher already names `internal_transfer`, and
// LOAN_PAYMENTS is the load-bearing drop — a mortgage payment is an
// own-account move to a tracked AccountKindMortgage (docs/SPENDING.md
// §2), and an instalment to a lender the product does not track is the
// `debt_repayment` delta rather than a merchant category.
// The interest-versus-principal split of such an instalment is the
// statement's, not this vocabulary's: it needs the lender's own
// balance, which no category can carry (docs/CASHFLOW.md §5).
//
// Seventeen delta values are ours rather than Plaid's and are
// primary-level (primary == detailed, so they group as their own
// bucket): `internal_transfer`, `cash_withdrawal`, `card_spend`,
// `gift`, `investment`, `other`, `debt_repayment`, `capital_return`,
// `loan_proceeds`, `reimbursement`, `inheritance`, `cash_deposit`,
// `deposit_transfer` and
// the four vehicle crossings — `retirement_transfer`,
// `education_transfer`, `health_transfer` and `trust_transfer`. They
// keep the repo's lowercase enum idiom, which also marks them at a
// glance as not-from-Plaid. Eight of them — `internal_transfer`,
// `gift`, `other` and the five crossings — are one row read from
// either side, which is what FamilyBoth means.
//
// EXTENSIONS are the third class, and they differ from the deltas in
// the one way that matters: a model MAY emit them. A delta is decided
// from structure a counterparty's name cannot reveal — whose account
// the money went to, whether a card is itemised, whether an arriving
// sum was earned or borrowed — so the gauntlet refuses one. An
// extension is the opposite: an ordinary judgement about a merchant or
// a payer for which the vendored vocabulary simply has no word yet. It
// is therefore shaped like a vendored row, `<PRIMARY>_<DETAIL>` under
// an existing primary, so that when the taxonomy does catch up the
// refreshed CSV supersedes ours as a clean diff rather than sitting
// beside it.
//
// The policy each delta encodes — what is and is not spending, what is
// and is not income, which tier places it — is docs/SPENDING.md §2 and
// docs/INCOME.md §2; it is not restated here.

// Family says which vocabulary a row belongs to. The two families ask
// different questions of the same counterparty — what was bought, and
// what was received — so most values answer to one of them; a delta
// whose meaning does not depend on the direction of the money answers
// to both.
type Family string

const (
	FamilySpending Family = "spending"
	FamilyIncome   Family = "income"
	FamilyBoth     Family = "both"
)

// InFamily reports whether a row of family f is part of want's
// vocabulary. FamilyBoth is part of either, which is the whole of what
// a shared delta is; every other family is part of its own alone.
func (f Family) InFamily(want Family) bool { return f == want || f == FamilyBoth }

// SpendCategory is one (primary, detailed, description, family) row of
// the taxonomy. Gold's `spend_categories` dimension is seeded from this
// table — migration 0040 seeded the vendored spending rows and the
// first three deltas, 0045/0046/0047 one delta each, 0056 and 0065 the
// extensions, 0069 the whole income side and the family column, 0076
// one extension, 0078 the five the cash flow statement needed — and a
// generator-style test pins the migrated dimension to the table so they
// cannot drift. A new value is a row here plus a new migration; an
// applied migration is never edited.
type SpendCategory struct {
	Primary     string
	Detailed    string
	Description string
	Family      Family
}

// The spending delta values. A rule, a pin, the internal-transfer
// matcher or the provider tier — where the provider's own booking type
// names the movement — assigns these; the model tier never emits them.
const (
	SpendDetailedInternalTransfer = "internal_transfer"
	SpendDetailedCashWithdrawal   = "cash_withdrawal"
	SpendDetailedCardSpend        = "card_spend"
	SpendDetailedGift             = "gift"
	SpendDetailedInvestment       = "investment"
	SpendDetailedOther            = "other"
	// SpendDetailedDebtRepayment is the outflow mirror of
	// `loan_proceeds`: an instalment to a lender the product does not
	// track. It is what the dropped LOAN_PAYMENTS primary was for,
	// minus the interest share nothing in the data splits out.
	SpendDetailedDebtRepayment = "debt_repayment"
)

// The income delta values, placed by the same tiers. The three shared
// with spending keep their Spend* names, since one row cannot have two
// constants: `internal_transfer`, `gift` and `other` are read from
// either side.
const (
	IncomeDetailedCapitalReturn = "capital_return"
	IncomeDetailedLoanProceeds  = "loan_proceeds"
	IncomeDetailedReimbursement = "reimbursement"
	IncomeDetailedInheritance   = "inheritance"
	IncomeDetailedCashDeposit   = "cash_deposit"
)

// The vehicle crossings: a move between the household and a pool whose
// far side the product does not hold — four of them earmarked by a tax
// wrapper, and `deposit_transfer` below with no wrapper at all. Read from
// either side like `internal_transfer`, and for the same reason — one
// movement, two possible legs — so they carry neutral names rather
// than a family's.
//
// The direction is the ROW's, never the value's: a withdrawal placed
// `retirement_transfer` is a contribution and a deposit placed the same
// is a distribution. One value per pool rather than one for all four,
// because money set aside for retirement and money set aside for a
// child's education are different decisions read at different stages of
// a life — and because the alternative would need a rule to carry a
// second field saying which pool it meant.
const (
	DetailedRetirementTransfer = "retirement_transfer"
	DetailedEducationTransfer  = "education_transfer"
	DetailedHealthTransfer     = "health_transfer"
	DetailedTrustTransfer      = "trust_transfer"
	// DetailedDepositTransfer is the same crossing with no wrapper
	// behind it: the household's own cash, moved into a bank's deposit
	// PRODUCT — a call deposit, a fixed-term deposit, a notice account
	// — that the collector never lists as an account because the bank
	// books it under the funding account rather than beside it.
	//
	// It is a delta rather than `internal_transfer` because nothing can
	// pair it: the far leg does not exist in the product at all, so the
	// move reaches the resolution one-legged and would otherwise read
	// as a crossing to an account nobody collects. Placing it by
	// VERDICT is what lets both legs carry it — the outflow through the
	// spending overlay, the return through income's — where a far
	// account could only ever have been written on the one leg the
	// spending overlay holds.
	DetailedDepositTransfer = "deposit_transfer"
)

// The spending extension values: ours, but shaped like the vendored
// rows and emittable by the model tier. See extensionSpendCategories.
const (
	SpendDetailedDigitalServices = "GENERAL_SERVICES_DIGITAL_SERVICES"
	SpendDetailedInvestmentFees  = "BANK_FEES_INVESTMENT_FEES"
	SpendDetailedWithholdingTax  = "GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX"
)

// The income extension values. See extensionIncomeCategories.
const (
	IncomeDetailedSelfEmployment         = "INCOME_SELF_EMPLOYMENT"
	IncomeDetailedGovernmentBenefits     = "INCOME_GOVERNMENT_BENEFITS"
	IncomeDetailedRent                   = "INCOME_RENT"
	IncomeDetailedRoyalties              = "INCOME_ROYALTIES"
	IncomeDetailedAlimonyAndChildSupport = "INCOME_ALIMONY_AND_CHILD_SUPPORT"
	IncomeDetailedStaking                = "INCOME_STAKING"
	IncomeDetailedRewards                = "INCOME_REWARDS"
	IncomeDetailedDistributions          = "INCOME_DISTRIBUTIONS"
	IncomeDetailedInsurancePayout        = "INCOME_INSURANCE_PAYOUT"
)

// vendoredSpendCategories is the outflow half of the Plaid subset, in
// the source CSV's order and grouped by primary as it groups them.
var vendoredSpendCategories = []SpendCategory{
	{"BANK_FEES", "BANK_FEES_ATM_FEES", "Fees incurred for out-of-network ATMs", FamilySpending},
	{"BANK_FEES", "BANK_FEES_FOREIGN_TRANSACTION_FEES", "Fees incurred on non-domestic transactions", FamilySpending},
	{"BANK_FEES", "BANK_FEES_INSUFFICIENT_FUNDS", "Fees relating to insufficient funds", FamilySpending},
	{"BANK_FEES", "BANK_FEES_INTEREST_CHARGE", "Fees incurred for interest on purchases, including not-paid-in-full or interest on cash advances", FamilySpending},
	{"BANK_FEES", "BANK_FEES_OVERDRAFT_FEES", "Fees incurred when an account is in overdraft", FamilySpending},
	{"BANK_FEES", "BANK_FEES_OTHER_BANK_FEES", "Other miscellaneous bank fees", FamilySpending},

	{"ENTERTAINMENT", "ENTERTAINMENT_CASINOS_AND_GAMBLING", "Gambling, casinos, and sports betting", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_MUSIC_AND_AUDIO", "Digital and in-person music purchases, including music streaming services", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS", "Purchases made at sporting events, music venues, concerts, museums, and amusement parks", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_TV_AND_MOVIES", "In home movie streaming services and movie theaters", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_VIDEO_GAMES", "Digital and in-person video game purchases", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_OTHER_ENTERTAINMENT", "Other miscellaneous entertainment purchases, including night life and adult entertainment", FamilySpending},

	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR", "Beer, Wine & Liquor Stores", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE", "Purchases at coffee shops or cafes", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_FAST_FOOD", "Dining expenses for fast food chains", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", "Purchases for fresh produce and groceries, including farmers' markets", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT", "Dining expenses for restaurants, bars, gastropubs, and diners", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_VENDING_MACHINES", "Purchases made at vending machine operators", FamilySpending},
	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK", "Other miscellaneous food and drink, including desserts, juice bars, and delis", FamilySpending},

	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS", "Books, magazines, and news", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES", "Apparel, shoes, and jewelry", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_CONVENIENCE_STORES", "Purchases at convenience stores", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_DEPARTMENT_STORES", "Retail stores with wide ranges of consumer goods, typically specializing in clothing and home goods", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_DISCOUNT_STORES", "Stores selling goods at a discounted price", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ELECTRONICS", "Electronics stores and websites", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES", "Photo, gifts, cards, and floral stores", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OFFICE_SUPPLIES", "Stores that specialize in office goods", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_ONLINE_MARKETPLACES", "Multi-purpose e-commerce platforms such as Etsy, Ebay and Amazon", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_PET_SUPPLIES", "Pet supplies and pet food", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SPORTING_GOODS", "Sporting goods, camping gear, and outdoor equipment", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_SUPERSTORES", "Superstores such as Target and Walmart, selling both groceries and general merchandise", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_TOBACCO_AND_VAPE", "Purchases for tobacco and vaping products", FamilySpending},
	{"GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "Other miscellaneous merchandise, including toys, hobbies, and arts and crafts", FamilySpending},

	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_FURNITURE", "Furniture, bedding, and home accessories", FamilySpending},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_HARDWARE", "Building materials, hardware stores, paint, and wallpaper", FamilySpending},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE", "Plumbing, lighting, gardening, and roofing", FamilySpending},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_SECURITY", "Home security system purchases", FamilySpending},
	{"HOME_IMPROVEMENT", "HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT", "Other miscellaneous home purchases, including pool installation and pest control", FamilySpending},

	{"MEDICAL", "MEDICAL_DENTAL_CARE", "Dentists and general dental care", FamilySpending},
	{"MEDICAL", "MEDICAL_EYE_CARE", "Optometrists, contacts, and glasses stores", FamilySpending},
	{"MEDICAL", "MEDICAL_NURSING_CARE", "Nursing care and facilities", FamilySpending},
	{"MEDICAL", "MEDICAL_PHARMACIES_AND_SUPPLEMENTS", "Pharmacies and nutrition shops", FamilySpending},
	{"MEDICAL", "MEDICAL_PRIMARY_CARE", "Doctors and physicians", FamilySpending},
	{"MEDICAL", "MEDICAL_VETERINARY_SERVICES", "Prevention and care procedures for animals", FamilySpending},
	{"MEDICAL", "MEDICAL_OTHER_MEDICAL", "Other miscellaneous medical, including blood work, hospitals, and ambulances", FamilySpending},

	{"PERSONAL_CARE", "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS", "Gyms, fitness centers, and workout classes", FamilySpending},
	{"PERSONAL_CARE", "PERSONAL_CARE_HAIR_AND_BEAUTY", "Manicures, haircuts, waxing, spa/massages, and bath and beauty products", FamilySpending},
	{"PERSONAL_CARE", "PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING", "Wash and fold, and dry cleaning expenses", FamilySpending},
	{"PERSONAL_CARE", "PERSONAL_CARE_OTHER_PERSONAL_CARE", "Other miscellaneous personal care, including mental health apps and services", FamilySpending},

	{"GENERAL_SERVICES", "GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING", "Financial planning, and tax and accounting services", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_AUTOMOTIVE", "Oil changes, car washes, repairs, and towing", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CHILDCARE", "Babysitters and daycare", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CONSULTING_AND_LEGAL", "Consulting and legal services", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_EDUCATION", "Elementary, high school, professional schools, and college tuition", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_INSURANCE", "Insurance for auto, home, and healthcare", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_POSTAGE_AND_SHIPPING", "Mail, packaging, and shipping services", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_STORAGE", "Storage services and facilities", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "Other miscellaneous services, including advertising and cloud storage", FamilySpending},

	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_DONATIONS", "Charitable, political, and religious donations", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES", "Government departments and agencies, such as driving licences, and passport renewal", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT", "Tax payments, including income and property taxes", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT", "Other miscellaneous government and non-profit agencies", FamilySpending},

	{"TRANSPORTATION", "TRANSPORTATION_BIKES_AND_SCOOTERS", "Bike and scooter rentals", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_GAS", "Purchases at a gas station", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_PARKING", "Parking fees and expenses", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_PUBLIC_TRANSIT", "Public transportation, including rail and train, buses, and metro", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_TAXIS_AND_RIDE_SHARES", "Taxi and ride share services", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_TOLLS", "Toll expenses", FamilySpending},
	{"TRANSPORTATION", "TRANSPORTATION_OTHER_TRANSPORTATION", "Other miscellaneous transportation expenses", FamilySpending},

	{"TRAVEL", "TRAVEL_FLIGHTS", "Airline expenses", FamilySpending},
	{"TRAVEL", "TRAVEL_LODGING", "Hotels, motels, and hosted accommodation such as Airbnb", FamilySpending},
	{"TRAVEL", "TRAVEL_RENTAL_CARS", "Rental cars, charter buses, and trucks", FamilySpending},
	{"TRAVEL", "TRAVEL_OTHER_TRAVEL", "Other miscellaneous travel expenses", FamilySpending},

	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_GAS_AND_ELECTRICITY", "Gas and electricity bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_INTERNET_AND_CABLE", "Internet and cable bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_RENT", "Rent payment", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT", "Sewage and garbage disposal bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_TELEPHONE", "Cell phone bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_WATER", "Water bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_OTHER_UTILITIES", "Other miscellaneous utility bills", FamilySpending},
}

// vendoredIncomeCategories is Plaid's INCOME primary, whole. It is the
// first primary in the source CSV and was dropped by the original
// vendoring because nothing read it yet; the seven values and their
// descriptions are the CSV's, untouched, so a refresh diffs against
// them as it does against the outflow rows.
var vendoredIncomeCategories = []SpendCategory{
	{"INCOME", "INCOME_DIVIDENDS", "Dividends from investment accounts", FamilyIncome},
	{"INCOME", "INCOME_INTEREST_EARNED", "Income from interest on savings accounts", FamilyIncome},
	{"INCOME", "INCOME_RETIREMENT_PENSION", "Income from pension payments", FamilyIncome},
	{"INCOME", "INCOME_TAX_REFUND", "Income from tax refunds", FamilyIncome},
	{"INCOME", "INCOME_UNEMPLOYMENT", "Income from unemployment benefits, including unemployment insurance and healthcare", FamilyIncome},
	{"INCOME", "INCOME_WAGES", "Income from salaries, gig-economy work, and tips earned", FamilyIncome},
	{"INCOME", "INCOME_OTHER_INCOME", "Other miscellaneous income, including alimony, social security, child support, and rental", FamilyIncome},
}

// deltaCategories are the sixteen own values the vendored taxonomy has
// no room for, both families in one table because seven of them are
// one value read from either side.
//
// The spending seven: an own-account move, cash whose eventual use is
// unobservable, a bill for a card whose purchases are not itemised, a
// cash gift with no merchant behind it, capital deployed to a
// destination the product does not track, a line nothing could place,
// and an instalment to an untracked lender.
//
// The income five that have no spending twin: capital of the holder's
// own coming back, money borrowed arriving, money back for money
// spent, an estate's distribution, and cash paid in over a counter.
// `capital_return`, `loan_proceeds` and `reimbursement` are excluded
// from the income base the way `investment` is excluded from the
// spending one — each names money that arrived without being earned —
// while `inheritance` and `cash_deposit` are receipts in their own
// right and stay in it.
//
// The four vehicle crossings leave BOTH bases, like `internal_transfer`
// and for the same reason: the money is still the holder's, and a
// crossing is neither a receipt nor a thing bought. `debt_repayment`
// leaves the spending base for the reason `investment` does — it
// reduces a liability rather than buying anything — and, like the
// crossings, it exists so the cashflow statement has an honest home for
// a movement the two families decline.
//
// A delta is primary-level, so a report grouped by primary shows it as
// its own bucket, and gold reads delta-ness off that equality rather
// than off a list (migration 0048).
var deltaCategories = []SpendCategory{
	{SpendDetailedInternalTransfer, SpendDetailedInternalTransfer,
		"Movement between two accounts the product already tracks, either leg — card payments, funding wires, mortgage payments, a pension contribution arriving", FamilyBoth},
	{SpendDetailedCashWithdrawal, SpendDetailedCashWithdrawal,
		"Cash taken out at an ATM or a counter; what it was then spent on is unobservable", FamilySpending},
	{SpendDetailedCardSpend, SpendDetailedCardSpend,
		"Credit-card bill for a card not itemised in wealthdb — generic card spend; replaced by the card's own purchases once the card is collected", FamilySpending},
	{SpendDetailedGift, SpendDetailedGift,
		"A cash gift or family support, given or received — with no merchant, employer or issuer behind it; not a gift item bought in a shop, and not a donation to a non-profit", FamilyBoth},
	{SpendDetailedInvestment, SpendDetailedInvestment,
		"Capital deployed from a cash account — a securities subscription, a deposit into a wallet — whose destination the product does not track; not consumed, and not an own-account move", FamilySpending},
	{SpendDetailedOther, SpendDetailedOther,
		"Money moved that no rule, matcher or model could place — a payment on the outflow side, a receipt on the inflow side", FamilyBoth},
	{SpendDetailedDebtRepayment, SpendDetailedDebtRepayment,
		"An instalment paid to a lender the product does not track — a car or student loan serviced, a credit line paid down; principal and interest together, a liability reduced rather than anything consumed. The outflow mirror of `loan_proceeds`; a payment to a lender gold DOES hold is an own-account move and pairs", FamilySpending},

	{IncomeDetailedCapitalReturn, IncomeDetailedCapitalReturn,
		"Capital of the holder's own coming back from a destination the product does not track — a private fund returning contributed basis, a loan the holder made repaid, a personal asset sold, a deposit refunded; returned rather than earned", FamilyIncome},
	{IncomeDetailedLoanProceeds, IncomeDetailedLoanProceeds,
		"Money borrowed arriving from a lender the product does not track — a loan disbursed, a mortgage or a credit line drawn; a liability incurred rather than income", FamilyIncome},
	{IncomeDetailedReimbursement, IncomeDetailedReimbursement,
		"Money back for money spent, where the outflow it answers is identifiable — an expense claim settled, a utility credit against a bill, a merchant reversing its own charge; a repayment of an outflow rather than income. An insurance payout is not this and is income (INCOME_INSURANCE_PAYOUT): the premium was already counted as spending, and nothing links a payout to the premiums it answers", FamilyIncome},
	{IncomeDetailedInheritance, IncomeDetailedInheritance,
		"An estate's distribution to the holder; kept apart from a gift because it arrives once or twice in a life and is often the largest receipt in it", FamilyIncome},
	{IncomeDetailedCashDeposit, IncomeDetailedCashDeposit,
		"Cash paid in at a counter or a machine; where it came from is unobservable", FamilyIncome},

	{DetailedRetirementTransfer, DetailedRetirementTransfer,
		"A move between the holder and a retirement plan the product does not track, either leg — a contribution wired out, a plan payout arriving. The row's own direction says which; the money is the holder's throughout, in a pool earmarked for a stage of life rather than for spending", FamilyBoth},
	{DetailedEducationTransfer, DetailedEducationTransfer,
		"The same crossing for an education plan or savings account the product does not track — money paid in, or drawn out for the costs it was set aside for", FamilyBoth},
	{DetailedHealthTransfer, DetailedHealthTransfer,
		"The same crossing for a health savings account the product does not track — a contribution paid in, or a medical cost reimbursed out of it", FamilyBoth},
	{DetailedTrustTransfer, DetailedTrustTransfer,
		"The same crossing for a trust that is a separate taxpayer and that the product does not track — a funding transfer out, a distribution arriving. A grantor trust is not this: it is tax-transparent and its accounts are the holder's own", FamilyBoth},
	{DetailedDepositTransfer, DetailedDepositTransfer,
		"The same crossing for a bank's own deposit product the collector does not list as an account — a call deposit, a fixed-term deposit, a notice account: money paid in, or the principal coming back. Earmarked for nothing and taxed like the funding account; it is here because the far leg does not exist in the product, not because the money went anywhere. Interest the product pays is NOT this — it is income, and it arrives on its own row", FamilyBoth},
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
//
// The other two came with brokerage accounts (migration 0064). Holding
// investments costs money in two ways the vendored vocabulary cannot
// name, and both arrive in volume: a fee charged for holding or
// managing the assets, and tax withheld at source before the income is
// ever received.
//
// BANK_FEES has ATM_FEES, FOREIGN_TRANSACTION_FEES, INSUFFICIENT_FUNDS,
// INTEREST_CHARGE and OVERDRAFT_FEES — every one of them a fee for
// BANKING. A custodian's ADR depositary charge, a platform's quarterly
// fee and an investment manager's bill are fees for INVESTING, and
// filing them under `OTHER_BANK_FEES` buries the cost of being
// invested inside the cost of having an account. They sit under
// BANK_FEES all the same, because that is the primary for "what a
// financial institution charged", and an extension earns its keep by
// landing where a vendored value would.
//
// Withholding is a tax and belongs beside TAX_PAYMENT, but is not the
// same thing: TAX_PAYMENT is assessed and then paid, while withholding
// is deducted before the money arrives. A report that cannot tell them
// apart cannot answer "what was paid in tax that was never seen" — and
// it is the spending side that carries it, because income is booked
// gross, as the source recorded it arriving (docs/INCOME.md §5).
//
// Both are ordinary judgements about what a row IS, so the model tier
// may emit them — and usefully can, since the signature on a
// withheld-tax row typically reads `NRA TAX <security>`, which names
// the answer.
var extensionSpendCategories = []SpendCategory{
	{"GENERAL_SERVICES", SpendDetailedDigitalServices,
		"Software and online subscriptions — SaaS, cloud storage and hosting, VPNs, password managers, AI assistants; not the internet connection itself and not a physical device", FamilySpending},
	{"BANK_FEES", SpendDetailedInvestmentFees,
		"Fees for holding or managing investments — advisory and management fees, custody and platform fees, and security-level pass-throughs such as ADR depositary charges; not a fee for banking itself", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", SpendDetailedWithholdingTax,
		"Tax withheld at source from investment income, such as foreign dividend or non-resident withholding; deducted before the money is received rather than paid on assessment", FamilySpending},
}

// extensionIncomeCategories are the income side's extensions, under
// the one vendored primary it has. Plaid has no value for most of them
// and files them under its catch-all — OTHER_INCOME's own description
// names alimony, social security, child support and rental — so a
// household reading its own report would see much of what it receives
// filed as "other". The two it does not fold there it files under a
// value that means something else, which is the sharper problem: a
// catch-all can at least be re-asked (`--refine`), a confident wrong
// answer cannot.
//
// The bar each clears is the bar an extension always clears — common,
// distinct on a statement or a tax return, and absent from the
// vendored vocabulary — and it is applied to households in general
// rather than to one, because the product is published. An unused
// value costs a little model precision; a missing one costs everyone
// who needed it a config rule.
//
// Left out deliberately, each because an existing value already says
// it: a bonus or a severance payment (wages), crypto lending interest
// (interest earned), a scholarship or a lottery win (other income),
// and mining, which a config rule places at self-employment or other
// income until it earns a value of its own.
//
// SELF_EMPLOYMENT and GOVERNMENT_BENEFITS are the two the vendored
// vocabulary comes closest to and still misses: Plaid has WAGES, which
// a tax return does not read a contractor's invoices as, and it names
// two state transfers — a pension and unemployment — out of the many a
// state makes. ALIMONY_AND_CHILD_SUPPORT is person-shaped, so the
// fence keeps it from the model and a rule or a pin places it; it is
// an extension all the same, because what it names is a kind of
// income rather than a structural fact about an account.
//
// STAKING and DISTRIBUTIONS carry a policy each. Staking is kept apart
// from interest for the reason gold keeps the `staking` kind apart
// from `interest`: jurisdictions tax the two differently.
// DISTRIBUTIONS is a fund's payout of realised gains on a holding
// still held — it returns no basis, so it is income, and it is kept
// apart from DIVIDENDS so that the vendored value keeps Plaid's
// meaning.
var extensionIncomeCategories = []SpendCategory{
	{"INCOME", IncomeDetailedSelfEmployment,
		"Freelance, contractor and sole-trader earnings — client invoices, a business's own takings, an owner's draw from their company; not a salary, tips or gig-platform earnings, which are wages", FamilyIncome},
	{"INCOME", IncomeDetailedGovernmentBenefits,
		"State transfers other than a pension or an unemployment benefit — child and family allowances, parental-leave pay, disability and housing benefits, stimulus payments", FamilyIncome},
	{"INCOME", IncomeDetailedRent,
		"Rent received from a tenant, directly or through a letting agent or a property manager; not a tenancy deposit returned and not the proceeds of selling the property", FamilyIncome},
	{"INCOME", IncomeDetailedRoyalties,
		"Royalties and creator payouts — book, music, software-licence and patent royalties, and a platform's share of what a creator's work earned", FamilyIncome},
	{"INCOME", IncomeDetailedAlimonyAndChildSupport,
		"Maintenance received from a former partner or a parent — alimony, spousal maintenance, child support; not a cash gift and not family support given freely", FamilyIncome},
	{"INCOME", IncomeDetailedInsurancePayout,
		"What an insurer pays out on a policy — a claim settled, a damage or health cost covered, a premium refunded on cancellation. Income rather than a reimbursement because the premium that bought the cover was already counted as spending, and nothing links a payout back to the premiums it answers: netting the payout out would count the outflow and drop the inflow", FamilyIncome},
	{"INCOME", IncomeDetailedStaking,
		"Proof-of-stake rewards and validator income earned by committing a crypto holding; kept apart from interest because jurisdictions tax the two differently", FamilyIncome},
	{"INCOME", IncomeDetailedRewards,
		"Card cashback, statement credits, account-opening and referral bonuses, and airdrops — earned on spending or on holding an account, and never netted against the spending itself", FamilyIncome},
	{"INCOME", IncomeDetailedDistributions,
		"A fund's cash payout of realised gains on a holding still held, returning no basis; not a company's dividend, and not a private fund returning contributed capital", FamilyIncome},
}

// SpendCategories is the whole taxonomy, both families — the vendored
// pairs, then the extensions, then the deltas. Ordered for readable
// diffs; the seed migrations (0040, 0045-0047, 0056, 0065, 0069) were
// generated from it and TestSpendCategoriesMatchGoTable compares the
// two as sets, so the order carries no contract.
var SpendCategories = concatCategories(
	vendoredSpendCategories, vendoredIncomeCategories,
	extensionSpendCategories, extensionIncomeCategories,
	deltaCategories)

// modelSpendCategories is the vocabulary a model may choose a spend
// category from: the vendored outflow rows plus their extensions, and
// never a delta. modelIncomeCategories is the same for income types.
var (
	modelSpendCategories  = concatCategories(vendoredSpendCategories, extensionSpendCategories)
	modelIncomeCategories = concatCategories(vendoredIncomeCategories, extensionIncomeCategories)
)

func concatCategories(tables ...[]SpendCategory) []SpendCategory {
	n := 0
	for _, t := range tables {
		n += len(t)
	}
	out := make([]SpendCategory, 0, n)
	for _, t := range tables {
		out = append(out, t...)
	}
	return out
}

// The validity sets: every recognised value of each family. Unlike the
// hand-written `map[T]struct{}` sets in enums.go they are derived from
// the tables above rather than restated — those vocabularies are short
// enough to keep honest by eye, a table this size is not. Only
// membership is kept: rolling a detailed value up to its primary is
// gold's job, through the seeded `spend_categories` dimension, so Go
// carries no such lookup.
//
// The model sets are the same over the rows a model may emit —
// vendored plus extension — so the delta values are absent from them
// rather than filtered out of them at every call site.
var (
	spendDetailedValues       = indexCategories(FamilySpending, SpendCategories)
	incomeDetailedValues      = indexCategories(FamilyIncome, SpendCategories)
	modelSpendDetailedValues  = indexCategories(FamilySpending, modelSpendCategories)
	modelIncomeDetailedValues = indexCategories(FamilyIncome, modelIncomeCategories)
)

// primaryOf maps every detailed value to its primary, so a caller can
// read a value's bucket without scanning the table. Detailed values
// are unique across both families, so one map serves them.
var primaryOf = indexPrimaries(SpendCategories)

func indexPrimaries(cats []SpendCategory) map[string]string {
	m := make(map[string]string, len(cats))
	for _, c := range cats {
		m[c.Detailed] = c.Primary
	}
	return m
}

func indexCategories(family Family, cats []SpendCategory) map[string]struct{} {
	m := make(map[string]struct{}, len(cats))
	for _, c := range cats {
		if c.Family.InFamily(family) {
			m[c.Detailed] = struct{}{}
		}
	}
	return m
}

// categoriesIn returns the rows of cats that belong to family, in
// order. A copy by construction, which is what the accessors below
// hand out.
func categoriesIn(family Family, cats []SpendCategory) []SpendCategory {
	out := make([]SpendCategory, 0, len(cats))
	for _, c := range cats {
		if c.Family.InFamily(family) {
			out = append(out, c)
		}
	}
	return out
}

// ValidSpendDetailed reports whether s is a recognised spend_detailed
// value — a vendored Plaid outflow value, a spending extension of
// ours, or one of the eleven deltas the spending side reads. A primary on
// its own is not valid unless it is also a delta, and an income value
// is not valid here: the two families are separate vocabularies that
// happen to share a table, so a spending rule or pin naming
// `INCOME_WAGES` is as wrong as one naming nothing at all.
func ValidSpendDetailed(s string) bool {
	_, ok := spendDetailedValues[s]
	return ok
}

// ValidIncomeDetailed is the same for income_detailed: the seven
// vendored INCOME values, the nine extensions, and the twelve deltas
// the income side reads. Income rules and pins validate against it.
func ValidIncomeDetailed(s string) bool {
	_, ok := incomeDetailedValues[s]
	return ok
}

// ModelSpendDetailed is the stricter sibling: it recognises the values
// a model may emit — vendored and extension — and REFUSES the deltas,
// because every delta is decided from structure a merchant-keyed
// verdict cannot see. The categorisation gauntlet validates against
// this predicate; ValidSpendDetailed stays the storage-level check.
// What each delta is decided from, and what one emitted by a model
// would do to a report: docs/SPENDING.md §2.
func ModelSpendDetailed(s string) bool {
	_, ok := modelSpendDetailedValues[s]
	return ok
}

// ModelIncomeDetailed is the income side's gauntlet check, and refuses
// the twelve income deltas for the same reason. `capital_return` is the
// one that would hurt most: it is what a private fund's distribution
// floors to, it is OUT of the income base, and a payer-keyed verdict
// carrying it would silently remove that payer from every income
// report rather than show a figure that is wrong.
func ModelIncomeDetailed(s string) bool {
	_, ok := modelIncomeDetailedValues[s]
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
// merchandise"; `INCOME_WAGES` reads "Wages"; a delta, which is its own
// primary, reads "Internal transfer". A value the rule reads wrongly is
// in spendLabelOverrides.
//
// One rule for both families: a label is read off a value's spelling,
// which does not depend on which family holds it, and a delta both
// families read has one label as it has one row. IncomeLabel is the
// income side's name for this function.
//
// The label is presentation only. The value stays the join key, the name
// a rule and a pin write, and what the model gauntlet validates — so a
// taxonomy refresh still diffs against the vendored spelling, and nothing
// downstream depends on how a label reads. An unknown value is returned
// unchanged rather than guessed at.
//
// Nothing at run time calls this: a label is SEEDED into
// spend_categories by whichever migration adds or corrects the row
// (0058 seeded them all, 0062 corrected one, 0065 and 0069 carried them
// on the rows they added), so a label can be corrected by hand without
// the correction being computed away. This is the rule those seeds were
// generated from, and TestSpendCategoryLabelsMatchGoTable holds them
// together — which is why its only callers are tests.
func SpendLabel(detailed string) string {
	if label, ok := spendLabelOverrides[detailed]; ok {
		return label
	}
	primary, ok := primaryOf[detailed]
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
// "Rent and utilities", "Income", "Gift" — overrides included, since a
// delta is its own primary and is labelled at both levels.
func SpendPrimaryLabel(primary string) string {
	if label, ok := spendLabelOverrides[primary]; ok {
		return label
	}
	return humanise(primary)
}

// IncomeLabel and IncomePrimaryLabel are the income side's names for
// the one labelling rule, so income code reads its own vocabulary's
// name rather than the outflow side's.
func IncomeLabel(detailed string) string { return SpendLabel(detailed) }

// IncomePrimaryLabel labels an income primary. There is one vendored
// income primary, INCOME, and a delta is its own.
func IncomePrimaryLabel(primary string) string { return SpendPrimaryLabel(primary) }

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

// CatchAllSpendDetailed reports whether s is a spending primary's own
// catch-all — the value that says only "somewhere in this primary, and
// nothing finer". Every one spells its detail part `OTHER_...`, which
// is the vendored taxonomy's own convention and the reason this can be
// read off the value rather than listed.
//
// It exists so a tier can decline a row it can only place in a
// catch-all. A catch-all is not a verdict: it carries no more
// information than the primary already did, and a tier that claims a
// row with one pre-empts a later tier that could have read the
// counterparty's name and done better. The deltas are not catch-alls —
// `other` names a movement the taxonomy has no merchant word for, and
// is a deliberate verdict about what the row IS.
//
// Family-fenced like the validity predicates, so that adding the income
// side changed no answer this gave before it existed:
// `INCOME_OTHER_INCOME` is income's catch-all and not spending's.
func CatchAllSpendDetailed(s string) bool {
	return ValidSpendDetailed(s) && catchAllSpelling(s)
}

// CatchAllIncomeDetailed is the same predicate over the income
// vocabulary, where there is exactly one: `INCOME_OTHER_INCOME`.
func CatchAllIncomeDetailed(s string) bool {
	return ValidIncomeDetailed(s) && catchAllSpelling(s)
}

func catchAllSpelling(s string) bool {
	primary, ok := primaryOf[s]
	if !ok || primary == s {
		return false // unknown, or a delta, which is primary-level
	}
	return strings.HasPrefix(s[len(primary)+1:], "OTHER_")
}

// DeltaSpendCategories returns the spending family's delta rows — the
// values the model tier must never emit, with the descriptions that say
// why each exists. A copy, for the same reason VendoredSpendCategories
// hands one out.
func DeltaSpendCategories() []SpendCategory {
	return categoriesIn(FamilySpending, deltaCategories)
}

// DeltaIncomeCategories is the same for the income family. The three
// shared deltas appear in both lists: one row, read from either side.
func DeltaIncomeCategories() []SpendCategory {
	return categoriesIn(FamilyIncome, deltaCategories)
}

// ModelSpendCategories returns the rows a model may choose a spend
// category from — the vendored outflow vocabulary plus its extensions —
// with the descriptions that tell it what each value means. A copy, so a
// caller assembling a prompt cannot reorder the table the seed
// migrations are pinned to.
func ModelSpendCategories() []SpendCategory {
	return categoriesIn(FamilySpending, modelSpendCategories)
}

// ModelIncomeCategories is the same for the income conversation.
func ModelIncomeCategories() []SpendCategory {
	return categoriesIn(FamilyIncome, modelIncomeCategories)
}

// VendoredSpendCategories returns the vendored outflow rows alone —
// what a taxonomy refresh diffs against. A copy, for the same reason.
func VendoredSpendCategories() []SpendCategory {
	return categoriesIn(FamilySpending, vendoredSpendCategories)
}

// VendoredIncomeCategories returns Plaid's INCOME rows alone, for the
// same purpose.
func VendoredIncomeCategories() []SpendCategory {
	return categoriesIn(FamilyIncome, vendoredIncomeCategories)
}
