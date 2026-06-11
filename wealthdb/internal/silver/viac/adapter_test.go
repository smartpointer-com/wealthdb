package viac

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
	path := filepath.Join(t.TempDir(), "viac.db")
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

func TestKindIsViac(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "viac" {
		t.Errorf("Kind() = %q, want %q", got, "viac")
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

// TestSnapshotsTaxWrapperMapping covers the distinctive viac
// projection: product_code → tax_wrapper, the automated
// management-style default, and a position's quantity /
// market_value / book_value (book = quantity × acquisition_price)
// plus the cash balance.
func TestSnapshotsTaxWrapperMapping(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 2, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, product_code, name, state, currency_code, management_style, payload) VALUES
            (1000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}'),
            (1000, 'PVB1', '2', 'Vested', 'ACTIVE', 'CHF', 'automated', '{}'),
            (1000, 'INV1', '1', 'Free Invest', 'ACTIVE', 'CHF', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, asset_class, currency_code, name, quantity, market_value_chf, acquisition_price, asset_price, payload) VALUES
            (1000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Fund A', 10, 1500.00, 120.00, 150.00, '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, currency_code, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Fund A', 'CHF', 'equity', 1000, 1000, '{}');
        INSERT INTO cash_balances(snapshot_at, account_external_id, currency, balance_kind, amount, payload) VALUES
            (1000, 'P3A1', 'CHF', 'cash', 250.50, NULL);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	// Accounts: tax_wrapper per product_code, automated mgmt style.
	wrappers := map[string]canonical.TaxWrapper{}
	styles := map[string]canonical.ManagementStyle{}
	for _, a := range batch.Accounts {
		if a.TaxWrapper != nil {
			wrappers[a.AccountExternalID] = *a.TaxWrapper
		}
		if a.ManagementStyle != nil {
			styles[a.AccountExternalID] = *a.ManagementStyle
		}
		if a.AccountKind != canonical.AccountKindBrokerage {
			t.Errorf("%s account_kind = %q, want brokerage", a.AccountExternalID, a.AccountKind)
		}
	}
	if wrappers["P3A1"] != canonical.TaxWrapperPillar3a {
		t.Errorf("P3A1 tax_wrapper = %q, want pillar_3a", wrappers["P3A1"])
	}
	if wrappers["PVB1"] != canonical.TaxWrapperVestedBenefits {
		t.Errorf("PVB1 tax_wrapper = %q, want vested_benefits", wrappers["PVB1"])
	}
	if wrappers["INV1"] != canonical.TaxWrapperTaxablePersonal {
		t.Errorf("INV1 tax_wrapper = %q, want taxable_personal", wrappers["INV1"])
	}
	if styles["P3A1"] != canonical.ManagementStyleAutomated {
		t.Errorf("P3A1 management_style = %q, want automated", styles["P3A1"])
	}

	// Position: quantity / market_value / book_value.
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(batch.Positions))
	}
	p := batch.Positions[0]
	if p.AssetClass != canonical.AssetClassEquity {
		t.Errorf("asset_class = %q, want equity", p.AssetClass)
	}
	if p.Currency != "CHF" {
		t.Errorf("currency = %q, want CHF", p.Currency)
	}
	if p.Quantity == nil || p.Quantity.String() != "10" {
		t.Errorf("quantity = %v, want 10", p.Quantity)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "1500" {
		t.Errorf("market_value = %v, want 1500", p.MarketValue)
	}
	// book_value = quantity(10) × acquisition_price(120) = 1200.
	if p.BookValue == nil || p.BookValue.String() != "1200" {
		t.Errorf("book_value = %v, want 1200", p.BookValue)
	}

	// Cash balance.
	if len(batch.CashBalances) != 1 {
		t.Fatalf("cash_balances = %d, want 1", len(batch.CashBalances))
	}
	c := batch.CashBalances[0]
	if c.Currency != "CHF" || c.Amount.String() != "250.5" {
		t.Errorf("cash = %s %s, want CHF 250.5", c.Currency, c.Amount.String())
	}
}

// TestTransactions covers the canonical-kind pass-through and the
// sign convention (a dividend is a positive inflow).
func TestTransactions(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 2, '/x/1');
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at, account_external_id, type, kind, amount_chf, currency, payload) VALUES
            ('tx1', 1000, 900, 'P3A1', 'DIVIDEND', 'dividend', 42.00, 'CHF', '{}'),
            ('tx2', 1000, 950, 'P3A1', 'TRADE_BUY', 'buy', -100.00, 'CHF', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Transactions) != 2 {
		t.Fatalf("transactions = %d, want 2", len(batch.Transactions))
	}
	byID := map[string]canonical.TransactionChange{}
	for _, x := range batch.Transactions {
		byID[x.TransactionExternalID] = x
	}
	div := byID["tx1"]
	if div.Kind != canonical.TxKindDividend {
		t.Errorf("tx1 kind = %q, want dividend", div.Kind)
	}
	// Dividend canonical sign is positive (inflow).
	if div.NetAmount == nil || div.NetAmount.String() != "42" {
		t.Errorf("tx1 net_amount = %v, want 42", div.NetAmount)
	}
	buy := byID["tx2"]
	if buy.Kind != canonical.TxKindBuy {
		t.Errorf("tx2 kind = %q, want buy", buy.Kind)
	}
	// Buy canonical sign is negative (outflow); source already -100.
	if buy.NetAmount == nil || buy.NetAmount.String() != "-100" {
		t.Errorf("tx2 net_amount = %v, want -100", buy.NetAmount)
	}
}
