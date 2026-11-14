package spending

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// plaidPFCv2 is every detailed value of Plaid's personal finance category
// taxonomy, version 2, verbatim and in Plaid's order. The collector asks
// Plaid for version 2 on every read, so this is the whole vocabulary a
// plaid row can carry. It is the reviewed list. Each side translates a
// value or leaves it untranslated. Only a value outside the list is drift,
// which is what a revision of the taxonomy looks like.
//
// The taxonomy vendors thirteen of Plaid's eighteen primaries whole, so
// most of the list translates to itself. It leaves out the other five.
// The two transfer primaries and the two loan primaries name movements,
// and the matcher and the deltas place those. OTHER is Plaid's bucket for a
// row it could not place, so OTHER_OTHER stays untranslated and the
// model reads the counterparty.
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

// plaidCategories is the spending side. Every vendored spend value
// translates to itself. The other translations are loan payments, which
// the taxonomy leaves to its deltas. Each goes to the delta for the kind
// of lender its value names:
//
//   - a card bill is `card_spend`, and the matcher outranks it when the
//     card's own leg is in gold;
//   - a mortgage instalment is `mortgage_transfer`;
//   - a student, personal, cash-advance or car payment is
//     `debt_repayment`.
//
// A car payment can be a lease, which is consumption, and
// `debt_repayment` takes it out of the base. Plaid files loans and leases
// under one value. A config rule on the lessor's name, scoped to the
// paying account, puts a lease back. The other loan payments stay
// untranslated, for the reasons plaidUncategorized gives.
var plaidCategories = plaidTranslations(canonical.ValidSpendDetailed, map[string]string{
	"LOAN_PAYMENTS_CREDIT_CARD_PAYMENT":   canonical.SpendDetailedCardSpend,
	"LOAN_PAYMENTS_MORTGAGE_PAYMENT":      canonical.DetailedMortgageTransfer,
	"LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT":  canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT": canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_CASH_ADVANCES":         canonical.SpendDetailedDebtRepayment,
	"LOAN_PAYMENTS_CAR_PAYMENT":           canonical.SpendDetailedDebtRepayment,
})

// plaidIncomeCategories is the income side. Every vendored income value
// translates to itself. INCOME_OTHER is one of them, and it is the
// income catch-all: the row is recorded and declined, so the model reads
// the payer. The other translations are loan disbursements: money
// borrowed arriving is `loan_proceeds`, and a mortgage tranche is
// `mortgage_transfer`.
//
// INCOME_RETIREMENT_PENSION's version 2 text names payouts from plans
// such as a 401(k). So a payout from a plan gold does not track lands
// there. A config rule on the plan's name places it as
// `retirement_transfer`, the value for a payout from the holder's own
// plan.
var plaidIncomeCategories = plaidTranslations(canonical.ValidIncomeDetailed, map[string]string{
	"LOAN_DISBURSEMENTS_AUTO":               canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_CASH_ADVANCES":      canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_PERSONAL":           canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_STUDENT":            canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT": canonical.IncomeDetailedLoanProceeds,
	"LOAN_DISBURSEMENTS_MORTGAGE":           canonical.DetailedMortgageTransfer,
})

// plaidUncategorized and plaidIncomeUncategorized are each side's reviewed
// and untranslated values: the rest of plaidPFCv2.
//
//   - A transfer's category does not say whose account the far side is.
//     That is the matcher's to find and a rule's to state.
//   - A buy-now-pay-later instalment is the only trace of a purchase.
//   - LOAN_PAYMENTS_OTHER_PAYMENT can be a card bill or a loan payment.
//   - An early wage advance (LOAN_DISBURSEMENTS_EWA) is usually wages paid
//     early and taken back from the next pay, and sometimes a loan. It
//     stays in the income base as a visible receipt, and its repayment
//     (LOAN_PAYMENTS_EWA) in the spending base, so the two legs offset.
//   - OTHER_OTHER stays here, never `other`, which is a verdict and would
//     claim the row.
//   - Each side's values for the other direction arrive on refunds and
//     reversals.
var (
	plaidUncategorized       = plaidRest(plaidCategories)
	plaidIncomeUncategorized = plaidRest(plaidIncomeCategories)
)

// plaidTranslations is every plaidPFCv2 value that `valid` admits, to
// itself, plus `deltas`: values the taxonomy leaves out, each to the
// delta that names its movement.
func plaidTranslations(valid func(string) bool, deltas map[string]string) map[string]string {
	out := map[string]string{}
	for _, v := range plaidPFCv2 {
		if valid(v) {
			out[v] = v
		}
	}
	for k, v := range deltas {
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
