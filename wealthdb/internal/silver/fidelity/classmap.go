package fidelity

import (
	"regexp"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// assetClassFor maps fidelity-web's `positions.asset_class`
// (the silver-side classification: 'equity' / 'etf' / 'mutual_fund'
// / 'bond' / 'plan_fund' / 'money_market' / ...) to the canonical
// AssetClass. Unknown / empty values fall through to AssetClassOther.
//
// `money_market` rows never reach this helper — they're filtered
// out earlier in appendPositionsAndCash and emitted as
// CashBalanceChange instead.
func assetClassFor(silverClass string) canonical.AssetClass {
	switch silverClass {
	case "equity":
		return canonical.AssetClassEquity
	case "etf":
		return canonical.AssetClassETF
	case "mutual_fund", "plan_fund":
		// plan_fund is a 529 investment-option code — Fidelity-
		// administered fund wrapper around an underlying allocation.
		// Same canonical bucket as a regular mutual fund.
		return canonical.AssetClassFund
	case "bond":
		return canonical.AssetClassBond
	case "money_market":
		return canonical.AssetClassMoneyMarket
	}
	return canonical.AssetClassOther
}

// assetClassVehicleFor is the 2-D-taxonomy companion to assetClassFor:
// it maps fidelity-web's silver `positions.asset_class` onto a canonical
// V2 exposure (AssetClassNew) plus the wrapper Vehicle the exposure is
// held through. It runs beside the legacy assetClassFor (the control) so
// both dimensions are double-written without disturbing the V1 column.
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

// The instrument-key and description shapes classifyHistorical
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
	histPlanDescRe   = regexp.MustCompile(`\(FIDELITY [^)]*\)$`)
	histMutualFundRe = regexp.MustCompile(`^[A-Z]{4}X$`)
	// Bond rows carry a coupon: "… 04.12500% 01/15/2042" / "FIXED COUPON".
	histBondDescRe = regexp.MustCompile(`(?i)\b\d{1,2}\.\d{3,5}%|FIXED COUPON`)
	// Money-market sweeps ("FIDELITY GOVERNMENT MONEY MARKET",
	// "… CASH RESERVES") plus the svb builder's "NET CASH POSITION"
	// row — the statement's net cash/margin sleeve booked as a
	// position (negative = margin debit).
	histMoneyMktRe = regexp.MustCompile(`(?i)\bMONEY MARKET\b|\bCASH RESERVES\b|^NET CASH POSITION$`)
	histETFDescRe  = regexp.MustCompile(`\bETF\b`)
	// The svb builder's synthetic $0 closure marker — value 0, no
	// exposure to classify.
	histClosedDescRe = regexp.MustCompile(`^Account closed`)
)

// classifyHistorical derives the asset class of a
// `historical_position_snapshots` row from its instrument key and
// description — the statement PDFs behind these rows carry no
// structured type code, so shape heuristics are all there is.
// First match wins; the fall-through is equity, not other,
// because an unrecognised line is a plain stock/ADR row — the
// statements' overwhelming majority. The config's
// instrument_overrides remain the escape hatch for rows the
// shapes misjudge.
func classifyHistorical(instrumentKey, description string) canonical.AssetClass {
	switch {
	case histClosedDescRe.MatchString(description):
		return canonical.AssetClassOther
	case histOptionKeyRe.MatchString(instrumentKey),
		histOptionDescRe.MatchString(description):
		return canonical.AssetClassOption
	case histMoneyMktRe.MatchString(description):
		return canonical.AssetClassMoneyMarket
	case histPlanKeyRe.MatchString(instrumentKey),
		histPlanDescRe.MatchString(description):
		return canonical.AssetClassFund
	case histCUSIPRe.MatchString(instrumentKey),
		histBondDescRe.MatchString(description):
		return canonical.AssetClassBond
	case histETFDescRe.MatchString(description):
		return silver.RefineETFClass(description)
	case histMutualFundRe.MatchString(instrumentKey):
		return canonical.AssetClassFund
	}
	return canonical.AssetClassEquity
}

// classifyHistoricalPair is the 2-D-taxonomy companion to
// classifyHistorical: it mirrors the exact same shape branches (same
// order, first match wins) but yields a canonical V2 exposure plus the
// wrapper Vehicle for the double-write. classifyHistorical stays the
// control; this runs beside it at the historical instrument/position
// build.
//
//   - Account-closed marker → (other, other): a $0 synthetic row with no
//     exposure to classify.
//   - option → (public_equity, option): equity-underlying option legs.
//   - money-market sweep / net-cash sleeve → (cash, fund).
//   - 529 plan sleeve → (multi_asset, fund): a blended allocation wrapper.
//   - bond (CUSIP-9 or coupon in the description) → (fixed_income, bond).
//   - ETF-by-name → (RefineETFExposure(desc), etf).
//   - mutual-fund ticker → (RefineETFExposure(desc), fund).
//   - fall-through → (public_equity, stock): plain stock/ADR rows.
//
// Every pair returned satisfies canonical.ValidTaxonomyPair.
func classifyHistoricalPair(instrumentKey, description string) (canonical.AssetClass, canonical.Vehicle) {
	switch {
	case histClosedDescRe.MatchString(description):
		return canonical.AssetClassOther, canonical.VehicleOther
	case histOptionKeyRe.MatchString(instrumentKey),
		histOptionDescRe.MatchString(description):
		return canonical.AssetClassPublicEquity, canonical.VehicleOption
	case histMoneyMktRe.MatchString(description):
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
	}
	return canonical.AssetClassPublicEquity, canonical.VehicleStock
}
