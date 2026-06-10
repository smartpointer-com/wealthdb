package manual

import "github.com/ptu/wealthdb/internal/canonical"

// assetClassFor maps a manual position `kind` to its canonical AssetClass.
//
// The collector deliberately uses the canonical asset_class names as the
// bronze `kind` (collectors/manual/DESIGN.md §6) — real_estate,
// private_equity, convertible_note, private_fund, spv, other — so this is an
// IDENTITY map with a safety net: a kind the gold enum doesn't recognise
// (a class added to the collector's open-ended POSITION_KINDS but not yet to
// internal/canonical/enums.go) falls through to AssetClassOther rather than
// emitting an invalid asset_class the gold writer would reject.
func assetClassFor(kind string) canonical.AssetClass {
	ac := canonical.AssetClass(kind)
	if ac.Valid() {
		return ac
	}
	return canonical.AssetClassOther
}
