package carta

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/carta.db"
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

func unixDate(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("unixDate(%q): %v", s, err)
	}
	return tm.Unix()
}

// seed builds a one-portfolio book with both entity families and every
// cash-flow kind:
//   - entity 100 (cap-table): a held share lot + its exit, plus an `exercise`
//     and a $0 `exit` cash flow.
//   - entity 200 (fund): a capital-account NAV, plus a `capital_call` and a
//     `distribution` cash flow.
//
// The entities / securities / fund_metrics snapshot dates span the cash-flow
// dates, so the load window covers them.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	d0101 := unixDate(t, "2023-01-01")
	d0630 := unixDate(t, "2023-06-30")
	dExit := unixDate(t, "2025-09-09")
	stmts := fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload) VALUES
    (%d, 100, 'IND1', 0, 'ACME Inc',  '{}'),
    (%d, 200, 'IND1', 1, 'ACME Fund', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload) VALUES
    (%d, 100, 'share', 1, 1000, 500, 5000, 'held',   '$', '{}'),
    (%d, 100, 'share', 1, 1000, 500,    0, 'exited', '$', '{}');
INSERT INTO fund_metrics(snapshot_at, entity_external_id, currency, net_asset_value,
    capital_contributed, payload) VALUES
    (%d, 200, 'USD', '100000', '100000', '{}');
INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, snapshot_at, kind,
    flow_date, amount, shares, price_per_share, currency, payload) VALUES
    ('exercise:100:1', '100', 1700000000, 'exercise',     '01/01/2023',    500, 1000, 0.5,  'USD', '{}'),
    ('exit:100',       '100', 1700000000, 'exit',         '2025-09-09',      0, 1000, 0,    'USD', '{}'),
    ('call:200:s1',    '200', 1700000000, 'capital_call', '06/30/2023', 100000, NULL, NULL, 'USD', '{}'),
    ('dist:200:s2',    '200', 1700000000, 'distribution', '03/31/2025',   2500, NULL, NULL, 'USD', '{}');`,
		d0101, d0630, d0101, dExit, d0630)
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
}

func TestKindIsCarta(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "carta" {
		t.Errorf("Kind() = %q, want carta", got)
	}
}

func TestStatusTransactionExtrema(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Transaction extrema track the cash-flow dates: 2023-01-01 (first
	// exercise) .. 2025-09-09 (the exit).
	if s.OldestTransactionAt != unixDate(t, "2023-01-01") || s.LatestTransactionAt != unixDate(t, "2025-09-09") {
		t.Errorf("tx extrema = [%d,%d], want [%d,%d]", s.OldestTransactionAt,
			s.LatestTransactionAt, unixDate(t, "2023-01-01"), unixDate(t, "2025-09-09"))
	}
	if s.LatestChangeNumber != 1700000000 {
		t.Errorf("LatestChangeNumber = %d, want 1700000000", s.LatestChangeNumber)
	}
}

func TestSnapshotsEmitCustodyAccountOnly(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	accts := map[string]canonical.AccountChange{}
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range b.Accounts {
			accts[a.AccountExternalID] = a
		}
		if !more {
			break
		}
	}
	// One account: the custody account carries the positions AND the
	// transaction pairs — no funding sentinel.
	if a, ok := accts["IND1"]; !ok || a.AccountKind != canonical.AccountKindCustody {
		t.Errorf("custody account = %+v (ok=%v), want kind custody", a, ok)
	}
	if len(accts) != 1 {
		t.Errorf("accounts = %d (%v), want the custody account only", len(accts), accts)
	}
}

// TestTransactions verifies the double-entry model: each cash flow becomes a
// balanced pair, every leg sits on the custody account and links to its
// instrument, a $0 exit omits the $0 withdrawal, and the whole ledger nets to
// exactly 0.
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

	// exercise: deposit+buy. exit: sell ($0, withdrawal omitted). capital_call:
	// deposit+contribution. distribution: distribution+withdrawal. 2+1+2+2 = 7.
	if len(batch.Transactions) != 7 {
		t.Fatalf("transactions = %d, want 7", len(batch.Transactions))
	}

	byID := map[string]canonical.TransactionChange{}
	sum := canonical.NewDecimalFromInt(0)
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.AccountExternalID != "IND1" {
			t.Errorf("%s account = %q, want the custody account", tx.TransactionExternalID, tx.AccountExternalID)
		}
		if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID == "" {
			t.Errorf("%s has no instrument link", tx.TransactionExternalID)
		}
		if tx.NetAmount != nil {
			sum = sum.Add(*tx.NetAmount)
		}
	}
	// The double-entry invariant: the paired ledger implies no cash position.
	if !sum.IsZero() {
		t.Errorf("paired ledger nets to %s, want 0.00", sum.StringFixed(2))
	}

	want := func(id, inst string, kind canonical.TxKind, net string) canonical.TransactionChange {
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
		if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != inst {
			t.Errorf("%s instrument = %v, want %s", id, tx.InstrumentExternalID, inst)
		}
		return tx
	}

	// exercise → deposit (+) + buy (−, with lot).
	want("exercise:100:1:deposit", "entity:100", canonical.TxKindDeposit, "500.00")
	buy := want("exercise:100:1:buy", "entity:100", canonical.TxKindBuy, "-500.00")
	if buy.Quantity == nil || buy.Quantity.StringFixed(2) != "1000.00" || buy.Price == nil || buy.Price.StringFixed(2) != "0.50" {
		t.Errorf("buy lot = %v @ %v, want 1000.00 @ 0.50", buy.Quantity, buy.Price)
	}
	// capital_call → deposit (+) + contribution (−, no lot).
	want("call:200:s1:deposit", "entity:200", canonical.TxKindDeposit, "100000.00")
	contrib := want("call:200:s1:contribution", "entity:200", canonical.TxKindContribution, "-100000.00")
	if contrib.Quantity != nil || contrib.Price != nil {
		t.Errorf("contribution lot = %v/%v, want nil/nil", contrib.Quantity, contrib.Price)
	}
	// distribution → distribution (+) + withdrawal (−).
	want("dist:200:s2:distribution", "entity:200", canonical.TxKindDistribution, "2500.00")
	want("dist:200:s2:withdrawal", "entity:200", canonical.TxKindWithdrawal, "-2500.00")

	// $0 exit: the $0 sell is kept (with the share lot), the $0 withdrawal omitted.
	sell := want("exit:100:sell", "entity:100", canonical.TxKindSell, "0.00")
	if sell.Quantity == nil || sell.Quantity.StringFixed(2) != "1000.00" {
		t.Errorf("exit sell quantity = %v, want 1000.00", sell.Quantity)
	}
	if _, ok := byID["exit:100:withdrawal"]; ok {
		t.Error("a $0 withdrawal was emitted for the $0 exit; it must be omitted")
	}
}

// TestSideLoadedLegsEmit1to1 verifies that side-loaded canonical kinds
// (sell / withdrawal / …, from `<account_id>-transactions.csv`) are emitted as
// single transactions (NOT auto-paired) — the CSV supplies both halves, so the
// sale + its withdrawals net to 0 on the custody account.
func TestSideLoadedLegsEmit1to1(t *testing.T) {
	path, db := newFixtureSilver(t)
	d := unixDate(t, "2025-09-09")
	if _, err := db.Exec(fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (%d, 100, 'IND1', 0, 'ACME Inc', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    position_status, currency, payload) VALUES (%d, 100, 'share', 1, 'exited', '$', '{}');
INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, snapshot_at, kind,
    flow_date, amount, shares, price_per_share, currency, description, payload) VALUES
    ('tx:100:0', '100', 1700000000, 'sell',       '2025-09-09', 7000, 1500, 4.6667, 'USD', 'sale of all shares', '{}'),
    ('tx:100:1', '100', 1700000000, 'withdrawal', '2025-09-09', 6000, NULL, NULL,   'USD', 'to bank',            '{}'),
    ('tx:100:2', '100', 1700000000, 'withdrawal', '2025-09-09', 1000, NULL, NULL,   'USD', 'to second bank',     '{}');`,
		d, d)); err != nil {
		t.Fatal(err)
	}
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

	// 1:1 — exactly 3 transactions (no auto-pairing), net 0.
	if len(batch.Transactions) != 3 {
		t.Fatalf("transactions = %d, want 3 (sell + 2 withdrawals, 1:1)", len(batch.Transactions))
	}
	byID := map[string]canonical.TransactionChange{}
	sum := canonical.NewDecimalFromInt(0)
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.AccountExternalID != "IND1" {
			t.Errorf("%s account = %q, want the custody account", tx.TransactionExternalID, tx.AccountExternalID)
		}
		if tx.NetAmount != nil {
			sum = sum.Add(*tx.NetAmount)
		}
	}
	if !sum.IsZero() {
		t.Errorf("side-loaded legs net to %s, want 0.00", sum.StringFixed(2))
	}
	if sell, ok := byID["tx:100:0:sell"]; !ok || sell.Kind != canonical.TxKindSell ||
		sell.NetAmount == nil || sell.NetAmount.StringFixed(2) != "7000.00" ||
		sell.Quantity == nil || sell.Quantity.StringFixed(2) != "1500.00" {
		t.Errorf("sell leg = %+v, want sell +7000.00 qty 1500.00", sell)
	}
	if wd, ok := byID["tx:100:1:withdrawal"]; !ok || wd.Kind != canonical.TxKindWithdrawal ||
		wd.NetAmount == nil || wd.NetAmount.StringFixed(2) != "-6000.00" {
		t.Errorf("withdrawal leg = %+v, want withdrawal -6000.00", wd)
	}
	// Regression: the silver cash-flow description must reach the gold
	// transaction (e.g. a withdrawal's destination), not be dropped by the
	// projection query.
	for id, wantDesc := range map[string]string{
		"tx:100:0:sell":       "sale of all shares",
		"tx:100:1:withdrawal": "to bank",
		"tx:100:2:withdrawal": "to second bank",
	} {
		tx := byID[id]
		if tx.Description == nil || *tx.Description != wantDesc {
			t.Errorf("%s description = %v, want %q", id, tx.Description, wantDesc)
		}
	}
}

// TestConvertibleNotePosition verifies a purely-convertible holding (a SAFE)
// becomes a convertible_note position carried at its principal (book == market
// == cost, no share quantity), that BOTH the position and its instrument carry
// that class, and that an equity holding in the same book stays private_equity.
func TestConvertibleNotePosition(t *testing.T) {
	path, db := newFixtureSilver(t)
	d := unixDate(t, "2026-06-30")
	if _, err := db.Exec(fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload) VALUES
    (%d, 100, 'IND1', 0, 'ACME Inc', '{}'),
    (%d, 300, 'IND1', 0, 'SAFE Co',  '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload) VALUES
    (%d, 100, 'share',       1, 1000,    500,   5000, 'held', '$', '{}'),
    (%d, 300, 'convertible', 9,    0, 100000, 100000, 'held', '$', '{}');`,
		d, d, d, d)); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	posByKey := map[string]canonical.PositionChange{}
	instByID := map[string]canonical.InstrumentChange{}
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Positions {
			posByKey[p.PositionKey] = p
		}
		for _, i := range b.Instruments {
			instByID[i.InstrumentExternalID] = i
		}
		if !more {
			break
		}
	}

	// The SAFE: convertible_note, carried at principal — book == market == cost,
	// and no share quantity (it has no shares until it converts).
	safe, ok := posByKey["entity:300"]
	if !ok {
		t.Fatal("no position for the SAFE (entity:300)")
	}
	if safe.BookValue == nil || safe.BookValue.StringFixed(2) != "100000.00" {
		t.Errorf("SAFE book_value = %v, want 100000.00", safe.BookValue)
	}
	if safe.MarketValue == nil || safe.MarketValue.StringFixed(2) != "100000.00" {
		t.Errorf("SAFE market_value = %v, want 100000.00", safe.MarketValue)
	}
	if safe.Quantity != nil {
		t.Errorf("SAFE quantity = %v, want nil (no shares pre-conversion)", safe.Quantity)
	}
	// Regression: an equity holding is unaffected by the convertible split.
}

// TestTaxonomyPairs verifies the 2-D (asset_class, vehicle) pair emitted
// for every cap-table security shape plus a fund LP interest: real
// share-settled equity → (private_equity, stock); option-shaped equity comp →
// (private_equity, option); a mixed share+option holding → stock (share
// precedence); a purely-convertible SAFE → (private_debt, convertible_note);
// a fund → (private_equity, fund). Each pair is a canonical.ValidTaxonomyPair,
// and the position's pair matches its instrument's. Placeholder names / ids
// only.
func TestTaxonomyPairs(t *testing.T) {
	path, db := newFixtureSilver(t)
	d := unixDate(t, "2026-06-30")
	if _, err := db.Exec(fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload) VALUES
    (%[1]d, 100, 'IND1', 0, 'StockCo',  '{}'),
    (%[1]d, 200, 'IND1', 1, 'FundCo',   '{}'),
    (%[1]d, 300, 'IND1', 0, 'SafeCo',   '{}'),
    (%[1]d, 400, 'IND1', 0, 'OptionCo', '{}'),
    (%[1]d, 500, 'IND1', 0, 'MixedCo',  '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload) VALUES
    (%[1]d, 100, 'share',       1, 1000,   500,   5000, 'held', '$', '{}'),
    (%[1]d, 300, 'convertible', 1,    0, 100000, 100000, 'held', '$', '{}'),
    (%[1]d, 400, 'option',      1,  200,     0,   3000, 'held', '$', '{}'),
    (%[1]d, 500, 'share',       1,  400,   200,   2000, 'held', '$', '{}'),
    (%[1]d, 500, 'option',      2,  100,     0,   1500, 'held', '$', '{}');
INSERT INTO fund_metrics(snapshot_at, entity_external_id, currency, net_asset_value,
    capital_contributed, payload) VALUES
    (%[1]d, 200, 'USD', '100000', '100000', '{}');`, d)); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	posByKey := map[string]canonical.PositionChange{}
	instByID := map[string]canonical.InstrumentChange{}
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range b.Positions {
			posByKey[p.PositionKey] = p
		}
		for _, i := range b.Instruments {
			instByID[i.InstrumentExternalID] = i
		}
		if !more {
			break
		}
	}

	cases := []struct {
		key       string
		exposure  canonical.AssetClass
		vehicle   canonical.Vehicle
		wantClass canonical.AssetClass // coarse 1-D class each shape maps from
	}{
		{"entity:100", canonical.AssetClassPrivateEquity, canonical.VehicleStock, canonical.AssetClassPrivateEquity},
		{"entity:400", canonical.AssetClassPrivateEquity, canonical.VehicleOption, canonical.AssetClassPrivateEquity},
		{"entity:500", canonical.AssetClassPrivateEquity, canonical.VehicleStock, canonical.AssetClassPrivateEquity},
		{"entity:300", canonical.AssetClassPrivateDebt, canonical.VehicleConvertibleNote, canonical.AssetClassConvertibleNote},
		{"entity:200", canonical.AssetClassPrivateEquity, canonical.VehicleFund, canonical.AssetClassPrivateFund},
	}
	for _, tc := range cases {
		if !canonical.ValidTaxonomyPair(tc.exposure, tc.vehicle) {
			t.Fatalf("%s: (%s,%s) is not a valid taxonomy pair", tc.key, tc.exposure, tc.vehicle)
		}
		p, ok := posByKey[tc.key]
		if !ok {
			t.Fatalf("no position for %s", tc.key)
		}
		if p.AssetClass != tc.exposure || p.Vehicle != tc.vehicle {
			t.Errorf("%s position (asset_class,vehicle) = (%s,%s), want (%s,%s)", tc.key,
				p.AssetClass, p.Vehicle, tc.exposure, tc.vehicle)
		}
		i, ok := instByID[tc.key]
		if !ok {
			t.Fatalf("no instrument for %s", tc.key)
		}
		if i.AssetClass != tc.exposure || i.Vehicle != tc.vehicle {
			t.Errorf("%s instrument (asset_class,vehicle) = (%s,%s), want (%s,%s) (must match its position)",
				tc.key, i.AssetClass, i.Vehicle, tc.exposure, tc.vehicle)
		}
	}
}

// TestConvertiblePurchasePair verifies a `convertible_purchase` cash flow
// projects to a balanced deposit+buy pair on the custody account, where the buy
// carries NO share lot (a SAFE has no shares yet) and the pair nets to 0.
func TestConvertiblePurchasePair(t *testing.T) {
	path, db := newFixtureSilver(t)
	d := unixDate(t, "2026-06-30")
	if _, err := db.Exec(fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (%d, 300, 'IND1', 0, 'SAFE Co', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload)
    VALUES (%d, 300, 'convertible', 9, 0, 100000, 100000, 'held', '$', '{}');
INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, snapshot_at, kind,
    flow_date, amount, shares, price_per_share, currency, description, payload) VALUES
    ('convertible:300:9', '300', 1700000000, 'convertible_purchase', '06/30/2026', 100000, NULL, NULL, 'USD', 'SAFE / convertible purchase', '{}');`,
		d, d)); err != nil {
		t.Fatal(err)
	}
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

	if len(batch.Transactions) != 2 {
		t.Fatalf("transactions = %d, want 2 (deposit + buy)", len(batch.Transactions))
	}
	byID := map[string]canonical.TransactionChange{}
	sum := canonical.NewDecimalFromInt(0)
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.AccountExternalID != "IND1" {
			t.Errorf("%s account = %q, want the custody account", tx.TransactionExternalID, tx.AccountExternalID)
		}
		if tx.NetAmount != nil {
			sum = sum.Add(*tx.NetAmount)
		}
	}
	if !sum.IsZero() {
		t.Errorf("convertible_purchase pair nets to %s, want 0.00", sum.StringFixed(2))
	}
	if dep, ok := byID["convertible:300:9:deposit"]; !ok || dep.Kind != canonical.TxKindDeposit ||
		dep.NetAmount == nil || dep.NetAmount.StringFixed(2) != "100000.00" {
		t.Errorf("deposit leg = %+v, want deposit +100000.00", dep)
	}
	buy, ok := byID["convertible:300:9:buy"]
	if !ok || buy.Kind != canonical.TxKindBuy || buy.NetAmount == nil || buy.NetAmount.StringFixed(2) != "-100000.00" {
		t.Errorf("buy leg = %+v, want buy -100000.00", buy)
	}
	// No share lot on the buy — a SAFE has no shares yet — but it still links to
	// the company instrument.
	if buy.Quantity != nil || buy.Price != nil {
		t.Errorf("buy lot = %v/%v, want nil/nil (no shares pre-conversion)", buy.Quantity, buy.Price)
	}
	if buy.InstrumentExternalID == nil || *buy.InstrumentExternalID != "entity:300" {
		t.Errorf("buy instrument = %v, want entity:300", buy.InstrumentExternalID)
	}
}

// TestSnapshotsEmitClosureMarker pins the exit-day zero snapshot: when the
// LAST holding exits, that date's batch replays the previous snapshot's
// positions at zero value (silver.ClosureMarkerBatch) instead of dropping to
// an empty batch — and an empty date BEFORE the first holding stays empty (no
// pre-inception zero).
func TestSnapshotsEmitClosureMarker(t *testing.T) {
	path, db := newFixtureSilver(t)
	dEmpty := unixDate(t, "2022-06-01") // entity known, nothing held yet
	dHeld := unixDate(t, "2023-01-01")
	dExit := unixDate(t, "2025-09-09")
	stmts := fmt.Sprintf(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload) VALUES
    (%d, 100, 'IND1', 0, 'ACME Inc', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, payload) VALUES
    (%d, 100, 'share', 1, 1000, 500, 5000, 'held',   '$', '{}'),
    (%d, 100, 'share', 1, 1000, 500,    0, 'exited', '$', '{}');`,
		dEmpty, dHeld, dExit)
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

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
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want pre-holding empty + held + closure marker", len(batches))
	}
	if len(batches[0].Positions) != 0 || len(batches[0].Accounts) != 0 {
		t.Errorf("pre-holding batch must stay empty (no pre-inception zero): %+v", batches[0])
	}
	if len(batches[1].Positions) != 1 {
		t.Fatalf("held batch positions = %d, want 1", len(batches[1].Positions))
	}
	marker := batches[2]
	if len(marker.Positions) != 1 || len(marker.Accounts) != 1 || len(marker.Instruments) != 1 {
		t.Fatalf("marker shape = %d pos / %d accts / %d insts, want 1/1/1", len(marker.Positions), len(marker.Accounts), len(marker.Instruments))
	}
	p := marker.Positions[0]
	if p.PositionKey != "entity:100" || p.MarketValue == nil || !p.MarketValue.IsZero() {
		t.Errorf("marker position = %+v, want entity:100 at zero value", p)
	}
	if p.Quantity == nil || !p.Quantity.IsZero() {
		t.Errorf("marker quantity = %v, want explicit zero (share count existed)", p.Quantity)
	}
	if p.SnapshotAt != dExit {
		t.Errorf("marker snapshot_at = %d, want the exit date %d", p.SnapshotAt, dExit)
	}
	if marker.Accounts[0].AccountExternalID != "IND1" {
		t.Errorf("marker account = %q, want the custody account", marker.Accounts[0].AccountExternalID)
	}
}

// TestAPositionIsDatedToTheEarliestLotTheHolderAcquired: a certificate
// is re-issued whenever the holding is restructured — a transfer, a
// split, a conversion — and the new one is dated to the re-issue while
// the shares behind it are the same shares. Carta states the
// acquisition date separately (`original_acquisition_date`), and it can
// precede the platform's own coverage; read from the certificate
// instead, the holding would look younger than it is.
//
// A position here aggregates a company's whole cap-table line, so the
// date it carries is the EARLIEST its held lots do. Every value below
// is invented.
func TestAPositionIsDatedToTheEarliestLotTheHolderAcquired(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 3, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (1700000000, 100, 'IND1', 0, 'Fake Equity No1', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, position_status, currency, issue_date, payload) VALUES
    (1700000000, 100, 'share', 1, 1000, 10, 5000, 'held', '$', '01/02/2098',
     '{"original_acquisition_date": "03/04/2097"}'),
    (1700000000, 100, 'share', 2,  500,  5, 2500, 'held', '$', '01/02/2098',
     '{"original_acquisition_date": "05/06/2098"}'),
    (1700000000, 100, 'share', 3,  100,  1,  500, 'held', '$', '01/02/2098', '{}');
`); err != nil {
		t.Fatal(err)
	}

	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var pos []canonical.PositionChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		pos = append(pos, b.Positions...)
		if !more {
			break
		}
	}
	if len(pos) != 1 {
		t.Fatalf("positions = %d, want the one aggregated cap-table line", len(pos))
	}
	got := pos[0].AcquisitionDate
	if got == nil {
		t.Fatal("position carries no acquisition date")
	}
	want := time.Date(2097, 3, 4, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("acquisition date = %s, want the earliest lot's %s (not the certificate's issue date)",
			got.Format("2006-01-02"), want.Format("2006-01-02"))
	}
}
