package silver

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The security-name patterns RefineETFExposure matches, uppercase-
// insensitive. Word boundaries matter throughout: GOLD must not
// match GOLDMAN, ETHER must not match WEATHERFORD, and the miners
// guard keeps equity ETFs of mining companies (gold-miners funds
// hold stocks, not bullion) out of the metal bucket.
var (
	etfCryptoRe = regexp.MustCompile(`(?i)BITCOIN|ETHEREUM|\bETHER\b|SOLANA|CRYPTO|\bXRP\b`)
	etfMetalRe  = regexp.MustCompile(`(?i)\bGOLD\b|\bSILVER\b|PLATINUM|PALLADIUM|PRECIOUS METALS?\b`)
	etfMinersRe = regexp.MustCompile(`(?i)\bMINERS?\b|\bMINING\b`)
	etfBondRe   = regexp.MustCompile(`(?i)\bBONDS?\b|TREASURY|FIXED INCOME|MUNICIPAL|\bMUNI\b|\bTIPS\b`)
)

// NamesPhysicalMetal reports whether a security name reads as a
// physical precious-metal holding — bullion, not the stocks of
// companies that dig it up (the miners guard). Shared by the ETF
// refinement above and by adapters whose source vocabulary is too
// coarse for the distinction (e.g. an "Alternatives" sleeve that
// is actually a physical-gold index fund).
func NamesPhysicalMetal(name string) bool {
	return etfMetalRe.MatchString(name) && !etfMinersRe.MatchString(name)
}

// moneyMarketRe matches money-market-fund names. A purchased money
// fund (not the account's core sweep) reaches a fund/mutual-fund
// classification path, where its exposure must be read as cash, not
// as whatever RefineETFExposure guesses from a stray bond keyword
// like TREASURY in the fund name. Word boundaries keep it off e.g.
// "Money Center Bank".
var moneyMarketRe = regexp.MustCompile(`(?i)\bMONEY\s+MARKET\b|\bMONEY\s+FUND\b|\bCASH\s+RESERVES\b|\bMM(?:KT|F)\b|\bMONEY\s+MKT\b`)

// NamesMoneyMarket reports whether a fund's security name reads as a
// money-market fund — a cash equivalent (TAXONOMY.md: money-market
// funds are cash, not fixed income). Callers use it to route a
// fund/mutual-fund holding to (cash, fund) before name-based exposure
// refinement, which has no money-market awareness.
func NamesMoneyMarket(name string) bool {
	return moneyMarketRe.MatchString(name)
}

// RefineETFExposure returns the EXPOSURE (asset_class) of a fund/ETF
// already known to be a collective vehicle, from its security name:
// crypto → crypto, bullion → metal, bond keywords → fixed_income,
// everything else → public_equity (the default for a name that
// doesn't reveal a non-equity underlying). The wrapper (vehicle=etf
// or fund) is the caller's — this decides only what the wrapper
// holds. Callers apply it only AFTER the source's structured signal
// (CFI group, Schwab instrument.type, statement section, silver
// classifier) said "collective vehicle" — the name alone must never
// promote a non-fund into these buckets (a gold-miner *stock* is
// equity, not metal). Name-shy products are corrected by
// instrument_overrides.
func RefineETFExposure(name string) canonical.AssetClass {
	switch {
	case etfCryptoRe.MatchString(name):
		return canonical.AssetClassCrypto
	case NamesPhysicalMetal(name):
		return canonical.AssetClassMetal
	case etfBondRe.MatchString(name):
		return canonical.AssetClassFixedIncome
	}
	return canonical.AssetClassPublicEquity
}
