package ubs

import (
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// What a statement trade says it TRADED, where the instrument itself
// could not be identified.
//
// The instrument answers this whenever it is known, and the adapter
// leaves both fields empty there. But a feed that cannot name the
// security can still name the KIND of thing: the statement's booking
// type is a small, closed vocabulary UBS writes deliberately, and
// `PRECIOUS METAL SELL` says what it sold whether or not anything can
// say which bar. Stated here, the cash flow statement draws the row in
// its real asset class instead of the untracked-destination node.
//
// The pairs are the 2-D taxonomy's (exposure, wrapper) and match what
// this adapter already emits for the same things as POSITIONS: a
// precious metal is (metal, physical), and a private-markets fund is
// (private_equity, fund) — docs/TAXONOMY.md §5, where `private_fund`
// is the intermediate that maps to that pair. `private_equity` is the
// exposure whether the holding is one company or a fund of them; it is
// not a claim that a capital call bought a single company.
var bookingTypeTaxonomy = map[string]struct {
	class   canonical.AssetClass
	vehicle canonical.Vehicle
}{
	"SHARE":                   {canonical.AssetClassPublicEquity, canonical.VehicleStock},
	"SUBSCRIPTION RIGHT":      {canonical.AssetClassPublicEquity, canonical.VehicleRight},
	"PRECIOUS METAL SELL":     {canonical.AssetClassMetal, canonical.VehiclePhysical},
	"PRECIOUS METAL PURCHASE": {canonical.AssetClassMetal, canonical.VehiclePhysical},
	"CAPITAL CALL":            {canonical.AssetClassPrivateEquity, canonical.VehicleFund},
}

// preciousMetalCodes are the ISO 4217 codes for the metals, which a
// statement uses on the metal leg of a trade that carries no booking
// type at all ("You sold XAU ... You bought USD").
var preciousMetalCodes = []string{"XAU", "XAG", "XPT", "XPD"}

// unlinkedSecurityTaxonomy reports what an unidentified securities row
// traded, from its booking type and, failing that, its narrative.
//
// Both empty means the row says nothing this adapter is willing to
// read — which is the honest answer for a bare `SALE` or a row with no
// booking type and no metal on it, and which leaves the statement
// drawing it as untracked, meaning it.
func unlinkedSecurityTaxonomy(bookingType, narrative string) (canonical.AssetClass, canonical.Vehicle) {
	if t, ok := bookingTypeTaxonomy[strings.ToUpper(strings.TrimSpace(bookingType))]; ok {
		return t.class, t.vehicle
	}
	upper := strings.ToUpper(narrative)
	// A subscription right the statement books under a generic type
	// but names in the narrative: "<company> ANR <year>", Anrecht.
	if strings.Contains(upper, " ANR ") || strings.HasSuffix(upper, " ANR") {
		return canonical.AssetClassPublicEquity, canonical.VehicleRight
	}
	for _, m := range preciousMetalCodes {
		// Word-ish: the code is a currency on the trade, not a
		// substring of some longer token.
		if strings.Contains(upper, m+" ") || strings.Contains(upper, m+"/") {
			return canonical.AssetClassMetal, canonical.VehiclePhysical
		}
	}
	return "", ""
}
