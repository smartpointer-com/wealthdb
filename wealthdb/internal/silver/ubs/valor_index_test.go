package ubs

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// A Swiss ISIN is `CH` plus the valor padded to nine digits plus a
// check digit, so the ISIN IS the valor for every Swiss line — which is
// the only road to the instruments PSN describes without an identifier
// object at all.
func TestValorFromSwissISIN(t *testing.T) {
	for _, tc := range []struct{ isin, want string }{
		{"CH0000000017", "1"},         // leading zeros dropped
		{"CH0012345678", "1234567"},   // the ordinary shape
		{"CH1234567890", "123456789"}, // a full nine-digit valor
		{"US0000000017", ""},          // not Swiss
		{"CH00123456", ""},            // too short to be an ISIN
		{"CH00ABCDEF78", ""},          // not all digits
		{"", ""},
	} {
		if got := valorFromSwissISIN(tc.isin); got != tc.want {
			t.Errorf("valorFromSwissISIN(%q) = %q, want %q", tc.isin, got, tc.want)
		}
	}
}

// One spelling of a number, whichever road stated it, or the two roads
// would index the same instrument twice and collide with themselves.
func TestNormalizeValor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1234567", "1234567"},
		{"0001234567", "1234567"},
		{" 1234567 ", "1234567"},
		{"12A4567", ""},
		{"", ""},
		{"0000", ""},
	} {
		if got := normalizeValor(tc.in); got != tc.want {
			t.Errorf("normalizeValor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A row whose instrument cannot be identified can still say what KIND
// of thing it traded, and the statement's booking type is where UBS
// says it. Both empty is the honest answer where nothing does.
func TestUnlinkedSecurityTaxonomy(t *testing.T) {
	for _, tc := range []struct {
		name, booking, narrative string
		class                    canonical.AssetClass
		vehicle                  canonical.Vehicle
	}{
		{"a share", "SHARE", "", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		{"a metal sale", "PRECIOUS METAL SELL", "", canonical.AssetClassMetal, canonical.VehiclePhysical},
		{"a capital call", "CAPITAL CALL", "", canonical.AssetClassPrivateEquity, canonical.VehicleFund},
		{"a subscription right", "SUBSCRIPTION RIGHT", "", canonical.AssetClassPublicEquity, canonical.VehicleRight},
		// A right the statement books under a generic type and names
		// only in the narrative.
		{"a right named in the narrative", "SALE", "EXAMPLE CORP ANR 22",
			canonical.AssetClassPublicEquity, canonical.VehicleRight},
		// A metal leg on a trade with no booking type at all.
		{"gold sold for dollars", "", "You sold XAU / FW.g; You bought USD",
			canonical.AssetClassMetal, canonical.VehiclePhysical},
		// Nothing to read: say nothing.
		{"a bare sale", "SALE", "Example Payee", "", ""},
		{"nothing at all", "", "", "", ""},
		// A token merely containing a metal code is not a metal trade.
		{"not a metal", "", "XAUCORP", "", ""},
	} {
		c, v := unlinkedSecurityTaxonomy(tc.booking, tc.narrative)
		if c != tc.class || v != tc.vehicle {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, c, v, tc.class, tc.vehicle)
		}
		// Whatever it says must be a pair gold will accept.
		if c != "" && !canonical.ValidTaxonomyPair(c, v) {
			t.Errorf("%s: (%q, %q) is not an admitted pair", tc.name, c, v)
		}
	}
}
