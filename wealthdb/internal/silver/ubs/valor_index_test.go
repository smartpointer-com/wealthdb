package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The portfolio transaction list prints a security's valor and its ISIN
// in adjacent columns of one row, which is the only road to a security
// the feed's instrument dimension never described and whose non-Swiss
// ISIN no arithmetic recovers. Those are exactly the securities a
// printed statement names by valor alone.
func TestValorIndexReadsThePortfolioList(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.valor, row.isin = "10000001", "XX0000000001"
	seedPortfolioTx(t, r, row)

	index, err := buildValorIndex(context.Background(), nil, r)
	if err != nil {
		t.Fatal(err)
	}
	if got := index["10000001"]; got != "XX0000000001" {
		t.Errorf("index[valor] = %q, want the ISIN beside it", got)
	}
}

// Two feeds disagreeing about what a valor names is one wrong
// assumption, and the honest answer to a wrong assumption is no answer
// — whichever feed spoke first.
func TestValorIndexDropsACrossFeedCollision(t *testing.T) {
	r := newPortfolioTxFixture(t)
	row := ptxTrade("ptx:REF1")
	row.valor, row.isin = "10000001", "XX0000000002"
	seedPortfolioTx(t, r, row)
	psnDB := newPortfolioPSN(t)
	if _, err := psnDB.Exec(`
        INSERT INTO instruments (snapshot_at, relationship_id, isin, payload)
        VALUES (1, 'R1', 'XX0000000001',
                json_object('InstrIdtfr', json_object('Valor', '10000001')))`); err != nil {
		t.Fatal(err)
	}

	index, err := buildValorIndex(context.Background(), &psnReader{db: psnDB}, r)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := index["10000001"]; ok {
		t.Errorf("index[valor] = %q, want the collision left unresolved", got)
	}
}

// A silver that predates the list, or has no feed beside it, resolves
// less rather than failing.
func TestValorIndexToleratesAMissingSide(t *testing.T) {
	index, err := buildValorIndex(context.Background(), nil, newWebTxFixture(t))
	if err != nil {
		t.Fatalf("buildValorIndex: %v", err)
	}
	if len(index) != 0 {
		t.Errorf("index has %d entries, want none", len(index))
	}
}

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

// A valor the index holds resolves to its instrument; a well-formed one it
// does not hold becomes the hint a config link closes; anything else says
// nothing.
func TestResolveValor(t *testing.T) {
	index := map[string]string{"1234567": "XS0000000001"}
	for _, tc := range []struct{ in, wantID, wantHint string }{
		{"0001234567", "XS0000000001", ""},
		{"7654321", "", "7654321"},
		{"12A4567", "", ""},
		{"", "", ""},
	} {
		id, hint := resolveValor(index, tc.in)
		got := ""
		if id != nil {
			got = *id
		}
		if got != tc.wantID || hint != tc.wantHint {
			t.Errorf("resolveValor(%q) = (%q, %q), want (%q, %q)", tc.in, got, hint, tc.wantID, tc.wantHint)
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
