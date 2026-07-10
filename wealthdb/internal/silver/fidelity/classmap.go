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
