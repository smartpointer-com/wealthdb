package fidelity

import (
	"regexp"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// assetClassVehicleFor maps fidelity-web's silver
// `positions.asset_class` (the silver-side classification: 'equity' /
// 'etf' / 'mutual_fund' / 'bond' / 'plan_fund' / 'money_market' / …)
// onto the canonical exposure (AssetClass) plus the wrapper Vehicle
// the exposure is held through — the single (exposure, vehicle) pair
// emitted to gold. Unknown / empty silver classes fall through to
// (other, other).
//
// Funds/ETFs whose exposure depends on their holdings defer to
// silver.RefineETFExposure(name), which reads the security name
// (crypto / metal / fixed_income / public_equity); the vehicle (etf vs
// fund) is fixed by the silver class, not the name. A money-market
// fund is the exception: only the account's CORE sweep is labeled
// silver `money_market` (routed to CashBalanceChange upstream), so a
// separately-purchased money fund arrives here as `mutual_fund` — its
// name is checked for a money-market signal first, since it is a cash
// equivalent (cash × fund), not the exposure RefineETFExposure would
// guess from a stray bond keyword. Every pair returned satisfies
// canonical.ValidTaxonomyPair.
func assetClassVehicleFor(silverClass, name string) (canonical.AssetClass, canonical.Vehicle) {
	switch silverClass {
	case "equity":
		return canonical.AssetClassPublicEquity, canonical.VehicleStock
	case "etf":
		return silver.RefineETFExposure(name), canonical.VehicleETF
	case "mutual_fund":
		if silver.NamesMoneyMarket(name) {
			return canonical.AssetClassCash, canonical.VehicleFund
		}
		return silver.RefineETFExposure(name), canonical.VehicleFund
	case "plan_fund":
		// 529 investment-option wrapper: a blended allocation, so
		// multi_asset exposure held through a fund vehicle.
		return canonical.AssetClassMultiAsset, canonical.VehicleFund
	case "daf_pool":
		// Donor-Advised Fund investment pool (fidelity-web
		// DESIGN.md §12): the sponsor's pooled model portfolio —
		// same shape as the 529 plan fund, a blended allocation
		// held through a fund vehicle.
		return canonical.AssetClassMultiAsset, canonical.VehicleFund
	case "bond":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "money_market":
		return canonical.AssetClassCash, canonical.VehicleFund
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// Instrument-key and description shapes classifyHistoricalPair
// matches that are specific to this adapter's own sources. The
// shared, cross-adapter shapes (option / CUSIP / bond-coupon /
// money-market ticker / ETF / mutual-fund / ETF-issuer) live in
// silver.Stmt*Re; only the shapes unique to fidelity's supplied
// statements, the SVB statement families (Wealth Advisory / NFS
// brokerage, deposit, mortgage) and the SVB advisor workbook's
// month-end marks are declared here.
var (
	histPlanKeyRe = regexp.MustCompile(`^[A-Z]{3}[0-9]{6}$`)
	// Keyless 529 plan sleeves: "STATE PLAN 2099 (FIDELITY BLEND)".
	histPlanDescRe = regexp.MustCompile(`\(FIDELITY [^)]*\)$`)
	// Money-market sweeps ("FIDELITY GOVERNMENT MONEY MARKET",
	// "… CASH RESERVES").
	histMoneyMktRe = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b`)
	// The svb builder's net cash/margin sleeve, booked as a position
	// (negative = margin debit).
	histNetCashRe = regexp.MustCompile(`^NET CASH POSITION$`)
	// The row an svb statement that prints no positions and states a
	// $0 portfolio total materialises as. Value 0, so there is no
	// exposure to classify — and it is a real observation of an empty
	// month, not a closure: the same account can hold a residual the
	// month after.
	histNoPositionsRe = regexp.MustCompile(`^NO POSITIONS$`)
	// The two rows the svb builder materialises from its OCRed deposit
	// and mortgage statements: a deposit account's balance, and a home
	// loan's outstanding principal (carried negative). Neither has an
	// instrument key to go on, and neither shape resembles a security,
	// so both would otherwise take the public-equity fall-through
	// below — which for a liability is wrong twice over.
	histCashRe     = regexp.MustCompile(`^CASH BALANCE$`)
	histMortgageRe = regexp.MustCompile(`^MORTGAGE PRINCIPAL$`)
	// A month the svb statement archive misses, valued from the
	// advisor's workbook instead. It states the account's worth and
	// nothing about its composition, so there is no exposure to read
	// off it — unlike a statement row, which names a security.
	histAdvisorMarkRe = regexp.MustCompile(`^ACCOUNT VALUE \(ADVISOR MARK\)$`)
)

// classifyHistoricalPair derives the (exposure, vehicle) pair of a
// `historical_position_snapshots` row from its instrument key and
// description — the statement PDFs behind these rows carry no
// structured type code, so shape heuristics are all there is. First
// match wins; the config's instrument_overrides remain the escape
// hatch for rows the shapes misjudge.
//
//   - No-positions row → (other, other): a $0 row with no exposure to
//     classify.
//   - advisor month-end mark → (other, other): a whole-account value, with no
//     composition stated to classify.
//   - deposit-account balance → (cash, demand_deposit).
//   - mortgage principal → (real_estate, mortgage): the liability against
//     the property, which is how the manual adapter carries one too.
//   - option → (public_equity, option): equity-underlying option legs.
//   - money-market sweep / net-cash sleeve / XX-ticker money fund → (cash, fund).
//   - 529 plan sleeve → (multi_asset, fund): a blended allocation wrapper.
//   - bond (CUSIP-9 or coupon in the description) → (fixed_income, bond).
//   - ETF-by-name or ETF-only-issuer name → (RefineETFExposure(desc), etf).
//   - mutual-fund ticker → (RefineETFExposure(desc), fund).
//   - fall-through → (public_equity, stock): plain stock/ADR rows, the
//     statements' overwhelming majority.
//
// Every pair returned satisfies canonical.ValidTaxonomyPair.
func classifyHistoricalPair(instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	switch {
	case histNoPositionsRe.MatchString(description):
		return canonical.AssetClassOther, canonical.VehicleOther
	case histAdvisorMarkRe.MatchString(description):
		return canonical.AssetClassOther, canonical.VehicleOther
	case histCashRe.MatchString(description):
		return canonical.AssetClassCash, canonical.VehicleDemandDeposit
	case histMortgageRe.MatchString(description):
		return canonical.AssetClassRealEstate, canonical.VehicleMortgage
	case silver.StmtOptionKeyRe.MatchString(instrumentKey),
		silver.StmtOptionDescRe.MatchString(description):
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case histMoneyMktRe.MatchString(description),
		histNetCashRe.MatchString(description),
		silver.StmtMoneyMktKeyRe.MatchString(instrumentKey):
		return canonical.AssetClassCash, canonical.VehicleFund
	case histPlanKeyRe.MatchString(instrumentKey),
		histPlanDescRe.MatchString(description):
		return canonical.AssetClassMultiAsset, canonical.VehicleFund
	case silver.StmtCUSIPRe.MatchString(instrumentKey),
		silver.StmtBondDescRe.MatchString(description):
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case silver.StmtETFDescRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	case silver.StmtMutualFundRe.MatchString(instrumentKey):
		// A money fund whose name lacks the "MONEY MARKET" token
		// histMoneyMktRe caught above (e.g. "… MONEY FUND") but still
		// reads as cash.
		if silver.NamesMoneyMarket(description) {
			return canonical.AssetClassCash, canonical.VehicleFund
		}
		return silver.RefineETFExposure(description), canonical.VehicleFund
	case silver.StmtETFIssuerRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	}
	return canonical.AssetClassPublicEquity, canonical.VehicleStock
}

// isStatementConstruct reports whether a keyless historical row is one
// a statement builder materialises from a statement-level figure — the
// net cash sleeve, a stated-$0 month, an advisor mark, a deposit
// balance, a loan's principal — rather than from a security line. It
// has no symbol: its synthetic key is an identity the adapter made up,
// and showing it as one would read as a ticker.
func isStatementConstruct(description string) bool {
	for _, re := range []*regexp.Regexp{histNetCashRe, histNoPositionsRe,
		histAdvisorMarkRe, histCashRe, histMortgageRe} {
		if re.MatchString(description) {
			return true
		}
	}
	return false
}
