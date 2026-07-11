package silver

import (
	"regexp"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The security-name patterns RefineETFClass matches, uppercase-
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

// RefineETFClass narrows an instrument already identified as an
// exchange-traded fund/product to the asset class of what it
// holds, from its security name. The canonical taxonomy classes
// ETFs by underlying exposure, not by the wrapper: a spot-bitcoin
// ETF is `crypto` alongside directly-held coin, a bullion ETF is
// `metal` alongside vault gold, and a bond ETF is `bond_etf`
// (deliberately not `bond` — see the canonical.AssetClassBondETF
// comment). Everything else — equity and anything the name doesn't
// give away — stays `etf`; the config's instrument_overrides is
// the escape hatch for name-shy products.
//
// Callers apply this only AFTER the source's structured signal
// (CFI group, Schwab instrument.type, statement section, silver
// classifier) said "ETF" — the name alone must never promote a
// non-ETF into these buckets (a gold-miner *stock* is equity, not
// metal).
func RefineETFClass(name string) canonical.AssetClass {
	switch {
	case etfCryptoRe.MatchString(name):
		return canonical.AssetClassCrypto
	case NamesPhysicalMetal(name):
		return canonical.AssetClassMetal
	case etfBondRe.MatchString(name):
		return canonical.AssetClassBondETF
	}
	return canonical.AssetClassETF
}

// NamesPhysicalMetal reports whether a security name reads as a
// physical precious-metal holding — bullion, not the stocks of
// companies that dig it up (the miners guard). Shared by the ETF
// refinement above and by adapters whose source vocabulary is too
// coarse for the distinction (e.g. an "Alternatives" sleeve that
// is actually a physical-gold index fund).
func NamesPhysicalMetal(name string) bool {
	return etfMetalRe.MatchString(name) && !etfMinersRe.MatchString(name)
}

// RefineETFExposure is the 2-D-taxonomy counterpart of RefineETFClass:
// it returns the EXPOSURE (asset_class) of a fund/ETF already known to
// be a collective vehicle, from its security name. The wrapper
// (vehicle=etf or fund) is the caller's — this decides only what the
// wrapper holds. Same keyword logic as RefineETFClass, but in the V2
// vocabulary: crypto → crypto, bullion → metal, bond keywords →
// fixed_income, everything else → public_equity (the default for a
// name that doesn't reveal a non-equity underlying). Name-shy products
// are corrected by instrument_overrides.
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
