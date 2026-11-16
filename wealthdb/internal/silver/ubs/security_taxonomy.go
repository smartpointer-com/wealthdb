package ubs

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// preciousMetalCodes are the ISO 4217 codes for the metals, which a
// statement uses on the metal leg of a trade that carries no booking
// type at all ("You sold XAU ... You bought USD").
var preciousMetalCodes = []string{"XAU", "XAG", "XPT", "XPD"}

// unlinkedSecurityTaxonomy reports what an unidentified securities row
// traded, from its booking type (bookingTypes) and, failing that, its
// narrative. Stated, the cash flow statement draws the row in its real
// asset class instead of the untracked-destination node.
//
// Both empty means the row says nothing this adapter is willing to
// read — which is the honest answer for a bare `SALE` or a row with no
// booking type and no metal on it, and which leaves the statement
// drawing it as untracked, meaning it.
func unlinkedSecurityTaxonomy(bookingType, narrative string) (canonical.AssetClass, canonical.Vehicle) {
	if b := bookingOf(bookingType); b.class != "" {
		return b.class, b.vehicle
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
