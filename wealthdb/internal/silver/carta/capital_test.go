package carta

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCapitalAccountsReadThePartnerMetrics: a partner-metrics row is the
// fund's capital account, its figures as stated; a row a statement alone
// wrote is none, and neither is one outside the window.
func TestCapitalAccountsReadThePartnerMetrics(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	d1231 := silvertest.ISO(t, "2023-12-31")
	silvertest.Exec(t, db, `
INSERT INTO fund_metrics(snapshot_at, entity_external_id, currency, commitment, called_capital,
    capital_contributed, distributions, net_asset_value, payload)
    VALUES (?, 200, '$', '250000.00', '100000.00', '99500.00', '2500.00', '104000.00', '{}')`, d1231)
	conn := openAdapter(t, path)
	w := silvertest.WindowSince0(t, conn)
	got := silvertest.Merged(t, conn, w).CapitalAccounts
	if len(got) != 1 {
		t.Fatalf("capital accounts = %d, want the partner row alone: %+v", len(got), got)
	}
	ca := got[0]
	if ca.AccountExternalID == nil || *ca.AccountExternalID != "IND1" || ca.InstrumentExternalID != "entity:200" ||
		ca.AsOf != d1231 || ca.Currency != "USD" {
		t.Errorf("key = %v %s %d %s", ca.AccountExternalID, ca.InstrumentExternalID, ca.AsOf, ca.Currency)
	}
	for name, c := range map[string]struct {
		got  string
		want string
	}{
		"commitment": {ca.Commitment.String(), "250000"}, "called": {ca.Called.String(), "100000"},
		"paid_in": {ca.PaidIn.String(), "99500"}, "distributed": {ca.Distributed.String(), "2500"},
		"nav": {ca.NAV.String(), "104000"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", name, c.got, c.want)
		}
	}
	if ca.Unfunded != nil || ca.TVPI != nil || ca.DPI != nil {
		t.Errorf("Carta states no unfunded and no multiple: %+v", ca)
	}

	w.End = d1231 - 1
	if n := len(silvertest.Merged(t, conn, w).CapitalAccounts); n != 0 {
		t.Errorf("a window that ends before the row holds %d capital accounts, want none", n)
	}
}
