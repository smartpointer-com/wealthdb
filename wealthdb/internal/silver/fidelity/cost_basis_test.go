package fidelity

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Cost basis, open lots and realized lots. Every account, symbol,
// CUSIP-shaped token and figure below is invented.

//go:embed testdata/lots_schema.sql
var lotsSchemaSQL string

func addLotsSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(lotsSchemaSQL); err != nil {
		t.Fatalf("lots schema: %v", err)
	}
}

func seedSQL(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// allSnapshots drains a full Snapshots stream into one batch.
func allSnapshots(t *testing.T, conn silver.Connection) canonical.SnapshotBatch {
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
	var all canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// A lot sits in the batch of the position it belongs to, and
		// states its basis origin exactly when it states a book value.
		for _, l := range b.PositionLots {
			if (l.BookValue == nil) != (l.BasisOrigin == "") {
				t.Errorf("lot %s/%s@%d: origin %q with book %v", l.PositionKey, l.LotKey, l.SnapshotAt, l.BasisOrigin, l.BookValue)
			}
			found := false
			for _, p := range b.Positions {
				found = found || (p.SnapshotAt == l.SnapshotAt &&
					p.AccountExternalID == l.AccountExternalID && p.PositionKey == l.PositionKey)
			}
			if !found {
				t.Errorf("lot %s/%s@%d has no position in its batch", l.PositionKey, l.LotKey, l.SnapshotAt)
			}
		}
		all.Positions = append(all.Positions, b.Positions...)
		all.PositionLots = append(all.PositionLots, b.PositionLots...)
		all.CashBalances = append(all.CashBalances, b.CashBalances...)
		if !more {
			return all
		}
	}
}

func positionAt(t *testing.T, b canonical.SnapshotBatch, at int64, acct, key string) canonical.PositionChange {
	t.Helper()
	for _, p := range b.Positions {
		if p.SnapshotAt == at && p.AccountExternalID == acct && p.PositionKey == key {
			return p
		}
	}
	t.Fatalf("no position %s/%s at %d", acct, key, at)
	return canonical.PositionChange{}
}

func lotsAt(b canonical.SnapshotBatch, at int64, acct, key string) []canonical.PositionLotChange {
	var out []canonical.PositionLotChange
	for _, l := range b.PositionLots {
		if l.SnapshotAt == at && l.AccountExternalID == acct && l.PositionKey == key {
			out = append(out, l)
		}
	}
	return out
}

func decEq(d *canonical.Decimal, want string) bool {
	w, err := canonical.NewDecimalFromString(want)
	return err == nil && d != nil && d.Equal(w)
}

// TestBookValuesAreStatedLotSums: a live holding's cost basis total and
// a statement holding's cost basis are book values stamped stated /
// lots / included. A NULL basis carries no value and no stamp, and a
// core position stays a cash balance.
func TestBookValuesAreStatedLotSums(t *testing.T) {
	path, db := newFixtureSilver(t)
	addLotsSchema(t, db)
	seedSQL(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (5000, 13, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (5000, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, currency, is_core_position, quantity, current_value, cost_basis_total, payload) VALUES
            (5000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 'USD', 0, 10, 2500, 2000, '{}'),
            (5000, 'ACC1', 'QQQ', 'Nasdaq ETF', 'etf', 'USD', 0, 5, 2000, NULL, '{}'),
            (5000, 'ACC1', 'SPAXX', 'Money Market', 'money_market', 'USD', 1, 0, 300, 300, '{}');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, market_value, cost_basis, source_sha256, payload) VALUES
            (1000, 'ACC1', 'TOTAL MARKET ETF', 'VTI', 8, 1600, 1500, 'sha-stmt', '{}'),
            (1000, 'ACC1', 'CORE ACCOUNT', NULL, NULL, 50, NULL, 'sha-stmt', '{}');
    `)
	b := allSnapshots(t, openAdapter(t, path))

	vti := positionAt(t, b, 5000, "ACC1", "VTI")
	if !decEq(vti.BookValue, "2000") || vti.Basis != lotBasis {
		t.Errorf("live VTI book = %v %+v, want 2000 stated/lots/included", vti.BookValue, vti.Basis)
	}
	if qqq := positionAt(t, b, 5000, "ACC1", "QQQ"); qqq.BookValue != nil || !qqq.Basis.IsZero() {
		t.Errorf("live QQQ book = %v %+v, want none", qqq.BookValue, qqq.Basis)
	}
	if len(b.CashBalances) != 1 {
		t.Errorf("cash balances = %d, want the core position", len(b.CashBalances))
	}
	stmt := positionAt(t, b, 1000, "ACC1", "VTI")
	if !decEq(stmt.BookValue, "1500") || stmt.Basis != lotBasis {
		t.Errorf("statement VTI book = %v %+v, want 1500 stated/lots/included", stmt.BookValue, stmt.Basis)
	}
	core := positionAt(t, b, 1000, "ACC1", syntheticHistoricalInstrumentKey("CORE ACCOUNT"))
	if core.BookValue != nil || !core.Basis.IsZero() {
		t.Errorf("statement core book = %v %+v, want none", core.BookValue, core.Basis)
	}
	for _, p := range b.Positions {
		if err := canonical.ValidateBookValue(p.BookValue, p.Basis); err != nil {
			t.Errorf("%s@%d: %v", p.PositionKey, p.SnapshotAt, err)
		}
	}
}

// TestOpenLotsCarryWhileTheyDescribeThePosition: a lot set fetched at
// one dump rides every later snapshot of the position while its sums
// still match; a changed position whose fetch was deferred, a fetch
// that does not sum to its own position, and a sold position get none.
func TestOpenLotsCarryWhileTheyDescribeThePosition(t *testing.T) {
	path, db := newFixtureSilver(t)
	addLotsSchema(t, db)
	seedSQL(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1000, 13, '/x/1'), (2000, 13, '/x/2'), (3000, 13, '/x/3'), (4000, 13, '/x/4');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{}'), (2000, 'ACC1', '{}'), (3000, 'ACC1', '{}'), (4000, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, quantity, current_value, cost_basis_total, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 10, 2500, 1000, '{}'),
            (2000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 10, 2600, 1000, '{}'),
            (3000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 12, 3100, 1500, '{}'),
            (4000, 'ACC1', 'QQQ', 'Nasdaq ETF', 'etf', 6, 3000, 2400, '{}'),
            (4000, 'ACC1', 'BND1', 'Treasury Note', 'bond', 5000, 4950, NULL, '{}');
        INSERT INTO open_lots(snapshot_at, account_external_id, instrument_key, lot_index, cusip, quantity, unit_cost, cost_basis, acquired_date, unrealized_gain_loss, current_value, term, source_sha256, payload) VALUES
            (1000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 6, 100, 600, '2020-03-02', 900, 1500, 'LONG', 'sha-a', '{"cells":["Mar-02-2020","6.000"],"page":1}'),
            (1000, 'ACC1', 'VTI', 1, 'SYNCUSIP1', 4, 99.9975, 399.99, '2024-11-15', 600.01, 1000, 'SHORT', 'sha-a', '{"cells":["Nov-15-2024","4.000"],"page":1}'),
            (4000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 12, 125, 1500, '2020-03-02', 1600, 3100, 'LONG', 'sha-d', '{}'),
            (4000, 'ACC1', 'QQQ', 0, 'SYNCUSIP2', 5, 400, 2000, '2023-01-10', 500, 2500, 'LONG', 'sha-b', '{}'),
            (4000, 'ACC1', 'BND1', 0, 'BND1', 5000, 99, 4950, 'Transferred', 0, 4950, NULL, 'sha-c', '{}');
    `)
	b := allSnapshots(t, openAdapter(t, path))

	// Fetched at 1000, carried to 2000 with its quantities, costs and
	// dates; the fetch day's value and term stay in the payload.
	for _, at := range []int64{1000, 2000} {
		carried := at != 1000
		lots := lotsAt(b, at, "ACC1", "VTI")
		if len(lots) != 2 {
			t.Fatalf("VTI lots at %d = %d, want 2", at, len(lots))
		}
		l := lots[0]
		if l.LotKey != "0" || !decEq(l.Quantity, "6") || !decEq(l.BookValue, "600") ||
			l.Term != canonical.LotTermLong ||
			l.BasisOrigin != canonical.BasisStated || l.Covered != nil ||
			l.Currency != "USD" || l.InstrumentExternalID == nil || *l.InstrumentExternalID != "VTI" {
			t.Errorf("VTI lot 0 at %d = %+v", at, l)
		}
		if carried != (l.MarketValue == nil) || !carried && !decEq(l.MarketValue, "1500") {
			t.Errorf("VTI lot 0 value at %d = %v, want 1500 on the fetch's own snapshot only", at, l.MarketValue)
		}
		if l.AcquisitionDate == nil || l.AcquisitionDate.Format("2006-01-02") != "2020-03-02" {
			t.Errorf("VTI lot 0 acquired = %v, want 2020-03-02", l.AcquisitionDate)
		}
		if l.SourceDocument == nil || *l.SourceDocument != "sha-a" {
			t.Errorf("VTI lot 0 source = %v, want the lot table's sha", l.SourceDocument)
		}
		var payload map[string]any
		if err := json.Unmarshal(l.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["cusip"] != "SYNCUSIP1" || payload["unit_cost"] != 100.0 ||
			payload["unrealized_gain_loss"] != 900.0 || payload["fetched_at"] != 1000.0 ||
			payload["page"] != 1.0 || payload["cells"] == nil {
			t.Errorf("VTI lot 0 payload at %d = %v, want silver's cells with the annotations", at, payload)
		}
		if carried && (payload["current_value"] != 1500.0 || payload["term"] != "LONG") {
			t.Errorf("VTI lot 0 payload at %d = %v, want the fetch's value and term", at, payload)
		}
		if !carried && (payload["current_value"] != nil || payload["term"] != nil) {
			t.Errorf("VTI lot 0 payload at %d = %v, want value and term in their columns only", at, payload)
		}
		if lots[1].LotKey != "1" || lots[1].Term != canonical.LotTermShort {
			t.Errorf("VTI lot 1 = %+v", lots[1])
		}
		p := positionAt(t, b, at, "ACC1", "VTI")
		if p.AcquisitionDate == nil || p.AcquisitionDate.Format("2006-01-02") != "2020-03-02" {
			t.Errorf("VTI acquired at %d = %v, want its earliest lot's date", at, p.AcquisitionDate)
		}
	}

	// Bought more at 3000 with the fetch deferred: stale, no lots.
	if lots := lotsAt(b, 3000, "ACC1", "VTI"); len(lots) != 0 {
		t.Errorf("VTI lots at 3000 = %d, want none (the fetch no longer sums)", len(lots))
	}
	if p := positionAt(t, b, 3000, "ACC1", "VTI"); p.AcquisitionDate != nil {
		t.Errorf("VTI acquired at 3000 = %v, want none without lots", p.AcquisitionDate)
	}
	// Sold by 4000: silver holds a VTI lot table under that dump, but
	// the dump lists no VTI position, so none of its lots reach gold.
	for _, p := range b.Positions {
		if p.SnapshotAt == 4000 && p.PositionKey == "VTI" {
			t.Fatalf("a VTI position at 4000: %+v", p)
		}
	}
	if lots := lotsAt(b, 4000, "ACC1", "VTI"); len(lots) != 0 {
		t.Errorf("VTI lots at 4000 = %d, want none without a position", len(lots))
	}
	// A fetch that does not sum to its own dump's position.
	if lots := lotsAt(b, 4000, "ACC1", "QQQ"); len(lots) != 0 {
		t.Errorf("QQQ lots at 4000 = %d, want none (5 lots shares vs 6 held)", len(lots))
	}
	// No cost basis total: the quantity alone decides. The acquired
	// text is not a date and stays in the payload.
	bond := lotsAt(b, 4000, "ACC1", "BND1")
	if len(bond) != 1 || bond[0].AcquisitionDate != nil || bond[0].Term != "" {
		t.Fatalf("bond lots = %+v, want one undated lot with no term", bond)
	}
	var payload map[string]any
	if err := json.Unmarshal(bond[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["acquired_date"] != "Transferred" {
		t.Errorf("bond lot payload = %v, want the acquired text", payload)
	}
}

// TestCarriedLotsAge: a lot set fetched in January and carried to June
// keeps a long term, keeps a short one only while its first year is
// still running, and drops a short term it can no longer vouch for.
func TestCarriedLotsAge(t *testing.T) {
	const jan, jun = 1736467200, 1749513600 // 2025-01-10, 2025-06-10 UTC
	path, db := newFixtureSilver(t)
	addLotsSchema(t, db)
	seedSQL(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1736467200, 13, '/x/1'), (1749513600, 13, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1736467200, 'ACC1', '{}'), (1749513600, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, quantity, current_value, cost_basis_total, payload) VALUES
            (1736467200, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 11, 2200, 1100, '{}'),
            (1749513600, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 11, 2420, 1100, '{}');
        INSERT INTO open_lots(snapshot_at, account_external_id, instrument_key, lot_index, quantity, cost_basis, acquired_date, current_value, term) VALUES
            (1736467200, 'ACC1', 'VTI', 0, 5, 500, '2020-03-02', 1000, 'LONG'),
            (1736467200, 'ACC1', 'VTI', 1, 3, 300, '2024-11-15', 600, 'SHORT'),
            (1736467200, 'ACC1', 'VTI', 2, 2, 200, '2024-03-01', 400, 'SHORT'),
            (1736467200, 'ACC1', 'VTI', 3, 1, 100, 'Various', 200, 'SHORT');
    `)
	b := allSnapshots(t, openAdapter(t, path))
	for _, c := range []struct {
		at    int64
		terms []canonical.LotTerm
	}{
		{jan, []canonical.LotTerm{"long", "short", "short", "short"}},
		{jun, []canonical.LotTerm{"long", "short", "", ""}},
	} {
		lots := lotsAt(b, c.at, "ACC1", "VTI")
		if len(lots) != len(c.terms) {
			t.Fatalf("lots at %d = %d, want %d", c.at, len(lots), len(c.terms))
		}
		for i, l := range lots {
			if l.Term != c.terms[i] {
				t.Errorf("lot %s at %d: term %q, want %q", l.LotKey, c.at, l.Term, c.terms[i])
			}
			if (c.at == jun) != (l.MarketValue == nil) {
				t.Errorf("lot %s at %d: value %v, want one on the fetch's own snapshot only", l.LotKey, c.at, l.MarketValue)
			}
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(lotsAt(b, jun, "ACC1", "VTI")[2].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["term"] != "SHORT" || payload["current_value"] != 400.0 || payload["fetched_at"] != float64(jan) {
		t.Errorf("aged lot payload = %v, want the fetch's term and value", payload)
	}
}

// TestAgedTermAtTheAnniversary: a short lot is short through the day
// before its first anniversary and states no term from then on.
func TestAgedTermAtTheAnniversary(t *testing.T) {
	acquired := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	anniversary := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC).Unix()
	for _, c := range []struct {
		term     canonical.LotTerm
		acquired *time.Time
		at       int64
		want     canonical.LotTerm
	}{
		{canonical.LotTermShort, &acquired, anniversary - 1, canonical.LotTermShort},
		{canonical.LotTermShort, &acquired, anniversary, ""},
		{canonical.LotTermShort, nil, anniversary - 1, ""},
		{canonical.LotTermLong, nil, anniversary, canonical.LotTermLong},
		{"", &acquired, anniversary - 1, ""},
	} {
		if got := agedTerm(c.term, c.acquired, c.at); got != c.want {
			t.Errorf("agedTerm(%q, %v, %d) = %q, want %q", c.term, c.acquired, c.at, got, c.want)
		}
	}
}

func TestLotFetchDescribes(t *testing.T) {
	d := func(s string) *canonical.Decimal {
		v, _ := canonical.NewDecimalFromString(s)
		return &v
	}
	fetch := lotFetch{lots: []openLot{
		{quantity: d("6"), costBasis: d("600.01")},
		{quantity: d("4.0000001"), costBasis: d("400")},
	}}
	cases := []struct {
		name      string
		qty, cost *canonical.Decimal
		want      bool
	}{
		{"exact", d("10.0000001"), d("1000.01"), true},
		{"quantity within a millionth", d("10.000005"), d("1000.01"), true},
		{"quantity off", d("10.001"), d("1000.01"), false},
		{"cost within a cent per lot", d("10"), d("999.995"), true},
		{"cost off", d("10"), d("1000.05"), false},
		{"no total: quantity decides", d("10"), nil, true},
		{"no quantity", nil, d("1000"), false},
		{"short position", d("-10"), d("1000"), true},
	}
	for _, c := range cases {
		if got := fetch.describes(c.qty, c.cost); got != c.want {
			t.Errorf("%s: describes = %v, want %v", c.name, got, c.want)
		}
	}
	unpriced := lotFetch{lots: []openLot{{quantity: d("10")}}}
	if !unpriced.describes(d("10"), d("1234")) {
		t.Error("a lot with no cost leaves the cost unchecked")
	}
}

// TestRealizedLots covers the three document kinds, their primaries,
// and the CUSIP → symbol crosswalk.
func TestRealizedLots(t *testing.T) {
	path, db := newFixtureSilver(t)
	addLotsSchema(t, db)
	seedSQL(t, db, `
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, quantity, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Total Market ETF', 10, '{}'),
            (1000, 'ACC1', 'BND1', 'Treasury Note', 5000, '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, amount, payload) VALUES
            ('t1', 1000, 'ACC1', 'SELL', 'QQQ', 100, '{}');
        INSERT INTO open_lots(snapshot_at, account_external_id, instrument_key, lot_index, cusip, quantity) VALUES
            (1000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 10);
        INSERT INTO closed_lots(lot_id, document_kind, account_external_id, tax_year, form_prepared, security_name, instrument_key, cusip, action, quantity, acquired_date, disposed_date, settlement_date, proceeds, cost_basis, wash_sale_disallowed, realized_gain_loss, fees, term, covered, form_8949_box, specific_share_id, corrected, source_sha256) VALUES
            -- 2024: a 1099-B and a statement state the same two sales.
            ('f1', 'form_1099b', 'ACC1', 2024, '2025-02-14', 'TOTAL MARKET ETF', 'SYNCUSIP1', 'SYNCUSIP1', 'Sale', 3, 'Various', '2024-05-01', NULL, 900, 600, NULL, 300, NULL, 'LONG', 1, 'D', NULL, 0, 'sha-1099'),
            ('f2', 'form_1099b', 'ACC1', 2024, '2025-02-14', 'NASDAQ ETF', 'QQQ', 'SYNCUSIP2', 'Sale', 2, '2024-01-03', '2024-06-03', NULL, 1000, 1100, 50, -50, NULL, 'SHORT', 1, 'A', NULL, 1, 'sha-1099'),
            ('s1', 'statement', 'ACC1', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP2', NULL, 'You Sold', -2, NULL, NULL, '2024-06-05', 1000, 1100, NULL, -100, -0.5, 'SHORT', NULL, NULL, 1, NULL, 'sha-stmt'),
            -- 2023: only a statement, for a security nothing pairs.
            ('s2', 'statement', 'ACC1', NULL, NULL, 'OLD HOLDING INC', 'SYNCUSIP9', NULL, 'You Sold', 7, NULL, NULL, '2023-08-01', 700, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            -- 2025: the closed-positions page outranks the statement.
            ('w1', 'closed_positions', 'ACC1', 2025, NULL, 'NASDAQ ETF', 'QQQ', 'SYNCUSIP6', NULL, 1, '2024-02-01', '2025-03-03', NULL, 500, 450, NULL, 50, NULL, 'LONG', NULL, NULL, NULL, NULL, 'sha-page'),
            ('s3', 'statement', 'ACC1', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP6', NULL, 'You Sold', 1, NULL, NULL, '2025-03-05', 500, 450, NULL, 50, NULL, 'LONG', NULL, NULL, NULL, NULL, 'sha-stmt'),
            -- A 1099-B printing another symbol beside the page's CUSIP,
            -- and one CUSIP the forms pair with two symbols.
            ('f3', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'NASDAQ ETF', 'QQQX', 'SYNCUSIP6', 'Sale', 1, 'Unknown', '2024-09-09', NULL, 400, NULL, NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL, 'sha-1099'),
            ('f4', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'RENAMED CO', 'AAA', 'SYNCUSIP7', 'Sale', 1, '2020-01-01', '2024-09-10', NULL, 10, 8, NULL, 2, NULL, 'LONG', 1, 'D', NULL, NULL, 'sha-1099'),
            ('f5', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'RENAMED CO', 'BBB', 'SYNCUSIP7', 'Sale', 1, '2020-01-01', '2024-09-11', NULL, 10, 8, NULL, 2, NULL, 'LONG', 1, 'D', NULL, NULL, 'sha-1099'),
            ('s4', 'statement', 'ACC2', NULL, NULL, 'RENAMED CO', 'SYNCUSIP7', NULL, 'You Sold', 1, NULL, NULL, '2024-09-12', 10, 8, NULL, 2, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('s5', 'statement', 'ACC2', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP6', NULL, 'You Sold', 1, NULL, NULL, '2024-09-12', 400, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('s6', 'statement', 'ACC2', NULL, NULL, 'TREASURY NOTE', 'BND1', NULL, 'You Sold', 1000, NULL, NULL, '2024-10-01', 990, 1000, NULL, -10, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt');
    `)
	conn := openAdapter(t, path)
	reader, ok := conn.(silver.RealizedLotReader)
	if !ok {
		t.Fatal("the fidelity connection does not offer realized lots")
	}
	lots, err := reader.RealizedLots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]canonical.RealizedLotChange{}
	for _, r := range lots {
		byID[r.RealizedLotExternalID] = r
		if err := canonical.ValidateBookValue(r.BookValue, r.Basis); err != nil {
			t.Errorf("%s: %v", r.RealizedLotExternalID, err)
		}
	}
	if len(byID) != 12 {
		t.Fatalf("realized lots = %d, want 12", len(byID))
	}

	instr := func(id string) string {
		r := byID[id]
		if r.InstrumentExternalID == nil {
			return "hint:" + r.InstrumentHint
		}
		return *r.InstrumentExternalID
	}
	for id, want := range map[string]string{
		"f1": "VTI",            // CUSIP-keyed form row, paired by the open lots
		"f2": "QQQ",            // a symbol transactions use
		"s1": "QQQ",            // paired by the 1099-B
		"s2": "hint:SYNCUSIP9", // nothing pairs it
		"s3": "QQQ",            // paired by the page
		"s5": "QQQ",            // the page wins over the form's other symbol
		"s4": "hint:SYNCUSIP7", // the forms pair it with two symbols
		"f3": "hint:QQQX",      // a symbol nothing else uses
		"s6": "BND1",           // a bond, keyed by its CUSIP everywhere
	} {
		if got := instr(id); got != want {
			t.Errorf("%s instrument = %q, want %q", id, got, want)
		}
	}
	if d := byID["s2"].Description; d == nil || *d != "OLD HOLDING INC" {
		t.Errorf("s2 description = %v, want the printed name", d)
	}

	for id, want := range map[string]bool{
		"f1": true, "f2": true, "s1": false, // 2024: the 1099-B
		"s2": true,              // 2023: statements only
		"w1": true, "s3": false, // 2025: the page
		"f3": true, "f4": true, "f5": true, "s4": false, "s5": false, "s6": false,
	} {
		if got := byID[id].IsPrimary; got != want {
			t.Errorf("%s primary = %v, want %v", id, got, want)
		}
	}

	f1 := byID["f1"]
	if f1.DocumentKind != canonical.RealizedForm1099B || f1.TaxYear != 2024 ||
		!f1.AcquiredVarious || f1.AcquisitionDate != nil ||
		f1.DisposalDate == nil || f1.DisposalDate.Format("2006-01-02") != "2024-05-01" ||
		!decEq(f1.Proceeds, "900") || !decEq(f1.BookValue, "600") || !decEq(f1.RealizedGainLoss, "300") ||
		f1.Basis != lotBasis || f1.Term != canonical.LotTermLong ||
		f1.Covered == nil || !*f1.Covered || f1.Form8949Box == nil || *f1.Form8949Box != "D" ||
		f1.SourceDocument == nil || *f1.SourceDocument != "sha-1099" || f1.Currency != "USD" {
		t.Errorf("f1 = %+v", f1)
	}
	if f2 := byID["f2"]; !decEq(f2.WashSaleDisallowed, "50") || !decEq(f2.RealizedGainLoss, "-50") {
		t.Errorf("f2 wash/gain = %v/%v, want 50/-50", f2.WashSaleDisallowed, f2.RealizedGainLoss)
	}

	s1 := byID["s1"]
	if s1.DocumentKind != canonical.RealizedStatement || s1.TaxYear != 2024 ||
		s1.SettlementDate == nil || s1.DisposalDate != nil || s1.AcquisitionDate != nil ||
		!decEq(s1.Quantity, "2") || s1.Term != canonical.LotTermShort {
		t.Errorf("s1 = %+v", s1)
	}
	var payload map[string]any
	if err := json.Unmarshal(s1.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["fees"] != -0.5 || payload["specific_share_id"] != true || payload["action"] != "You Sold" {
		t.Errorf("s1 payload = %v, want fees, specific-share mark and action", payload)
	}

	if s2 := byID["s2"]; s2.TaxYear != 2023 || s2.BookValue != nil || !s2.Basis.IsZero() || s2.RealizedGainLoss != nil {
		t.Errorf("s2 = %+v, want 2023 with no basis and no gain", s2)
	}
	if w1 := byID["w1"]; w1.DocumentKind != canonical.RealizedClosedPositions || w1.TaxYear != 2025 || w1.Covered != nil {
		t.Errorf("w1 = %+v", w1)
	}
	if err := json.Unmarshal(byID["f3"].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["acquired_date"] != "Unknown" || byID["f3"].AcquiredVarious {
		t.Errorf("f3 payload = %v, want the acquired text", payload)
	}
}

// TestYearEndStatementSalesCountOnce: a statement sale settled in
// January that traded in December takes December's year, so the
// primaries of two years whose kinds differ count it exactly once.
func TestYearEndStatementSalesCountOnce(t *testing.T) {
	path, db := newFixtureSilver(t)
	addLotsSchema(t, db)
	seedSQL(t, db, `
        INSERT INTO closed_lots(lot_id, document_kind, account_external_id, tax_year, security_name, instrument_key, cusip, quantity, disposed_date, settlement_date, proceeds) VALUES
            -- ACC3: a 1099-B for 2024, statements only for 2025.
            ('a1', 'form_1099b', 'ACC3', 2024, 'TOTAL MARKET ETF', 'VTI', 'SYNCUSIP1', 6, '2024-12-31', NULL, 600),
            ('a2', 'form_1099b', 'ACC3', 2024, 'TOTAL MARKET ETF', 'VTI', 'SYNCUSIP1', 4, '2024-12-31', NULL, 400),
            ('a3', 'statement',  'ACC3', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 10, NULL, '2025-01-02', 1000),
            ('a4', 'statement',  'ACC3', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 7, NULL, '2025-01-06', 700),
            ('a5', 'form_1099b', 'ACC3', 2024, 'NASDAQ ETF', 'QQQ', 'SYNCUSIP2', 5, '2024-12-30', NULL, 500),
            ('a6', 'statement',  'ACC3', NULL, 'NASDAQ ETF', 'SYNCUSIP2', NULL, 2, NULL, '2025-01-02', 200),
            ('a7', 'statement',  'ACC3', NULL, 'NASDAQ ETF', 'SYNCUSIP2', NULL, 3, NULL, '2025-01-02', 300),
            -- ACC4: statements only for 2023, a 1099-B for 2024.
            ('b1', 'statement',  'ACC4', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 1, NULL, '2023-06-01', 100),
            ('b2', 'statement',  'ACC4', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 8, NULL, '2024-01-04', 800),
            ('b3', 'form_1099b', 'ACC4', 2024, 'TOTAL MARKET ETF', 'VTI', 'SYNCUSIP1', 4, '2024-01-04', NULL, 400),
            ('b4', 'statement',  'ACC4', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 4, NULL, '2024-01-05', 400),
            -- ACC5: statements only, both years.
            ('c1', 'statement',  'ACC5', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 2, NULL, '2024-12-02', 200),
            ('c2', 'statement',  'ACC5', NULL, 'TOTAL MARKET ETF', 'VTI', NULL, 3, NULL, '2025-01-02', 300);
    `)
	lots, err := openAdapter(t, path).(silver.RealizedLotReader).RealizedLots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		year    int
		primary bool
	}
	wants := map[string]want{
		"a1": {2024, true}, "a2": {2024, true},
		"a3": {2024, false}, // the form's two lots sold on 12-31
		"a4": {2025, true},  // a January trade: nothing sold 7 shares
		"a5": {2024, true},
		"a6": {2024, false}, "a7": {2024, false}, // together, the form's 5 by CUSIP
		"b1": {2023, true},
		"b2": {2023, true}, // not on the 2024 form, so a December trade
		"b3": {2024, true},
		"b4": {2024, false}, // the form's January sale
		"c1": {2024, true},
		"c2": {2025, true}, // no trade-dated document to tell: settlement year
	}
	if len(lots) != len(wants) {
		t.Fatalf("realized lots = %d, want %d", len(lots), len(wants))
	}
	for _, r := range lots {
		w := wants[r.RealizedLotExternalID]
		if r.TaxYear != w.year || r.IsPrimary != w.primary {
			t.Errorf("%s: year %d primary %v, want %d %v", r.RealizedLotExternalID, r.TaxYear, r.IsPrimary, w.year, w.primary)
		}
		if r.DocumentKind == canonical.RealizedStatement && (r.DisposalDate != nil || r.SettlementDate == nil) {
			t.Errorf("%s: dates %v/%v, want the settlement date only", r.RealizedLotExternalID, r.DisposalDate, r.SettlementDate)
		}
	}
}

// TestSvbShapedSilverStatesNoBasis: the svb silvers write this schema
// with no live positions, no lots and no cost columns filled. They load
// with no stamp, no lot and no realized lot, whether their silver
// predates the lot migrations or not.
func TestSvbShapedSilverStatesNoBasis(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		path, db := newFixtureSilver(t)
		if migrated {
			addLotsSchema(t, db)
		} else {
			// A silver at fidelity-web migration 0010: the closed lots
			// under their first names, and no open lots.
			seedSQL(t, db, `
                ALTER TABLE historical_position_snapshots ADD COLUMN cost_basis REAL;
                CREATE TABLE closed_lots (lot_id TEXT PRIMARY KEY, document_kind TEXT, description TEXT, payload TEXT);`)
		}
		seedSQL(t, db, `
            INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (3000, 10, 'svb-build');
            INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (3000, '0000000001', '{}');
            INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, market_value, payload) VALUES
                (2000, '0000000001', 'ACME CORP COM', NULL, 10, 120, '{}'),
                (2000, '0000000001', 'CASH BALANCE', NULL, NULL, 900, '{}');
        `)
		conn := openAdapter(t, path)
		b := allSnapshots(t, conn)
		if len(b.Positions) != 2 {
			t.Fatalf("migrated=%v: positions = %d, want 2", migrated, len(b.Positions))
		}
		for _, p := range b.Positions {
			if p.BookValue != nil || !p.Basis.IsZero() || p.AcquisitionDate != nil {
				t.Errorf("migrated=%v: %s book = %v %+v, want none", migrated, p.PositionKey, p.BookValue, p.Basis)
			}
		}
		if len(b.PositionLots) != 0 {
			t.Errorf("migrated=%v: lots = %d, want none", migrated, len(b.PositionLots))
		}
		lots, err := conn.(silver.RealizedLotReader).RealizedLots(context.Background())
		if err != nil || len(lots) != 0 {
			t.Errorf("migrated=%v: realized lots = %d (%v), want none", migrated, len(lots), err)
		}
	}
}

// TestASilverBeforeTheLotMigrationsStillLoads: no open_lots, no
// closed_lots, no statement cost basis column.
func TestASilverBeforeTheLotMigrationsStillLoads(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedSQL(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 8, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, quantity, current_value, cost_basis_total, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 10, 2500, 2000, '{}');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, market_value, payload) VALUES
            (500, 'ACC1', 'TOTAL MARKET ETF', 'VTI', 8, 1600, '{}');
    `)
	conn := openAdapter(t, path)
	b := allSnapshots(t, conn)
	if p := positionAt(t, b, 1000, "ACC1", "VTI"); !decEq(p.BookValue, "2000") {
		t.Errorf("live book = %v, want 2000", p.BookValue)
	}
	if p := positionAt(t, b, 500, "ACC1", "VTI"); p.BookValue != nil {
		t.Errorf("statement book = %v, want none before migration 0009", p.BookValue)
	}
	if len(b.PositionLots) != 0 {
		t.Errorf("lots = %d, want none", len(b.PositionLots))
	}
	lots, err := conn.(silver.RealizedLotReader).RealizedLots(context.Background())
	if err != nil || lots != nil {
		t.Errorf("realized lots = %v (%v), want none", lots, err)
	}
}
