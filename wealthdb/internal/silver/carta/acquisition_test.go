package carta

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A fund paid back capital before its first NAV is carried at the capital paid
// in less the capital paid back, while its book value stays the capital paid
// in, as on its NAV rows. Every fund row is acquired on the fund's first call.
func TestAFundsBookValueIsGrossCalledCapitalAndItIsAcquiredOnItsFirstCall(t *testing.T) {
	byDay, _ := fundPositions(t, fundBookFixture(t, false, `
INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, snapshot_at, kind,
    flow_date, amount, currency, payload) VALUES
    ('dist:300:notice:n2', '300', 1800000000, 'distribution', '07/31/2024', 10000, 'USD', '{}');`))
	firstCall := time.Unix(unixDate(t, "2023-02-15"), 0).UTC()
	for _, c := range []struct{ day, mv, bv string }{
		{"2023-02-15", "120000", "120000"},
		{"2024-06-30", "150000", "150000"},
		{"2024-07-31", "140000", "150000"},
		{"2024-09-30", "140000", "150000"},
	} {
		p, ok := byDay[unixDate(t, c.day)]
		if !ok {
			t.Errorf("%s: no fund position", c.day)
			continue
		}
		if p.MarketValue == nil || p.MarketValue.String() != c.mv {
			t.Errorf("%s: market value %v, want %s", c.day, p.MarketValue, c.mv)
		}
		if p.BookValue == nil || p.BookValue.String() != c.bv {
			t.Errorf("%s: book value %v, want %s", c.day, p.BookValue, c.bv)
		}
		if p.AcquisitionDate == nil || !p.AcquisitionDate.Equal(firstCall) {
			t.Errorf("%s: acquisition date %v, want %v", c.day, p.AcquisitionDate, firstCall)
		}
	}
}

// A convertible Carta states no acquisition date for is acquired on its issue
// date. A share certificate's issue date is not its acquisition date (it is
// re-issued on a split or transfer), so it gets none.
func TestAConvertibleIsAcquiredOnItsIssueDate(t *testing.T) {
	path, db := newFixtureSilver(t)
	snap := unixDate(t, "2025-01-31")
	if _, err := db.Exec(fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (%[1]d, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (%[1]d, 100, 'IND1', 0, 'Example Note Co', '{}'),
           (%[1]d, 200, 'IND1', 0, 'Example Share Co', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    cost, position_status, currency, issue_date, payload) VALUES
    (%[1]d, 100, 'convertible', 1, 25000, 'held', 'USD', '03/15/2022', '{}'),
    (%[1]d, 200, 'share',       2,  1000, 'held', 'USD', '04/01/2024', '{}');`, snap)); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := map[string]canonical.PositionChange{}
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Positions {
			got[p.PositionKey] = p
		}
		if !more {
			break
		}
	}
	issued := time.Unix(unixDate(t, "2022-03-15"), 0).UTC()
	if p := got[positionKey(100)]; p.AcquisitionDate == nil || !p.AcquisitionDate.Equal(issued) {
		t.Errorf("convertible acquisition date %v, want %v", p.AcquisitionDate, issued)
	}
	if p := got[positionKey(200)]; p.AcquisitionDate != nil {
		t.Errorf("share acquisition date %v, want none", p.AcquisitionDate)
	}
}
