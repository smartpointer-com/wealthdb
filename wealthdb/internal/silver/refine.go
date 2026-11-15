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
	etfBondRe   = regexp.MustCompile(`(?i)\bBONDS?\b|TREASURY|FIXED INCOME|\b(?:FIXED|FXD) INC(?:OME)?\b|MUNICIPAL|\bMUNI\b|\bTIPS\b`)
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

// namesMoneyMarket reports whether a fund's security name reads as a
// money-market fund — a cash equivalent (TAXONOMY.md: money-market
// funds are cash, not fixed income). FundExposure reads it before the
// name-based exposure refinement, which has no money-market awareness.
func namesMoneyMarket(name string) bool {
	return moneyMarketRe.MatchString(name)
}

// FundExposure is the exposure of a fund that is not exchange traded, read
// from its name: a money-market fund is cash, any other fund takes
// RefineETFExposure. A purchased money fund reaches a fund classification
// path rather than the account's cash sweep, and it is a cash equivalent,
// not the exposure RefineETFExposure would guess from a bond keyword in its
// name.
func FundExposure(name string) canonical.AssetClass {
	if namesMoneyMarket(name) {
		return canonical.AssetClassCash
	}
	return RefineETFExposure(name)
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

// bondAbbrevRe matches the bond words a broker abbreviates in a fund's
// description ("TOTAL BD MKT", "SHORT-TERM TREAS", "INFL PROT"), which
// RefineETFExposure reads only spelled out.
var bondAbbrevRe = regexp.MustCompile(`(?i)\bBN?D\b|\bTREAS\b|\bGOVT\b|\bINFL(?:ATION)?[- ]PROT`)

// ETFExposureAbbrev is an ETF's exposure read from its name:
// RefineETFExposure, with abbreviated bond words read as fixed income.
func ETFExposureAbbrev(name string) canonical.AssetClass {
	return BondByAbbrev(RefineETFExposure(name), name)
}

// FundExposureAbbrev is the same reading for a fund that is not exchange
// traded, a money-market fund's name read as cash first (FundExposure).
func FundExposureAbbrev(name string) canonical.AssetClass {
	return BondByAbbrev(FundExposure(name), name)
}

// BondByAbbrev reads a fund that the spelled-out name patterns leave as
// public equity as fixed income where its name holds an abbreviated
// bond word.
func BondByAbbrev(ac canonical.AssetClass, name string) canonical.AssetClass {
	if ac == canonical.AssetClassPublicEquity && bondAbbrevRe.MatchString(name) {
		return canonical.AssetClassFixedIncome
	}
	return ac
}

// RefineClass reads a lookup's type of a security over the pair an
// adapter gave the instrument. Gold's class pass (gold.RefineClasses)
// calls it for every instrument of every source that OpenFIGI types and
// the config does not pin. It returns the (asset_class, vehicle) pair of
// an instrument named name, which the adapter classed (ac, v) and
// OpenFIGI types securityType. The precedence is the lookup's type, then
// the adapter's own type, then the name:
//
//   - A bond's type (Govt, Corp, Muni, …) makes it (fixed_income, bond).
//   - A share's type (Common Stock, Depositary Receipt, REIT, …) makes
//     it (public_equity, stock).
//   - A fund's type keeps an ETF or a fund as the adapter types it, an
//     ETP's making a fund an ETF. Anything else becomes a fund (an ETP
//     an ETF) with its exposure read from its name: a trust an adapter
//     types as an equity, say.
//   - Any other type (an option, a warrant, an index) leaves the pair.
//
// A pair the taxonomy does not admit leaves the pair too, so every pair
// it returns satisfies canonical.ValidTaxonomyPair.
func RefineClass(securityType string, ac canonical.AssetClass, v canonical.Vehicle,
	name string) (canonical.AssetClass, canonical.Vehicle) {
	refined, rv := ac, v
	switch figi := canonical.OpenFIGIVehicle(securityType); figi {
	case canonical.VehicleBond:
		refined, rv = canonical.AssetClassFixedIncome, canonical.VehicleBond
	case canonical.VehicleStock:
		refined, rv = canonical.AssetClassPublicEquity, canonical.VehicleStock
	case canonical.VehicleETF, canonical.VehicleFund:
		switch {
		case v == canonical.VehicleETF:
			// OpenFIGI types an ETF "Mutual Fund": the adapter's type is
			// the finer one.
		case v == canonical.VehicleFund:
			rv = figi
		case figi == canonical.VehicleETF:
			refined, rv = ETFExposureAbbrev(name), canonical.VehicleETF
		default:
			refined, rv = FundExposureAbbrev(name), canonical.VehicleFund
		}
	}
	if !canonical.ValidTaxonomyPair(refined, rv) {
		return ac, v
	}
	return refined, rv
}
