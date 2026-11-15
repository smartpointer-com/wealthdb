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

// listedPrivateEquityRe recognises a listed private-equity ETF by its
// name. Silver's asset_class (equity / other) cannot tell it apart from
// an equity index fund.
var listedPrivateEquityRe = regexp.MustCompile(`(?i)private\s+equity`)

// taxonomyFor derives the 2-D taxonomy pair (exposure, vehicle) — see
// docs/TAXONOMY.md — from silver's `asset_class` plus the instrument
// name. Silver translates VIAC's raw section to its own class string
// (EQUITIES → equity); the switch below reads that string.
//
// A VIAC holding reads as an institutional index fund that is not
// exchange-traded, so its vehicle is `fund`, unless its name marks a
// listed private-equity ETF (`etf`). Exposure comes from silver's already-
// canonicalised class where it's unambiguous; silver's 'other'
// (VIAC's ALTERNATIVES sleeve) falls back to a name-derived exposure.
//
// The returned pair always satisfies canonical.ValidTaxonomyPair.
func taxonomyFor(rawClass, name string) (canonical.AssetClass, canonical.Vehicle) {
	// Name-based special case first: a listed private-equity ETF,
	// whether silver filed it under EQUITIES or ALTERNATIVES. Listed PE is private-equity exposure in an ETF
	// wrapper (TAXONOMY.md §5.3).
	if listedPrivateEquityRe.MatchString(name) {
		return canonical.AssetClassPrivateEquity, canonical.VehicleETF
	}
	switch rawClass {
	case "equity":
		// CSIF equity index funds.
		return canonical.AssetClassPublicEquity, canonical.VehicleFund
	case "bond":
		// CSIF bond index funds.
		return canonical.AssetClassFixedIncome, canonical.VehicleFund
	case "fund":
		// Silver maps VIAC's REAL_ESTATE section to 'fund': a
		// property CSIF is real-estate exposure in a fund wrapper.
		return canonical.AssetClassRealEstate, canonical.VehicleFund
	case "metal":
		// COMMODITIES sleeve — a physical-precious-metal CSIF.
		return canonical.AssetClassMetal, canonical.VehicleFund
	case "money_market":
		// LIQUIDITY money-market fund → cash exposure, fund wrapper
		// (TAXONOMY.md §5.8: money-market funds are cash, not fixed
		// income).
		return canonical.AssetClassCash, canonical.VehicleFund
	}
	// Silver's 'other' (VIAC's ALTERNATIVES sleeve) and any unknown class:
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
// → corporate_action, etc.); txKindFor casts and validates it.
// Unknown values fall through to TxKindOther.
func txKindFor(raw string) canonical.TxKind {
	k := canonical.TxKind(raw)
	if k.Valid() {
		return k
	}
	return canonical.TxKindOther
}
