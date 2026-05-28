package viac

import "github.com/ptu/wealthdb/internal/canonical"

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
// string to the canonical AssetClass enum. Silver does the
// VIAC-raw → canonical translation (e.g. EQUITIES → equity); we
// just type-cast and validate. Unknown values fall through to
// AssetClassOther.
func assetClassFor(raw string) canonical.AssetClass {
	c := canonical.AssetClass(raw)
	if c.Valid() {
		return c
	}
	return canonical.AssetClassOther
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
