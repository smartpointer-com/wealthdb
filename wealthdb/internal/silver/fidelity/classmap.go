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
	case "bond":
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case "money_market":
		return canonical.AssetClassCash, canonical.VehicleFund
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// The instrument-key and description shapes classifyHistoricalPair
// matches. The key shapes mirror fidelity-web's live silver
// classifier (`load.py _classify_asset_class`); the description
// shapes cover what the supplied-statement and SVB statement-PDF
// families surface instead of a structured type
// code.
var (
	// OCC option symbol: root + YYMMDD + C/P + strike.
	histOptionKeyRe = regexp.MustCompile(`^[A-Z.]{1,6}\d{6}[CP]\d+(\.\d+)?$`)
	// Option legs print as "CALL (ABCD) …" / "PUT (…) …".
	histOptionDescRe = regexp.MustCompile(`^(CALL|PUT)\b`)
	histCUSIPRe      = regexp.MustCompile(`^[A-Z0-9]{8}[0-9]$`)
	histPlanKeyRe    = regexp.MustCompile(`^[A-Z]{3}[0-9]{6}$`)
	// Keyless 529 plan sleeves: "STATE PLAN 2099 (FIDELITY BLEND)".
	histPlanDescRe = regexp.MustCompile(`\(FIDELITY [^)]*\)$`)
	// US money-market funds carry 5-letter tickers ending in a
	// doubled X — the convention separating them from ordinary
	// mutual funds' single trailing X. Catches money funds whose
	// truncated statement description says neither "MONEY MARKET"
	// nor "CASH RESERVES".
	histMoneyMktKeyRe = regexp.MustCompile(`^[A-Z]{3}XX$`)
	histMutualFundRe  = regexp.MustCompile(`^[A-Z]{4}X$`)
	// Bond rows carry a coupon: "… 04.12500% 01/15/2042" / "FIXED COUPON".
	histBondDescRe = regexp.MustCompile(`(?i)\b\d{1,2}\.\d{3,5}%|FIXED COUPON`)
	// Money-market sweeps ("FIDELITY GOVERNMENT MONEY MARKET",
	// "… CASH RESERVES") plus the svb builder's "NET CASH POSITION"
	// row — the statement's net cash/margin sleeve booked as a
	// position (negative = margin debit).
	histMoneyMktRe = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b|^NET CASH POSITION$`)
	histETFDescRe  = regexp.MustCompile(`\bETF\b`)
	// ETF-only issuer families whose statement descriptions often
	// omit the "ETF" token — truncated lines like "ISHARES TRUST DJ
	// US EXAMPLE" or "VANGUARD INTL EQUITY INDEX FDS EXAMPLE".
	// Checked AFTER the mutual-fund ticker shape so an issuer's
	// ordinary mutual funds (5-letter X-tickers) keep the fund
	// vehicle.
	histETFIssuerRe = regexp.MustCompile(`(?i)^(ISHARES|SPDR|VANGUARD|XTRACKERS|PROSHARES|WISDOMTREE)\b`)
	// The svb builder's synthetic $0 closure marker — value 0, no
	// exposure to classify.
	histClosedDescRe = regexp.MustCompile(`^Account closed`)
)

// classifyHistoricalPair derives the (exposure, vehicle) pair of a
// `historical_position_snapshots` row from its instrument key and
// description — the statement PDFs behind these rows carry no
// structured type code, so shape heuristics are all there is. First
// match wins; the config's instrument_overrides remain the escape
// hatch for rows the shapes misjudge.
//
//   - Account-closed marker → (other, other): a $0 synthetic row with no
//     exposure to classify.
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
	case histClosedDescRe.MatchString(description):
		return canonical.AssetClassOther, canonical.VehicleOther
	case histOptionKeyRe.MatchString(instrumentKey),
		histOptionDescRe.MatchString(description):
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case histMoneyMktRe.MatchString(description),
		histMoneyMktKeyRe.MatchString(instrumentKey):
		return canonical.AssetClassCash, canonical.VehicleFund
	case histPlanKeyRe.MatchString(instrumentKey),
		histPlanDescRe.MatchString(description):
		return canonical.AssetClassMultiAsset, canonical.VehicleFund
	case histCUSIPRe.MatchString(instrumentKey),
		histBondDescRe.MatchString(description):
		return canonical.AssetClassFixedIncome, canonical.VehicleBond
	case histETFDescRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	case histMutualFundRe.MatchString(instrumentKey):
		// A money fund whose name lacks the "MONEY MARKET" token
		// histMoneyMktRe caught above (e.g. "… MONEY FUND") but still
		// reads as cash.
		if silver.NamesMoneyMarket(description) {
			return canonical.AssetClassCash, canonical.VehicleFund
		}
		return silver.RefineETFExposure(description), canonical.VehicleFund
	case histETFIssuerRe.MatchString(description):
		return silver.RefineETFExposure(description), canonical.VehicleETF
	}
	return canonical.AssetClassPublicEquity, canonical.VehicleStock
}
