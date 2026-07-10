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
	case etfMetalRe.MatchString(name) && !etfMinersRe.MatchString(name):
		return canonical.AssetClassMetal
	case etfBondRe.MatchString(name):
		return canonical.AssetClassBondETF
	}
	return canonical.AssetClassETF
}
