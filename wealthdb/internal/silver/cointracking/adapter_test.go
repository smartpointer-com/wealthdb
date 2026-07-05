package cointracking

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
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
		if !more {
			break
		}
	}

	if nPos != 1 {
		t.Fatalf("positions = %d, want 1 (BTC; USD is cash)", nPos)
	}
	if crypto.PositionKey != "BTC" {
		t.Errorf("position key = %q, want BTC", crypto.PositionKey)
	}
	if crypto.AssetClass != canonical.AssetClassCrypto {
		t.Errorf("asset_class = %q, want crypto", crypto.AssetClass)
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
