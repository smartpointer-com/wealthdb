package fidelity

import "github.com/ptu/wealthdb/internal/canonical"

// assetClassFor maps fidelity-web-dump's `positions.asset_class`
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
