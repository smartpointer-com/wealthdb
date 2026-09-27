package viac

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// taxWrapperFor maps silver's `accounts.product_code` to the
// canonical tax_wrapper. Unknown codes fall through to
// taxable_personal (the silver layer documents '1' = inv /
// free investment, '2' = pvb / vested benefits, '3' = p3a;
// other values would be future product lines).
func taxWrapperFor(productCode string) canonical.TaxWrapper {
	switch productCode {
	case "3":
		return canonical.TaxWrapperPillar3a
	case "2":
		return canonical.TaxWrapperVestedBenefits
	case "1":
		return canonical.TaxWrapperTaxablePersonal
	}
	return canonical.TaxWrapperTaxablePersonal
}

// assetClassFor maps silver's already-canonicalised `asset_class`
// string to the coarse canonical AssetClass enum — the intermediate
// 1-D class taxonomyFor then maps to an (exposure, vehicle) pair.
// Silver does the VIAC-raw → canonical translation (e.g. EQUITIES →
// equity); we just type-cast and validate against the coarse
// vocabulary. Unknown values fall through to AssetClassOther.
func assetClassFor(raw string) canonical.AssetClass {
	c := canonical.AssetClass(raw)
	if c.ValidCoarse() {
		return c
	}
	return canonical.AssetClassOther
}

// listedPrivateEquityRe matches the one exchange-traded holding in the
// otherwise all-CSIF VIAC universe: the iShares Listed Private Equity
// UCITS ETF. Silver's coarse asset_class (equity / other) can't tell it
// apart from the CSIF equity funds, so it's recognised by name.
var listedPrivateEquityRe = regexp.MustCompile(`(?i)private\s+equity`)

// taxonomyFor derives the 2-D taxonomy pair (exposure, vehicle) — see
// docs/TAXONOMY.md — from silver's `asset_class` plus the instrument
// name. It uses assetClassFor internally to canonicalise the coarse
// class, then maps that intermediate to the emitted pair.
//
// Every VIAC holding is a Credit Suisse Index Fund (CSIF) — a
// non-exchange-traded institutional index fund, so the vehicle is
// `fund` throughout — except the one listed private-equity ETF, which
// is exchange-traded (`etf`). Exposure comes from silver's already-
// canonicalised class where it's unambiguous; the coarse 'other'
// (VIAC's ALTERNATIVES sleeve) falls back to a name-derived exposure.
//
// The returned pair always satisfies canonical.ValidTaxonomyPair.
func taxonomyFor(rawClass, name string) (canonical.AssetClass, canonical.Vehicle) {
	// Name-based special case first: the iShares Listed Private Equity
	// ETF, regardless of whether silver filed it under EQUITIES or
	// ALTERNATIVES. Listed PE is private-equity exposure in an ETF
	// wrapper (TAXONOMY.md §5.3).
	if listedPrivateEquityRe.MatchString(name) {
		return canonical.AssetClassPrivateEquity, canonical.VehicleETF
	}
	switch assetClassFor(rawClass) {
	case canonical.AssetClassEquity:
		// CSIF equity index funds.
		return canonical.AssetClassPublicEquity, canonical.VehicleFund
	case canonical.AssetClassBond:
		// CSIF bond index funds.
		return canonical.AssetClassFixedIncome, canonical.VehicleFund
	case canonical.AssetClassFund:
		// Silver maps VIAC's REAL_ESTATE section to coarse 'fund': a
		// property CSIF is real-estate exposure in a fund wrapper.
		return canonical.AssetClassRealEstate, canonical.VehicleFund
	case canonical.AssetClassMetal:
		// COMMODITIES sleeve — a physical-precious-metal CSIF.
		return canonical.AssetClassMetal, canonical.VehicleFund
	case canonical.AssetClassMoneyMarket:
		// LIQUIDITY money-market fund → cash exposure, fund wrapper
		// (TAXONOMY.md §5.8: money-market funds are cash, not fixed
		// income).
		return canonical.AssetClassCash, canonical.VehicleFund
	}
	// Coarse 'other' (VIAC's ALTERNATIVES sleeve) and any unknown class:
	// recover the exposure from the fund name where it's unambiguous.
	// silver.RefineETFExposure returns public_equity as its "name reveals
	// nothing" default; for an ALTERNATIVES holding that default isn't
	// meaningful, so only a name that clearly reads crypto / metal /
	// fixed-income drives the exposure — anything else stays (other,
	// other). The wrapper remains fund (CSIF).
	switch exp := silver.RefineETFExposure(name); exp {
	case canonical.AssetClassCrypto, canonical.AssetClassMetal, canonical.AssetClassFixedIncome:
		return exp, canonical.VehicleFund
	}
	return canonical.AssetClassOther, canonical.VehicleOther
}

// txKindFor maps silver's already-canonicalised `transactions.kind`
// string to the canonical TxKind enum. Silver does the
// VIAC-raw → canonical translation (TRADE_BUY → buy, FUSION_*
// → corporate_action, etc.); we just type-cast and validate.
// Unknown values fall through to TxKindOther.
func txKindFor(raw string) canonical.TxKind {
	k := canonical.TxKind(raw)
	if k.Valid() {
		return k
	}
	return canonical.TxKindOther
}
