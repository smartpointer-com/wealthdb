package swissquote

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swissquote.db")
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
	conn, err := (&Adapter{}).Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestKindIsSwissquote(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "swissquote" {
		t.Errorf("Kind() = %q, want %q", got, "swissquote")
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

func TestSnapshotsPositionsAndInstruments(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, '1234567', '{"customer_id":"1234567"}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload) VALUES
            (1000, '1234567', 'IUSQ', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"IUSQ","quantity":10,"total_value":1500.00,"price":150.00}'),
            (1000, '1234567', 'XS1234567890', 'CHF',
             '{"asset_class":"Bonds","currency":"CHF","symbol":"XS1234567890","quantity":100000,"total_value":98500.00,"price":98.5}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Positions) != 2 {
		t.Fatalf("positions = %d, want 2", len(batch.Positions))
	}
	if len(batch.Instruments) != 2 {
		t.Fatalf("instruments = %d, want 2", len(batch.Instruments))
	}

	byKey := map[string]canonical.PositionChange{}
	for _, p := range batch.Positions {
		byKey[p.PositionKey] = p
	}
	if byKey["IUSQ@USD"].AssetClass != canonical.AssetClassETF {
		t.Errorf("ETFs → %q, want etf", byKey["IUSQ@USD"].AssetClass)
	}
	if byKey["XS1234567890@CHF"].AssetClass != canonical.AssetClassBond {
		t.Errorf("Bonds → %q, want bond", byKey["XS1234567890@CHF"].AssetClass)
	}
	if byKey["IUSQ@USD"].Quantity == nil || byKey["IUSQ@USD"].Quantity.String() != "10" {
		t.Errorf("IUSQ quantity = %v, want 10", byKey["IUSQ@USD"].Quantity)
	}
	if byKey["IUSQ@USD"].MarketValue == nil || byKey["IUSQ@USD"].MarketValue.String() != "1500" {
		t.Errorf("IUSQ market_value = %v, want 1500", byKey["IUSQ@USD"].MarketValue)
	}
}

func TestSnapshotsCurrencyBalancesAndFxRates(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO currency_balances(snapshot_at, account_external_id, currency, payload) VALUES
            (1000, '1234567', 'CHF', '{"cash_balance":5000.00,"rate_to_chf":1.0,"currency":"CHF"}'),
            (1000, '1234567', 'USD', '{"cash_balance":100.00,"rate_to_chf":0.7862,"currency":"USD"}'),
            (1000, '1234567', 'EUR', '{"cash_balance":0.00,"rate_to_chf":0.914,"currency":"EUR"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.CashBalances) != 3 {
		t.Fatalf("cash_balances = %d, want 3", len(batch.CashBalances))
	}
	if len(batch.FxRates) != 2 {
		t.Fatalf("fx_rates = %d, want 2 (CHF skipped as 1.0)", len(batch.FxRates))
	}

	byCcy := map[string]canonical.FxRateChange{}
	for _, r := range batch.FxRates {
		byCcy[r.QuoteCurrency] = r
	}
	if got := byCcy["USD"].MidRate.String(); got != "0.7862" {
		t.Errorf("USD rate = %q, want 0.7862", got)
	}
	if byCcy["USD"].BaseCurrency != "CHF" {
		t.Errorf("USD base = %q, want CHF", byCcy["USD"].BaseCurrency)
	}
}

func TestKindMapping(t *testing.T) {
	cases := []struct {
		txType string
		want   canonical.TxKind
	}{
		{"Buy", canonical.TxKindBuy},
		{"Sell", canonical.TxKindSell},
		{"Dividend", canonical.TxKindDividend},
		{"Coupon", canonical.TxKindCoupon},
		{"Capital Gain", canonical.TxKindCapitalGain},
		{"Custody Fees", canonical.TxKindFee},
		{"Fees Tax Statement", canonical.TxKindFee},
		{"Interest on deposits", canonical.TxKindInterest},
		{"Payment", canonical.TxKindDeposit},
		{"Debit", canonical.TxKindWithdrawal},
		{"Whatever", canonical.TxKindOther},
	}
	for _, c := range cases {
		if got := kindFor(c.txType); got != c.want {
			t.Errorf("kindFor(%q) = %q, want %q", c.txType, got, c.want)
		}
	}
}

func TestTransactionsSyntheticID(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(account_external_id, occurred_at, transaction_type, isin, symbol, currency, net_amount, payload) VALUES
            ('1234567', 1500, 'Buy', 'IE00BJK9H753', 'IUSQ', 'USD', -1500.00,
             '{"name":"iShares Core MSCI World","quantity":10,"unit_price":150.00}'),
            ('1234567', 1700, 'Dividend', 'IE00BJK9H753', 'IUSQ', 'USD', 25.00,
             '{"name":"iShares Core MSCI World"}'),
            ('1234567', 1800, 'Debit', NULL, NULL, 'CHF', -200.00,
             '{"name":"Outgoing wire"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Transactions) != 3 {
		t.Fatalf("transactions = %d, want 3", len(batch.Transactions))
	}

	// IDs are deterministic: re-running should produce the same.
	idSet := map[string]struct{}{}
	for _, tx := range batch.Transactions {
		if _, dup := idSet[tx.TransactionExternalID]; dup {
			t.Errorf("duplicate synthetic ID %q", tx.TransactionExternalID)
		}
		idSet[tx.TransactionExternalID] = struct{}{}
	}

	// Buy: ISIN + qty/price extracted.
	buy := batch.Transactions[0]
	if buy.Kind != canonical.TxKindBuy {
		t.Errorf("buy kind = %q", buy.Kind)
	}
	if buy.InstrumentExternalID == nil || *buy.InstrumentExternalID != "IE00BJK9H753" {
		t.Errorf("buy isin = %v", buy.InstrumentExternalID)
	}
	if buy.Quantity == nil || buy.Quantity.String() != "10" {
		t.Errorf("buy qty = %v", buy.Quantity)
	}
	if buy.Price == nil || buy.Price.String() != "150" {
		t.Errorf("buy price = %v", buy.Price)
	}

	// Debit: no ISIN/symbol → InstrumentExternalID nil; kind=withdrawal.
	debit := batch.Transactions[2]
	if debit.Kind != canonical.TxKindWithdrawal {
		t.Errorf("debit kind = %q", debit.Kind)
	}
	if debit.InstrumentExternalID != nil {
		t.Errorf("debit instrument = %v, want nil", debit.InstrumentExternalID)
	}
}

func TestSyntheticIDStable(t *testing.T) {
	a := syntheticTxID("ACC", 1000, "Buy", "AAPL", "USD", canonical.NewDecimalFromInt(100))
	b := syntheticTxID("ACC", 1000, "Buy", "AAPL", "USD", canonical.NewDecimalFromInt(100))
	if a != b {
		t.Errorf("syntheticTxID is non-deterministic: %q vs %q", a, b)
	}

	c := syntheticTxID("ACC", 1000, "Buy", "AAPL", "USD", canonical.NewDecimalFromInt(101))
	if a == c {
		t.Errorf("differing amount should give different ID")
	}
}
