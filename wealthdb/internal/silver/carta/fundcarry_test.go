package carta

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// fundBookFixture is a fund (entity 300) called twice before its first NAV,
// and optionally a cap-table company (entity 100) held from an earlier day.
// The first call's date is ISO-shaped, as a supplied pre-coverage row is.
func fundBookFixture(t *testing.T, withCompany bool) silver.Connection {
	t.Helper()
	path, db := newFixtureSilver(t)
	nav := unixDate(t, "2024-09-30")
	stmts := fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1800000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (%d, 300, 'IND1', 1, 'Example Fund', '{}');
INSERT INTO fund_metrics(snapshot_at, entity_external_id, currency, net_asset_value,
    capital_contributed, payload) VALUES (%d, 300, 'USD', '140000', '150000', '{}');
INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, snapshot_at, kind,
    flow_date, amount, currency, payload) VALUES
    ('call:300:supplied:0', '300', 1800000000, 'capital_call', '2023-02-15', 120000, 'USD', '{}'),
    ('call:300:notice:n1',  '300', 1800000000, 'capital_call', '06/30/2024',  30000, 'USD', '{}');`,
		nav, nav)
	if withCompany {
		d0101 := unixDate(t, "2023-01-01")
		stmts += fmt.Sprintf(`
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (%d, 100, 'IND1', 0, 'ACME Inc', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload)
    VALUES (%d, 100, 'share', 1, 1000, 500, 5000, 'held', '$', '{}');`, d0101, d0101)
	}
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
	return openAdapter(t, path)
}

// fundPositions collects the fund's position per snapshot day, and every
// snapshot day the stream emitted.
func fundPositions(t *testing.T, conn silver.Connection) (map[int64]canonical.PositionChange, map[int64]bool) {
	t.Helper()
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
	byDay := map[int64]canonical.PositionChange{}
	days := map[int64]bool{}
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range b.Accounts {
			days[a.FirstSeenAt] = true
		}
		for _, p := range b.Positions {
			if p.PositionKey == positionKey(300) {
				byDay[p.SnapshotAt] = p
			}
		}
		if !more {
			break
		}
	}
	return byDay, days
}

// Until its first NAV a fund is carried at the capital paid in, so a call is
// never a loss in the period it was paid; the first NAV then marks it.
func TestAFundIsCarriedAtItsCalledCapitalUntilItsFirstNAV(t *testing.T) {
	byDay, days := fundPositions(t, fundBookFixture(t, true))

	d0101 := unixDate(t, "2023-01-01")
	if !days[d0101] {
		t.Fatalf("no snapshot on the company's day; days = %v", days)
	}
	if p, ok := byDay[d0101]; ok {
		t.Errorf("fund held before its first call: %+v", p)
	}
	for _, c := range []struct {
		day        string
		mv, bv     string
		calledOnly bool
	}{
		{"2023-02-15", "120000", "120000", true},
		{"2024-06-30", "150000", "150000", true},
		{"2024-09-30", "140000", "150000", false},
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
		if p.AssetClass != canonical.AssetClassPrivateEquity || p.Vehicle != canonical.VehicleFund {
			t.Errorf("%s: taxonomy (%s, %s), want (private_equity, fund)", c.day, p.AssetClass, p.Vehicle)
		}
		var payload map[string]any
		_ = json.Unmarshal(p.Payload, &payload)
		if got := payload["valuation_basis"] == "called_capital"; got != c.calledOnly {
			t.Errorf("%s: payload %s, carried at called capital = %v, want %v", c.day, p.Payload, got, c.calledOnly)
		}
	}
}

// A call that precedes every statement and every cap-table event still falls
// inside the load window, as a snapshot and as a transaction pair.
func TestTheWindowReachesAFundCallBeforeAnyStatement(t *testing.T) {
	conn := fundBookFixture(t, false)
	ctx := context.Background()
	first := unixDate(t, "2023-02-15")

	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != first {
		t.Errorf("window starts %d, want the first call %d", w.Start, first)
	}
	st, err := conn.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != first || st.OldestTransactionAt != first {
		t.Errorf("status oldest snapshot %d, transaction %d, want both %d",
			st.OldestSnapshotAt, st.OldestTransactionAt, first)
	}

	byDay, _ := fundPositions(t, conn)
	if _, ok := byDay[first]; !ok {
		t.Error("no carried position on the first call's day")
	}
	stream, err := conn.Transactions(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	b, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var legs int
	for _, tx := range b.Transactions {
		if tx.OccurredAt == first {
			legs++
		}
	}
	if legs != 2 {
		t.Errorf("%d legs on the first call's day, want the deposit + contribution pair", legs)
	}
}
