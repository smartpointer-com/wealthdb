package cointracking

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// newFixtureSilver creates a temp DuckDB silver, runs `seed` against
// it, then CLOSES it — cointracking's adapter opens DuckDB
// read-only, and DuckDB won't grant a read-only handle while a
// read-write one is still open on the same file. Returns the path.
func newFixtureSilver(t *testing.T, seed string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cointracking.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		db.Close()
		t.Fatalf("schema: %v", err)
	}
	if seed != "" {
		if _, err := db.Exec(seed); err != nil {
			db.Close()
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	return path
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

func TestKindIsCointracking(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "cointracking" {
		t.Errorf("Kind() = %q, want %q", got, "cointracking")
	}
}

func TestStatusEmpty(t *testing.T) {
	path := newFixtureSilver(t, "")
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

// TestSnapshotsCryptoPositionAndFiatCash covers the cointracking
// projection: a crypto holding becomes a PositionChange valued at
// quantity × portfolio_prices.price in the portfolio's quote
// currency; a fiat holding (USD) becomes a CashBalanceChange, not
// a position; accounts are account_kind=crypto / self_directed /
// taxable_personal.
func TestSnapshotsCryptoPositionAndFiatCash(t *testing.T) {
	path := newFixtureSilver(t, `
        INSERT INTO dump_runs VALUES (1000, 2, '/x/1', NULL);
        INSERT INTO portfolios VALUES ('cu1', 1, 'aaa', 1000, NULL);
        INSERT INTO wallets VALUES ('cu1', 'cu1:Kraken', 'Kraken', 1000, NULL);
        INSERT INTO transactions VALUES
            ('tx1', 'cu1', 'cu1:Kraken', 1000, TIMESTAMP '2024-01-01 00:00:00',
             'Trade', 2.0, 'BTC', 60000.0, 'USD', NULL, NULL, NULL, NULL, NULL);
        INSERT INTO positions_daily VALUES
            (DATE '2024-01-01', 'cu1', 'cu1:Kraken', 'BTC', 2.0, 1000),
            (DATE '2024-01-01', 'cu1', 'cu1:Kraken', 'USD', 500.0, 1000);
        INSERT INTO portfolio_prices VALUES
            (DATE '2024-01-01', 'cu1', 'BTC', 'USD', 30000.0, 1000);
    `)
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges {
		t.Fatal("ChangeWindow.HasChanges = false, want true")
	}
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()

	// Drain to the snapshot batch carrying the 2024-01-01 holdings.
	var crypto canonical.PositionChange
	var cash canonical.CashBalanceChange
	var btcInst canonical.InstrumentChange
	var nPos, nCash int
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			nPos++
			crypto = p
		}
		for _, c := range batch.CashBalances {
			nCash++
			cash = c
		}
		for _, ins := range batch.Instruments {
			if ins.InstrumentExternalID == "BTC" {
				btcInst = ins
			}
		}
		if !more {
			break
		}
	}

	// InstrumentChange carries the same 2-D pair as the position.
	if btcInst.AssetClass != canonical.AssetClassCrypto {
		t.Errorf("instrument asset_class = %q, want crypto", btcInst.AssetClass)
	}
	if btcInst.Vehicle != canonical.VehiclePhysical {
		t.Errorf("instrument vehicle = %q, want physical", btcInst.Vehicle)
	}

	if nPos != 1 {
		t.Fatalf("positions = %d, want 1 (BTC; USD is cash)", nPos)
	}
	if crypto.PositionKey != "BTC" {
		t.Errorf("position key = %q, want BTC", crypto.PositionKey)
	}
	// 2-D taxonomy: every cointracking holding is crypto exposure
	// held directly in a wallet → crypto × physical.
	if crypto.AssetClass != canonical.AssetClassCrypto {
		t.Errorf("asset_class = %q, want crypto", crypto.AssetClass)
	}
	if crypto.Vehicle != canonical.VehiclePhysical {
		t.Errorf("vehicle = %q, want physical", crypto.Vehicle)
	}
	if !canonical.ValidTaxonomyPair(crypto.AssetClass, crypto.Vehicle) {
		t.Errorf("(%q, %q) is not a valid taxonomy pair",
			crypto.AssetClass, crypto.Vehicle)
	}
	if crypto.Currency != "USD" {
		t.Errorf("currency = %q, want USD", crypto.Currency)
	}
	if crypto.Quantity == nil || crypto.Quantity.String() != "2" {
		t.Errorf("quantity = %v, want 2", crypto.Quantity)
	}
	// market_value = 2 × 30000 = 60000.
	if crypto.MarketValue == nil || crypto.MarketValue.String() != "60000" {
		t.Errorf("market_value = %v, want 60000", crypto.MarketValue)
	}

	if nCash != 1 {
		t.Fatalf("cash_balances = %d, want 1 (the USD holding)", nCash)
	}
	if cash.Currency != "USD" || cash.Amount.String() != "500" {
		t.Errorf("cash = %s %s, want USD 500", cash.Currency, cash.Amount.String())
	}

	// Account taxonomy is on the latest batch's Accounts. Re-open to
	// read the dimension records (they ride the latest batch).
	conn2 := openAdapter(t, path)
	w2, _ := conn2.ChangeWindow(context.Background(), -1)
	stream2, _ := conn2.Snapshots(context.Background(), w2)
	defer stream2.Close()
	var acct canonical.AccountChange
	var nAcct int
	for {
		batch, more, err := stream2.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range batch.Accounts {
			nAcct++
			acct = a
		}
		if !more {
			break
		}
	}
	if nAcct != 1 {
		t.Fatalf("accounts = %d, want 1", nAcct)
	}
	if acct.AccountKind != canonical.AccountKindCrypto {
		t.Errorf("account_kind = %q, want crypto", acct.AccountKind)
	}
	if acct.ManagementStyle == nil || *acct.ManagementStyle != canonical.ManagementStyleSelfDirected {
		t.Errorf("management_style = %v, want self_directed", acct.ManagementStyle)
	}
	if acct.TaxWrapper == nil || *acct.TaxWrapper != canonical.TaxWrapperTaxablePersonal {
		t.Errorf("tax_wrapper = %v, want taxable_personal", acct.TaxWrapper)
	}
}

// The payload carries the CT fields the lot engine reads: the type, the
// comment, the trade's currencies and its fee. The kind alone cannot
// tell a deposit from an airdrop.
func TestTransactionsCarryTheLotFields(t *testing.T) {
	path := newFixtureSilver(t, `
        INSERT INTO dump_runs VALUES (1000, 2, '/x/1', NULL);
        INSERT INTO portfolios VALUES ('cu1', 1, 'aaa', 1000, NULL);
        INSERT INTO wallets VALUES ('cu1', 'cu1:Kraken', 'Kraken', 1000, NULL);
        INSERT INTO transactions (transaction_external_id, portfolio_external_id, wallet_external_id, snapshot_at,
                                  occurred_at, type, buy_amount, buy_currency, sell_amount, sell_currency,
                                  fee_amount, fee_currency, comment) VALUES
            ('t1', 'cu1', 'cu1:Kraken', 1000, TIMESTAMP '2024-01-01 10:00:00', 'Trade',
             2.0, 'BTC', 60000.0, 'USD', 0.01, 'BNB', NULL),
            ('t2', 'cu1', 'cu1:Kraken', 1000, TIMESTAMP '2024-01-02 10:00:00', 'Airdrop',
             5.0, 'ETH', NULL, NULL, 0, NULL, 'promo');
        INSERT INTO portfolio_prices VALUES (DATE '2024-01-01', 'cu1', 'BTC', 'USD', 30000.0, 1000);
    `)
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	payloads := map[string]map[string]any{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tx := range batch.Transactions {
			var m map[string]any
			if err := json.Unmarshal(tx.Payload, &m); err != nil {
				t.Fatalf("%s payload %q: %v", tx.TransactionExternalID, tx.Payload, err)
			}
			payloads[tx.TransactionExternalID] = m
		}
		if !more {
			break
		}
	}
	trade, drop := payloads["t1"], payloads["t2"]
	if trade["type"] != "Trade" || trade["fee_currency"] != "BNB" || trade["fee_amount"] != 0.01 ||
		trade["buy_currency"] != "BTC" || trade["sell_currency"] != "USD" {
		t.Errorf("trade payload %v", trade)
	}
	if drop["type"] != "Airdrop" || drop["comment"] != "promo" {
		t.Errorf("airdrop payload %v", drop)
	}
	if _, ok := drop["fee_amount"]; ok {
		t.Errorf("a zero fee is left out: %v", drop)
	}
}
