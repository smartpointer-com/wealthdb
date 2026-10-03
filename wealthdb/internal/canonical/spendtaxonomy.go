package canonical

import "strings"

// The two-level category vocabulary behind gold's `spend_categories`
// dimension: the `spend_detailed` column of the spending overlay and the
// `income_detailed` column of the income one. The primary is the coarse
// bucket a report groups by; the detailed value is what the enrichment pass
// assigns to a transaction, a merchant or a payer.
//
// Every row carries a Family — `spending`, `income`, or `both` for a value
// that means the same read from either side — and every predicate and
// accessor below derives from that column rather than a restated list, so a
// value added here is admitted or refused everywhere at once.
//
// Three classes of value share the table:
//
//   - VENDORED pairs, copied verbatim from Plaid's Personal Finance Category
//     taxonomy, version 2 (the PFCv2 columns of pfc-taxonomy-all.csv,
//     retrieved 2026-10-03), so a revision arrives as a reviewable diff. The
//     transfer and loan primaries are left out — those movements are the
//     matcher's and the deltas' to name — and so is OTHER, Plaid's bucket
//     for a row it could not place.
//   - DELTAS, ours, lower-case and primary-level: movements decided from
//     structure a counterparty's name cannot reveal — whose account the
//     money went to, whether an arriving sum was earned or borrowed. A model
//     may never emit one.
//   - EXTENSIONS, ours but shaped like a vendored row under an existing
//     primary: an ordinary judgement the vendored vocabulary has no word for
//     yet, which a model may emit and a refreshed CSV supersedes.
//
// The policy each value encodes — what is and is not spending or income,
// and which tier places it — is docs/SPENDING.md §2 and docs/INCOME.md §2.

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
// table, one migration per change — 0040 seeded the vendored spending
// rows, 0069 the income side and the family column, 0111 moved the
// vendored rows to Plaid's version 2 — and a generator-style test pins
// the migrated dimension to the table so they cannot drift. A new value
// is a row here plus a new migration; an applied migration is never
// edited.
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

// DetailedMortgageTransfer is an instalment paid to, or a tranche drawn
// from, a MORTGAGE servicer the product does not hold as an account.
//
// It is a delta like the crossings above and placed by the same tiers,
// but it is not a vehicle crossing: it reaches `financing · Mortgage`,
// where the interest/principal split applies, rather than the vehicles
// section. `debt_repayment` is the neighbouring value and a different
// one — a car loan or a credit line, drawn as `loans`, with no split
// because nothing in the data separates the two halves.
//
// It exists because `financing · Mortgage` was otherwise reachable
// only through a far account of kind `mortgage` or a `far_class` only
// the built-in tier may write. A servicer whose narrative is its own
// legal entity — personal text that cannot go in a tracked built-in —
// had no road to that node at all. A payment to a mortgage gold DOES
// hold is an own-account move and pairs; this is for the one it does
// not.
//
// The direction is the ROW's, as for the crossings: an outflow is an
// instalment and an inflow is a drawdown, and `cashflow_lines_base`
// reads the sign. A servicer with no balance in gold has no principal
// series to apportion against, so its instalments draw whole as
// `Mortgage interest` — right for an interest-only tranche, and a
// visible approximation for any other.
const DetailedMortgageTransfer = "mortgage_transfer"

// The spending extension values: ours, but shaped like the vendored
// rows and emittable by the model tier. See extensionSpendCategories.
const (
	SpendDetailedDigitalServices = "GENERAL_SERVICES_DIGITAL_SERVICES"
	SpendDetailedInvestmentFees  = "BANK_FEES_INVESTMENT_FEES"
	SpendDetailedWithholdingTax  = "GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX"
)

// The income extension values. See extensionIncomeCategories.
const (
	IncomeDetailedGovernmentBenefits = "INCOME_GOVERNMENT_BENEFITS"
	IncomeDetailedRoyalties          = "INCOME_ROYALTIES"
	IncomeDetailedAlimony            = "INCOME_ALIMONY"
	IncomeDetailedStaking            = "INCOME_STAKING"
	IncomeDetailedRewards            = "INCOME_REWARDS"
	IncomeDetailedDistributions      = "INCOME_DISTRIBUTIONS"
	IncomeDetailedInsurancePayout    = "INCOME_INSURANCE_PAYOUT"
	IncomeDetailedEnergyFeedIn       = "INCOME_ENERGY_FEED_IN"
)

// vendoredSpendCategories is the outflow half of the Plaid subset, in
// the source CSV's order and grouped by primary as it groups them.
var vendoredSpendCategories = []SpendCategory{
	{"BANK_FEES", "BANK_FEES_ATM_FEES", "Fees incurred for out-of-network ATMs", FamilySpending},
	{"BANK_FEES", "BANK_FEES_INSUFFICIENT_FUNDS", "Fees relating to insufficient funds", FamilySpending},
	{"BANK_FEES", "BANK_FEES_INTEREST_CHARGE", "Fees incurred for interest on purchases (this excludes cash advance interest fee)", FamilySpending},
	{"BANK_FEES", "BANK_FEES_FOREIGN_TRANSACTION_FEES", "Fees incurred on non-domestic transactions", FamilySpending},
	{"BANK_FEES", "BANK_FEES_OVERDRAFT_FEES", "Penalty payment for overdrafts", FamilySpending},
	{"BANK_FEES", "BANK_FEES_LATE_FEES", "Penalty payment for late payment", FamilySpending},
	{"BANK_FEES", "BANK_FEES_CASH_ADVANCE", "Fees incurred for withdrawing cash using a credit card, including transaction fees and interest fees.", FamilySpending},
	{"BANK_FEES", "BANK_FEES_OTHER_BANK_FEES", "Other miscellaneous bank fees, including annual fee", FamilySpending},

	{"ENTERTAINMENT", "ENTERTAINMENT_CASINOS_AND_GAMBLING", "Gambling, casinos, and sports betting", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_MUSIC_AND_AUDIO", "Digital and in-person music purchases, including music streaming services", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS", "Purchases made at sporting events, music venues, concerts, museums, and amusement parks", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_TV_AND_MOVIES", "In home movie streaming services and movie theaters", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_VIDEO_GAMES", "Digital and in-person video game purchases", FamilySpending},
	{"ENTERTAINMENT", "ENTERTAINMENT_OTHER_ENTERTAINMENT", "Other miscellaneous entertainment purchases, including night life and adult entertainment", FamilySpending},

	{"FOOD_AND_DRINK", "FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR", "Beer, wine, and liquor stores.", FamilySpending},
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

	{"GENERAL_SERVICES", "GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING", "Financial planning, tax, and accounting services.", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_AUTOMOTIVE", "Oil changes, car washes, repairs, and towing", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CHILDCARE", "Babysitters and daycare", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_CONSULTING_AND_LEGAL", "Consulting and legal services", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_EDUCATION", "Elementary, high school, professional schools, and college tuition", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_INSURANCE", "Insurance for auto, home, and healthcare", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_POSTAGE_AND_SHIPPING", "Mail, packaging, and shipping services", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_STORAGE", "Storage services and facilities", FamilySpending},
	{"GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "Other miscellaneous services, including advertising and cloud storage", FamilySpending},

	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_DONATIONS", "Charitable, political, and religious donations", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES", "Government departments and agencies, such as driving licenses, and passport renewal", FamilySpending},
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
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_TELEPHONE", "Telephone bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_WATER", "Water bills", FamilySpending},
	{"RENT_AND_UTILITIES", "RENT_AND_UTILITIES_OTHER_UTILITIES", "Other miscellaneous utility bills", FamilySpending},
}

// vendoredIncomeCategories is Plaid's INCOME primary, whole: the first
// primary in the source CSV, its thirteen values and their descriptions
// untouched, so a refresh diffs against them as it does against the
// outflow rows.
var vendoredIncomeCategories = []SpendCategory{
	{"INCOME", "INCOME_CHILD_SUPPORT", "Child support refers to court-ordered payments made by a parent to financially support their child’s living expenses", FamilyIncome},
	{"INCOME", "INCOME_CONTRACTOR", "Income from freelance or independent contract work.", FamilyIncome},
	{"INCOME", "INCOME_DIVIDENDS", "Income from dividends", FamilyIncome},
	{"INCOME", "INCOME_GIG_ECONOMY", "Money earned by working in the gig economy, for example by driving for Lyft, Uber, etc.", FamilyIncome},
	{"INCOME", "INCOME_INTEREST_EARNED", "Income from interest on savings accounts", FamilyIncome},
	{"INCOME", "INCOME_LONG_TERM_DISABILITY", "Disability payments, for example from social security.", FamilyIncome},
	{"INCOME", "INCOME_MILITARY", "Money earned from veterans benefits. Salary earned from serving in the military (through DFAS) is categorized as salary", FamilyIncome},
	{"INCOME", "INCOME_RENTAL", "Rental income includes money earned from payments related to property rentals, lease income, and short-term rental platforms such as airbnb and VRBO.", FamilyIncome},
	{"INCOME", "INCOME_RETIREMENT_PENSION", "Payments from the social security administration, private retirement systems, (eg. 401k) pensions, and government retirement programs", FamilyIncome},
	{"INCOME", "INCOME_SALARY", "Income from salaries and wages", FamilyIncome},
	{"INCOME", "INCOME_TAX_REFUND", "Government tax refund provided to the user", FamilyIncome},
	{"INCOME", "INCOME_UNEMPLOYMENT", "Money earned from unemployment benefits", FamilyIncome},
	{"INCOME", "INCOME_OTHER", "Other miscellaneous income", FamilyIncome},
}

// deltaCategories are the eighteen own values the vendored taxonomy
// has no room for, both families in one table because nine of them are
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
// The five crossings leave BOTH bases, like `internal_transfer` and for
// the same reason: the money is still the holder's, and a crossing is
// neither a receipt nor a thing bought. Four are earmarked by a tax
// wrapper; `deposit_transfer` is earmarked by nothing and is here
// because the far leg is a bank's own deposit product the collector
// does not list as an account. `mortgage_transfer` leaves both bases
// on the same reasoning without being a crossing: it reduces the
// household's own liability, and it draws under financing rather than
// vehicles. `debt_repayment` leaves the spending base for the reason
// `investment` does — it reduces a liability rather than buying
// anything — and, like the crossings, it exists so the cashflow
// statement has an honest home for a movement the two families
// decline.
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
		"A move between the holder and a retirement plan the product does not track, either leg — a contribution wired out, a plan payout arriving. The row's own direction says which; the money is the holder's throughout, in a pool earmarked for a stage of life rather than for spending. A payout from a plan whose balance is the holder's own — a 401(k), an IRA, a pillar 3a or vested-benefits account — is this; a pension paid from a pool the holder does not own, social security among them, is INCOME_RETIREMENT_PENSION", FamilyBoth},
	{DetailedEducationTransfer, DetailedEducationTransfer,
		"The same crossing for an education plan or savings account the product does not track — money paid in, or drawn out for the costs it was set aside for", FamilyBoth},
	{DetailedHealthTransfer, DetailedHealthTransfer,
		"The same crossing for a health savings account the product does not track — a contribution paid in, or a medical cost reimbursed out of it", FamilyBoth},
	{DetailedTrustTransfer, DetailedTrustTransfer,
		"The same crossing for a trust that is a separate taxpayer and that the product does not track — a funding transfer out, a distribution arriving. A grantor trust is not this: it is tax-transparent and its accounts are the holder's own", FamilyBoth},
	{DetailedMortgageTransfer, DetailedMortgageTransfer,
		"An instalment paid to, or a tranche drawn from, a mortgage servicer the product does not hold as an account — the row's own direction says which. Drawn under financing beside a tracked mortgage rather than as a vehicle crossing; `debt_repayment` is the value for any other untracked lender. A payment to a mortgage gold does hold is an own-account move and pairs instead", FamilyBoth},
	{DetailedDepositTransfer, DetailedDepositTransfer,
		"The same crossing for a bank's own deposit product the collector does not list as an account — a call deposit, a fixed-term deposit, a notice account: money paid in, or the principal coming back. Earmarked for nothing and taxed like the funding account; it is here because the far leg does not exist in the product, not because the money went anywhere. Interest the product pays is NOT this — it is income, and it arrives on its own row", FamilyBoth},
}

// extensionSpendCategories are detailed values of OURS that sit under a
// vendored primary and that the model tier MAY emit: ordinary judgements
// about what a row is, for which the vendored vocabulary has no word yet —
// digital services under GENERAL_SERVICES, and the two costs of holding
// investments, a fee for investing under BANK_FEES and tax withheld at
// source beside TAX_PAYMENT. Each lands where a vendored value would, so a
// taxonomy that catches up supersedes it as a clean diff. docs/SPENDING.md §2
// argues each placement.
var extensionSpendCategories = []SpendCategory{
	{"GENERAL_SERVICES", SpendDetailedDigitalServices,
		"Software and online subscriptions — SaaS, cloud storage and hosting, VPNs, password managers, AI assistants; not the internet connection itself and not a physical device", FamilySpending},
	{"BANK_FEES", SpendDetailedInvestmentFees,
		"Fees for holding or managing investments — advisory and management fees, custody and platform fees, and security-level pass-throughs such as ADR depositary charges; not a fee for banking itself", FamilySpending},
	{"GOVERNMENT_AND_NON_PROFIT", SpendDetailedWithholdingTax,
		"Tax withheld at source from investment income, such as foreign dividend or non-resident withholding; deducted before the money is received rather than paid on assessment", FamilySpending},
}

// extensionIncomeCategories are the income side's extensions, under
// the one vendored primary it has. Each names income the vendored
// vocabulary has no value for. Plaid files most of it under its
// catch-all, INCOME_OTHER, so without the extension a household reading
// its own report would see what it receives filed as "other"; the rest
// it files under a neighbour that means something else, such as a fund's
// distribution under DIVIDENDS.
//
// The bar each clears is the bar an extension always clears — common,
// distinct on a statement or a tax return, and absent from the
// vendored vocabulary — and it is applied to households in general
// rather than to one, because the product is published. An unused
// value costs a little model precision; a missing one costs everyone
// who needed it a config rule.
//
// Left out deliberately, each because an existing value already says
// it: a bonus or a severance payment (salary), crypto lending interest
// (interest earned), a scholarship or a lottery win (other income),
// and mining, which a config rule places at contractor or other income
// until it earns a value of its own.
//
// GOVERNMENT_BENEFITS is the state transfers the vendored vocabulary
// does not name: it names a pension, unemployment, disability and
// veterans' benefits, out of the many a state makes. ALIMONY is the
// half of family maintenance the vendored CHILD_SUPPORT leaves out. It
// is person-shaped, so the fence keeps most of it from the model and a
// rule or a pin places it; it is an extension all the same, because
// what it names is a kind of income rather than a structural fact
// about an account.
//
// STAKING and DISTRIBUTIONS carry a policy each. Staking is kept apart
// from interest for the reason gold keeps the `staking` kind apart
// from `interest`: jurisdictions tax the two differently.
// DISTRIBUTIONS is a fund's payout of realised gains on a holding
// still held — it returns no basis, so it is income, and it is kept
// apart from DIVIDENDS so that the vendored value keeps Plaid's
// meaning.
var extensionIncomeCategories = []SpendCategory{
	{"INCOME", IncomeDetailedGovernmentBenefits,
		"State transfers no other value names — child and family allowances, parental-leave pay, housing benefits, social assistance, stimulus payments; not a state pension (INCOME_RETIREMENT_PENSION), an unemployment benefit (INCOME_UNEMPLOYMENT), a disability benefit (INCOME_LONG_TERM_DISABILITY), a veterans benefit (INCOME_MILITARY) or child support an agency pays out or advances (INCOME_CHILD_SUPPORT), and not a tax refund, which is INCOME_TAX_REFUND whichever tax office pays it and in whatever language", FamilyIncome},
	{"INCOME", IncomeDetailedRoyalties,
		"Royalties and creator payouts — book, music, software-licence and patent royalties, and a platform's share of what a creator's work earned", FamilyIncome},
	{"INCOME", IncomeDetailedEnergyFeedIn,
		"What a grid operator or an energy retailer pays for electricity the household's own generation fed into the grid — a solar feed-in tariff or a net-metering credit paid out; income from an asset the household owns, not a refund of a utility bill, which is a reimbursement", FamilyIncome},
	{"INCOME", IncomeDetailedAlimony,
		"Maintenance received from a former spouse or partner — alimony, spousal or separation maintenance; not child support, which is INCOME_CHILD_SUPPORT whether a court ordered it or the parents agreed it, and not a cash gift or family support given freely", FamilyIncome},
	{"INCOME", IncomeDetailedInsurancePayout,
		"What an insurer pays out on a policy — a claim settled, a damage or health cost covered, a premium refunded on cancellation; not a recurring disability benefit, which is INCOME_LONG_TERM_DISABILITY whoever pays it. Income rather than a reimbursement because the premium that bought the cover was already counted as spending, and nothing links a payout back to the premiums it answers: netting the payout out would count the outflow and drop the inflow", FamilyIncome},
	{"INCOME", IncomeDetailedStaking,
		"Proof-of-stake rewards and validator income earned by committing a crypto holding; kept apart from interest because jurisdictions tax the two differently", FamilyIncome},
	{"INCOME", IncomeDetailedRewards,
		"Card cashback, statement credits, account-opening and referral bonuses, and airdrops — earned on spending or on holding an account, and never netted against the spending itself", FamilyIncome},
	{"INCOME", IncomeDetailedDistributions,
		"A fund's cash payout of realised gains on a holding still held, returning no basis; not a company's dividend, and not a private fund returning contributed capital", FamilyIncome},
}

// SpendCategories is the whole taxonomy, both families — the vendored
// pairs, then the extensions, then the deltas. Ordered for readable
// diffs; the seed migrations were generated from it and
// TestSpendCategoriesMatchGoTable compares the two as sets, so the order
// carries no contract.
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
// ours, or one of the thirteen deltas the spending side reads. A primary
// on its own is not valid unless it is also a delta, and an income value
// is not valid here: the two families are separate vocabularies that
// happen to share a table, so a spending rule or pin naming
// `INCOME_SALARY` is as wrong as one naming nothing at all.
func ValidSpendDetailed(s string) bool {
	_, ok := spendDetailedValues[s]
	return ok
}

// ValidIncomeDetailed is the same for income_detailed: the thirteen
// vendored INCOME values, the eight extensions, and the fourteen deltas
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
// the fourteen income deltas for the same reason. `capital_return` is the
// one that would hurt most: it is what a private fund's distribution
// floors to, it is OUT of the income base, and a payer-keyed verdict
// carrying it would silently remove that payer from every income
// report rather than show a figure that is wrong.
func ModelIncomeDetailed(s string) bool {
	_, ok := modelIncomeDetailedValues[s]
	return ok
}

// Retired is a detailed value an earlier taxonomy held and this one does
// not.
type Retired struct {
	// Successor is the value stored verdicts moved to (migration 0111).
	Successor string
	// Use is what a refusal advises in its place: the successor, or each
	// successor with the case it covers when the old value was split.
	Use string
}

// retiredDetailed are the spellings the move to Plaid's version 2
// retired: two vendored values Plaid renamed, and three extensions a
// version 2 value took over. Closed: a later retirement is a new entry
// beside a new migration.
var retiredDetailed = map[string]Retired{
	"INCOME_WAGES":           {"INCOME_SALARY", "INCOME_SALARY, or INCOME_GIG_ECONOMY for gig-platform pay"},
	"INCOME_OTHER_INCOME":    {"INCOME_OTHER", "INCOME_OTHER"},
	"INCOME_SELF_EMPLOYMENT": {"INCOME_CONTRACTOR", "INCOME_CONTRACTOR"},
	"INCOME_RENT":            {"INCOME_RENTAL", "INCOME_RENTAL"},
	"INCOME_ALIMONY_AND_CHILD_SUPPORT": {"INCOME_CHILD_SUPPORT",
		"INCOME_CHILD_SUPPORT for child support, or INCOME_ALIMONY for maintenance from a former partner"},
}

// modelNotes are wealthdb's own line for a few vendored values, which
// the model prompt prints after the value's description. A vendored
// description stays Plaid's text, word for word, so a refresh still
// diffs cleanly; where wealthdb draws the line somewhere that text does
// not say, the note tells the model. A note says what a value also
// covers, or what to skip — never to emit a value outside the model's
// vocabulary.
var modelNotes = map[string]string{
	"INCOME_CONTRACTOR": "Also a sole trader's takings and an owner's draw from their own company.",
	"INCOME_RENTAL":     "Not a returned tenancy deposit and not the proceeds of selling the property: skip those.",
	"INCOME_RETIREMENT_PENSION": "Only a pension paid from a pool the holder does not own, such as social security or a defined-benefit plan. " +
		"A payout from a plan whose balance is the holder's own — a 401(k), an IRA, a pillar 3a or vested-benefits account — is not income: skip it.",
}

// ModelNote is wealthdb's note on a value for the model prompt, or ""
// for a value whose description says all of it.
func ModelNote(detailed string) string { return modelNotes[detailed] }

// RetiredDetailed reports whether s is a retired spelling, and what
// replaced it. Validity stays derived from the table, so a retired
// spelling is refused like any other value outside it; this only lets
// the refusal name what to write instead.
func RetiredDetailed(s string) (Retired, bool) {
	r, ok := retiredDetailed[s]
	return r, ok
}

// spendLabelAcronyms are the words the mechanical rule would sentence-case
// wrongly. Two, and both are initialisms the vendored taxonomy spells in
// full caps because every value is in full caps.
var spendLabelAcronyms = map[string]string{"Atm": "ATM", "Tv": "TV"}

// spendLabelOverrides are the values whose display name is not what the
// mechanical rule reads off them:
//
//   - `card_spend`, which the rule renders "Card spend" — true of every
//     card purchase in the product, so among the merchant categories on a
//     chart it reads as a KIND of spending rather than as the placeholder
//     it is. The value stands for a bill on a card wealthdb does not
//     itemise (migration 0046): real consumption whose purchases nobody
//     has seen, in the base and replaced by those purchases the day the
//     card is collected. "Uncategorized card spend" says both halves.
//   - INCOME_OTHER, which the rule renders "Other" — the label of the
//     `other` delta, a different thing: a catch-all declines a row, while
//     `other` is a verdict that takes it out of the backlog.
//   - INCOME_MILITARY, whose value names veterans' benefits; "Military"
//     reads as military pay, which its own description files as salary.
//   - BANK_FEES_CASH_ADVANCE, which "Cash advance" names as the advance
//     itself — borrowed money, not a fee.
//   - two hyphenated compounds the rule cannot spell.
var spendLabelOverrides = map[string]string{
	SpendDetailedCardSpend:        "Uncategorized card spend",
	"INCOME_OTHER":                "Other income",
	"INCOME_MILITARY":             "Veterans benefits",
	"BANK_FEES_CASH_ADVANCE":      "Cash advance fees",
	"INCOME_LONG_TERM_DISABILITY": "Long-term disability",
	IncomeDetailedEnergyFeedIn:    "Energy feed-in",
}

// SpendLabel is a detailed value's display name: the vendored value with
// its primary's prefix taken off, underscores opened out and one capital
// at the front. `FOOD_AND_DRINK_GROCERIES` reads "Groceries";
// `GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE` reads "Other general
// merchandise"; `INCOME_SALARY` reads "Salary"; a delta, which is its own
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
// (0058 seeded them all, and every row added since carries its own), so
// a label can be corrected by hand without
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
// nothing finer". Every one spells its detail part `OTHER` or
// `OTHER_...`, which is the vendored taxonomy's own convention and the
// reason this can be read off the value rather than listed.
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
// side changed no answer this gave before it existed: `INCOME_OTHER` is
// income's catch-all and not spending's.
func CatchAllSpendDetailed(s string) bool {
	return ValidSpendDetailed(s) && catchAllSpelling(s)
}

// CatchAllIncomeDetailed is the same predicate over the income
// vocabulary, where there is exactly one: `INCOME_OTHER`.
func CatchAllIncomeDetailed(s string) bool {
	return ValidIncomeDetailed(s) && catchAllSpelling(s)
}

func catchAllSpelling(s string) bool {
	primary, ok := primaryOf[s]
	if !ok || primary == s {
		return false // unknown, or a delta, which is primary-level
	}
	rest := s[len(primary)+1:]
	return rest == "OTHER" || strings.HasPrefix(rest, "OTHER_")
}

// DeltaSpendCategories returns the spending family's delta rows — the
// values the model tier must never emit, with the descriptions that say
// why each exists. A copy, for the same reason VendoredSpendCategories
// hands one out.
func DeltaSpendCategories() []SpendCategory {
	return categoriesIn(FamilySpending, deltaCategories)
}

// DeltaIncomeCategories is the same for the income family. The shared
// deltas appear in both lists: one row, read from either side.
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
