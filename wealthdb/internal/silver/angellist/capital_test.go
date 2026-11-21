package angellist

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCapitalAccountsReadThePositionNodes: an offering whose node states
// a commitment is a capital account at the data date of the dump that
// last listed it, its figures scaled from cents. The commitment is the
// node's commitmentAmount, else its investmentAmount: a fund's, above
// what it called, leaves the difference unfunded; an SPV's equals what
// it paid, so nothing is. One whose node states neither a commitment nor
// a TVPI is none, and neither is one dated outside the window.
func TestCapitalAccountsReadThePositionNodes(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	silvertest.Exec(t, db, `
INSERT INTO offerings(position_external_id, kind, company_name) VALUES ('p4', 'spv', 'SPV Delta');
UPDATE offerings SET last_seen_at = 1000, payload = '{"commitmentAmount":{"currency":"USD","fractional":5000000},
    "investmentAmount":{"currency":"USD","fractional":4500000},
    "contributedAmount":{"currency":"USD","fractional":4000000},"realizedValue":{"currency":"USD","fractional":0},
    "unrealizedValue":{"currency":"USD","fractional":9000000},"tvpi":2.25}' WHERE position_external_id = 'p1';
UPDATE offerings SET last_seen_at = 1000, payload = '{"investmentAmount":{"currency":"EUR","fractional":5000000},
    "contributedAmount":{"currency":"EUR","fractional":2000000}}' WHERE position_external_id = 'p2';
UPDATE offerings SET last_seen_at = 900, payload = '{"investmentAmount":{"currency":"USD","fractional":1500000},
    "contributedAmount":{"currency":"USD","fractional":1500000},"tvpi":0}' WHERE position_external_id = 'p3';
UPDATE offerings SET last_seen_at = 1000, payload = '{"contributedAmount":{"currency":"USD","fractional":2000000}}'
 WHERE position_external_id = 'p4';
INSERT INTO portfolio_summary(snapshot_at, invest_account_slug, data_date) VALUES (1000, 'acct', 990)`)
	conn := openAdapter(t, path)
	w := silvertest.WindowSince0(t, conn)
	got := silvertest.Merged(t, conn, w).CapitalAccounts
	if len(got) != 3 {
		t.Fatalf("capital accounts = %+v, want p1's, p2's and p3's", got)
	}
	p1, fund, spv := got[0], got[1], got[2]
	if p1.InstrumentExternalID != "p1" || *p1.AccountExternalID != "acct" || p1.AsOf != 990 || p1.Currency != "USD" ||
		p1.Commitment.String() != "50000" || p1.Called.String() != "40000" || p1.Distributed.String() != "0" ||
		p1.NAV.String() != "90000" || p1.TVPI == nil || *p1.TVPI != 2.25 {
		t.Errorf("p1, its commitmentAmount over its investmentAmount: %+v", p1)
	}
	if p1.PaidIn != nil || p1.Unfunded != nil || p1.DPI != nil {
		t.Errorf("a position node states no paid in apart from its contributions, no unfunded and no DPI: %+v", p1)
	}
	// Gold reads the unfunded as the commitment less the calls.
	if fund.InstrumentExternalID != "p2" || fund.Currency != "EUR" || fund.Commitment.String() != "50000" ||
		fund.Called.String() != "20000" || fund.Unfunded != nil || fund.TVPI != nil {
		t.Errorf("the fund, committed above its calls: %+v", fund)
	}
	if d := fund.Commitment.Sub(*fund.Called); d.String() != "30000" {
		t.Errorf("the fund's commitment less its calls = %s, want 30000 unfunded", d)
	}
	// A dump with no portfolio summary dates the node at its own time.
	if spv.InstrumentExternalID != "p3" || spv.AsOf != 900 || spv.NAV != nil || spv.TVPI == nil || *spv.TVPI != 0 ||
		spv.Commitment.String() != "15000" || !spv.Commitment.Equal(*spv.Called) {
		t.Errorf("the SPV, invested whole at once, nothing unfunded: %+v", spv)
	}

	w.End = 950
	if got := silvertest.Merged(t, conn, w).CapitalAccounts; len(got) != 1 || got[0].InstrumentExternalID != "p3" {
		t.Errorf("a window that ends before the data date holds %+v, want p3's alone", got)
	}
}
