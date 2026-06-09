package equityzen

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/equityzen.db"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// iso parses a calendar date to unix seconds at UTC midnight — what the
// adapter's strftime('%s', as_of_date) yields for a bare ISO date.
func iso(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("iso(%q): %v", s, err)
	}
	return tm.Unix()
}

// seed builds an event-sourced book exercising every forward-fill path:
//
//   - d1 (spv): investment (cost) -> partial tender disposition (still open),
//     with a purchase + an spv distribution in the cash ledger.
//   - d2 (private_fund): investment -> capital-account statement (NAV), with a
//     purchase + a fund distribution.
//   - d3 (spv): investment -> exit (is_open=0); drops out at its exit date.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at) VALUES (1700000000);
        INSERT INTO offerings(deal_external_id, kind, company_name, currency, payload) VALUES
            ('d1', 'spv',          'Acme SPV',  'USD', '{"deal":"d1"}'),
            ('d2', 'private_fund', 'Beta Fund', 'USD', '{"deal":"d2"}'),
            ('d3', 'spv',          'Gamma SPV', 'USD', '{"deal":"d3"}');
        INSERT INTO positions(deal_external_id, event_seq, as_of_date, event_type,
            is_open, shares_held, cost_basis_remaining, market_value) VALUES
            ('d1', 0, '2022-01-01', 'investment',  1, 100, 1000, 1000),
            ('d1', 1, '2023-01-01', 'disposition', 1,  60,  600, 1200),
            ('d2', 0, '2022-06-01', 'investment',  1, 200, 2000, 2000),
            ('d2', 1, '2023-06-01', 'statement',   1, 200, 2000, 2500),
            ('d3', 0, '2022-03-01', 'investment',  1,  50,  500,  500),
            ('d3', 1, '2024-01-01', 'exit',        0,   0,    0,    0);
        INSERT INTO cash_flows(cash_flow_external_id, deal_external_id, kind,
            flow_date, amount, shares, price_per_share, currency) VALUES
            ('cf-d1-buy',  'd1', 'purchase',     '2022-01-01', 1000, 100, 10, 'USD'),
            ('cf-d1-dist', 'd1', 'distribution', '2023-01-01',  800,  40, 20, 'USD'),
            ('cf-d2-buy',  'd2', 'purchase',     '2022-06-01', 2000, 200, 10, 'USD'),
            ('cf-d2-dist', 'd2', 'distribution', '2023-06-01',  300,   0,  0, 'USD'),
            ('cf-d3-buy',  'd3', 'purchase',     '2022-03-01',  500,  50, 10, 'USD'),
            ('cf-d3-dist', 'd3', 'distribution', '2024-01-01',    0,   0,  0, 'USD');
    `); err != nil {
		t.Fatal(err)
	}
}

func TestKindIsEquityzen(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "equityzen" {
		t.Errorf("Kind() = %q, want equityzen", got)
	}
}

func TestStatusEmpty(t *testing.T) {
	path, _ := newFixtureSilver(t)
	conn := openAdapter(t, path)
	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.Status{
		OldestSnapshotAt: -1, LatestSnapshotAt: -1,
		OldestTransactionAt: -1, LatestTransactionAt: -1,
		LatestChangeNumber: -1,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
}

func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestSnapshotAt != iso(t, "2022-01-01") || s.LatestSnapshotAt != iso(t, "2024-01-01") {
		t.Errorf("snapshot extrema = [%d,%d], want [%d,%d]",
			s.OldestSnapshotAt, s.LatestSnapshotAt, iso(t, "2022-01-01"), iso(t, "2024-01-01"))
	}
	if s.LatestChangeNumber != 1700000000 {
		t.Errorf("LatestChangeNumber = %d, want 1700000000 (the dump_run)", s.LatestChangeNumber)
	}
	// Transaction extrema track the cash-flow dates (2022-01-01 .. 2024-01-01,
	// the latter the d3 $0 exit distribution).
	if s.OldestTransactionAt != iso(t, "2022-01-01") || s.LatestTransactionAt != iso(t, "2024-01-01") {
		t.Errorf("tx extrema = [%d,%d], want [%d,%d]",
			s.OldestTransactionAt, s.LatestTransactionAt, iso(t, "2022-01-01"), iso(t, "2024-01-01"))
	}

	// Window spans both fact streams; End is the exit date (a position event
	// past the last cash flow).
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != iso(t, "2022-01-01") || w.End != iso(t, "2024-01-01") || w.NewChangeNumber != 1700000000 {
		t.Errorf("ChangeWindow(-1) = %+v, want HasChanges Start=%d End=%d NewCN=1700000000",
			w, iso(t, "2022-01-01"), iso(t, "2024-01-01"))
	}
	w2, err := conn.ChangeWindow(context.Background(), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if w2.HasChanges {
		t.Errorf("ChangeWindow(1700000000).HasChanges = true, want false (no new download)")
	}
}

func collectSnapshots(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	var batches []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, b)
		if !more {
			break
		}
	}
	return batches
}

func TestSnapshotsForwardFillPerEventDate(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	batches := collectSnapshots(t, conn, w)

	// One batch per distinct event date: 2022-01-01, 2022-03-01, 2022-06-01,
	// 2023-01-01, 2023-06-01, 2024-01-01.
	if len(batches) != 6 {
		t.Fatalf("batches = %d, want 6 distinct event dates", len(batches))
	}

	posByT := map[int64]map[string]canonical.PositionChange{}
	accts := map[string]canonical.AccountChange{}
	for i := range batches {
		for _, p := range batches[i].Positions {
			if posByT[p.SnapshotAt] == nil {
				posByT[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			posByT[p.SnapshotAt][p.PositionKey] = p
		}
		// every batch with positions must carry one instrument per position
		if n := len(batches[i].Instruments); n != len(batches[i].Positions) {
			t.Errorf("batch %d: %d instruments for %d positions", i, n, len(batches[i].Positions))
		}
		for _, a := range batches[i].Accounts {
			accts[a.AccountExternalID] = a
		}
	}

	mv := func(date, deal string) string {
		p, ok := posByT[iso(t, date)][deal]
		if !ok || p.MarketValue == nil {
			t.Fatalf("%s %s: no market value", date, deal)
		}
		return p.MarketValue.StringFixed(2)
	}

	// 2022-01-01: only d1 invested.
	if got := len(posByT[iso(t, "2022-01-01")]); got != 1 {
		t.Errorf("2022-01-01 positions = %d, want 1 (d1)", got)
	}
	if mv("2022-01-01", "d1") != "1000.00" {
		t.Errorf("2022-01-01 d1 market_value = %s, want 1000.00 (cost)", mv("2022-01-01", "d1"))
	}
	if q := posByT[iso(t, "2022-01-01")]["d1"].Quantity; q == nil || q.StringFixed(2) != "100.00" {
		t.Errorf("d1 quantity = %v, want 100.00 (spv share count)", q)
	}
	if posByT[iso(t, "2022-01-01")]["d1"].AssetClass != canonical.AssetClassSPV {
		t.Errorf("d1 asset_class = %q, want spv", posByT[iso(t, "2022-01-01")]["d1"].AssetClass)
	}

	// 2022-06-01: d1 (forward-filled to its investment), d3 (invested
	// 2022-03-01), d2 (invested today).
	at := posByT[iso(t, "2022-06-01")]
	if len(at) != 3 {
		t.Errorf("2022-06-01 positions = %d, want 3 (d1,d2,d3)", len(at))
	}
	if at["d2"].AssetClass != canonical.AssetClassPrivateFund {
		t.Errorf("d2 asset_class = %q, want private_fund", at["d2"].AssetClass)
	}
	if at["d2"].Quantity != nil {
		t.Errorf("d2 (fund) quantity = %v, want nil", at["d2"].Quantity)
	}

	// 2023-06-01: d1 forward-fills to its tender (market 1200, 60 shares),
	// d2 to its statement NAV (2500), d3 still at cost.
	if mv("2023-06-01", "d1") != "1200.00" {
		t.Errorf("2023-06-01 d1 market_value = %s, want 1200.00 (tender mark)", mv("2023-06-01", "d1"))
	}
	if q := posByT[iso(t, "2023-06-01")]["d1"].Quantity; q == nil || q.StringFixed(2) != "60.00" {
		t.Errorf("2023-06-01 d1 quantity = %v, want 60.00 (after tender)", q)
	}
	if mv("2023-06-01", "d2") != "2500.00" {
		t.Errorf("2023-06-01 d2 market_value = %s, want 2500.00 (statement NAV)", mv("2023-06-01", "d2"))
	}

	// 2024-01-01: d3's latest event is its exit (is_open=0) → dropped.
	end := posByT[iso(t, "2024-01-01")]
	if len(end) != 2 {
		t.Errorf("2024-01-01 positions = %d, want 2 (d1,d2; d3 exited)", len(end))
	}
	if _, ok := end["d3"]; ok {
		t.Error("2024-01-01 still contains exited d3")
	}

	// Acquisition date is the deal's first event.
	if ad := posByT[iso(t, "2024-01-01")]["d1"].AcquisitionDate; ad == nil || ad.Format("2006-01-02") != "2022-01-01" {
		t.Errorf("d1 acquisition_date = %v, want 2022-01-01", ad)
	}

	// Two accounts: the custody account (positions) and the sentinel funding
	// cash account (the transaction pairs).
	if a, ok := accts["equityzen"]; !ok || a.AccountKind != canonical.AccountKindCustody {
		t.Errorf("custody account = %+v (ok=%v), want kind custody", a, ok)
	}
	if a, ok := accts[fundingAccountKey]; !ok || a.AccountKind != canonical.AccountKindCash {
		t.Errorf("funding account = %+v (ok=%v), want kind cash", a, ok)
	}
}

// TestTransactions verifies the double-entry funding-account model: each cash
// flow becomes a balanced pair (deposit+buy / deposit+contribution /
// sell+withdrawal / distribution+withdrawal), every leg sits on the sentinel
// funding account and links to its instrument, a $0 distribution omits the $0
// withdrawal, and the whole ledger nets to exactly 0.
func TestTransactions(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// d1 spv: deposit+buy, sell+withdrawal. d2 fund: deposit+contribution,
	// distribution+withdrawal. d3 spv: deposit+buy, then a $0 sell (withdrawal
	// omitted). 2+2 + 2+2 + 2+1 = 11.
	if len(batch.Transactions) != 11 {
		t.Fatalf("transactions = %d, want 11", len(batch.Transactions))
	}

	byID := map[string]canonical.TransactionChange{}
	sum := canonical.NewDecimalFromInt(0)
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.AccountExternalID != fundingAccountKey {
			t.Errorf("%s account = %q, want %q", tx.TransactionExternalID, tx.AccountExternalID, fundingAccountKey)
		}
		if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID == "" {
			t.Errorf("%s has no instrument link", tx.TransactionExternalID)
		}
		if tx.NetAmount != nil {
			sum = sum.Add(*tx.NetAmount)
		}
	}
	// The sentinel invariant: the funding account's derived balance is 0.
	if !sum.IsZero() {
		t.Errorf("funding ledger nets to %s, want 0.00", sum.StringFixed(2))
	}

	want := func(id, deal string, kind canonical.TxKind, net string) canonical.TransactionChange {
		tx, ok := byID[id]
		if !ok {
			t.Fatalf("missing transaction %q", id)
		}
		if tx.Kind != kind {
			t.Errorf("%s kind = %q, want %q", id, tx.Kind, kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.StringFixed(2) != net {
			t.Errorf("%s net = %v, want %s", id, tx.NetAmount, net)
		}
		if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != deal {
			t.Errorf("%s instrument = %v, want %s", id, tx.InstrumentExternalID, deal)
		}
		return tx
	}

	// SPV purchase → deposit (+) + buy (−, with lot).
	want("cf-d1-buy:deposit", "d1", canonical.TxKindDeposit, "1000.00")
	buy := want("cf-d1-buy:buy", "d1", canonical.TxKindBuy, "-1000.00")
	if buy.Quantity == nil || buy.Quantity.StringFixed(2) != "100.00" || buy.Price == nil || buy.Price.StringFixed(2) != "10.00" {
		t.Errorf("buy lot = %v @ %v, want 100.00 @ 10.00", buy.Quantity, buy.Price)
	}
	// SPV distribution → sell (+, with lot) + withdrawal (−).
	sell := want("cf-d1-dist:sell", "d1", canonical.TxKindSell, "800.00")
	if sell.Quantity == nil || sell.Quantity.StringFixed(2) != "40.00" || sell.Price == nil || sell.Price.StringFixed(2) != "20.00" {
		t.Errorf("sell lot = %v @ %v, want 40.00 @ 20.00", sell.Quantity, sell.Price)
	}
	want("cf-d1-dist:withdrawal", "d1", canonical.TxKindWithdrawal, "-800.00")

	// Fund purchase → deposit (+) + contribution (−, no lot). Fund distribution
	// → distribution (+, no lot) + withdrawal (−).
	want("cf-d2-buy:deposit", "d2", canonical.TxKindDeposit, "2000.00")
	contrib := want("cf-d2-buy:contribution", "d2", canonical.TxKindContribution, "-2000.00")
	if contrib.Quantity != nil || contrib.Price != nil {
		t.Errorf("contribution lot = %v/%v, want nil/nil", contrib.Quantity, contrib.Price)
	}
	dist := want("cf-d2-dist:distribution", "d2", canonical.TxKindDistribution, "300.00")
	if dist.Quantity != nil || dist.Price != nil {
		t.Errorf("fund distribution lot = %v/%v, want nil/nil", dist.Quantity, dist.Price)
	}
	want("cf-d2-dist:withdrawal", "d2", canonical.TxKindWithdrawal, "-300.00")

	// $0 exit (a defunct SPV): the $0 sell is kept, the $0 withdrawal is omitted.
	want("cf-d3-dist:sell", "d3", canonical.TxKindSell, "0.00")
	if _, ok := byID["cf-d3-dist:withdrawal"]; ok {
		t.Error("a $0 withdrawal was emitted for the $0 exit; it must be omitted")
	}
}
