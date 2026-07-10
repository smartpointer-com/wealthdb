package fidelity

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

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fidelity.db")
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

func TestKindIsFidelity(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "fidelity" {
		t.Errorf("Kind() = %q, want %q", got, "fidelity")
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

// TestSnapshotsCorePositionBecomesCash covers fidelity's
// distinctive split: an is_core_position=1 money-market sweep row
// projects to a CashBalanceChange, an ordinary row to a
// PositionChange, and a "Pending activity" row is dropped. Also
// checks the account_kind=brokerage / USD base / portfolio-kind
// taxonomy projection.
func TestSnapshotsCorePositionBecomesCash(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES
            (1000, 'PORT1', '529', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, management_style, payload) VALUES
            (1000, 'ACC1', 'PORT1', 'College', NULL, '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, currency, is_core_position, quantity, current_value, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Vanguard Total Market', 'etf', 'USD', 0, 10, 2500.00, '{}'),
            (1000, 'ACC1', 'FDRXX', 'Fidelity Cash', 'money_market', 'USD', 1, 0, 314.15, '{}'),
            (1000, 'ACC1', '', 'Pending activity', '', 'USD', 0, 0, 0, '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	// Only the ordinary position; core + pending excluded.
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %d, want 1 (core + pending excluded)", len(batch.Positions))
	}
	p := batch.Positions[0]
	if p.PositionKey != "VTI" {
		t.Errorf("position key = %q, want VTI", p.PositionKey)
	}
	if p.AssetClass != canonical.AssetClassETF {
		t.Errorf("asset_class = %q, want etf", p.AssetClass)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "2500" {
		t.Errorf("market_value = %v, want 2500", p.MarketValue)
	}

	// Core money-market row → cash balance.
	if len(batch.CashBalances) != 1 {
		t.Fatalf("cash_balances = %d, want 1 (the core position)", len(batch.CashBalances))
	}
	c := batch.CashBalances[0]
	if c.BalanceKind != canonical.BalanceKindCurrent {
		t.Errorf("balance_kind = %q, want current", c.BalanceKind)
	}
	if c.Currency != "USD" || c.Amount.String() != "314.15" {
		t.Errorf("cash = %s %s, want USD 314.15", c.Currency, c.Amount.String())
	}

	// Account: brokerage / USD / 529 wrapper from portfolio kind.
	if len(batch.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(batch.Accounts))
	}
	a := batch.Accounts[0]
	if a.AccountKind != canonical.AccountKindBrokerage {
		t.Errorf("account_kind = %q, want brokerage", a.AccountKind)
	}
	if a.BaseCurrency == nil || *a.BaseCurrency != "USD" {
		t.Errorf("base_currency = %v, want USD", a.BaseCurrency)
	}
	if a.TaxWrapper == nil || *a.TaxWrapper != canonical.TaxWrapper529 {
		t.Errorf("tax_wrapper = %v, want 529", a.TaxWrapper)
	}
}

// TestTransactions checks canonical-kind pass-through + signing.
func TestTransactions(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('act1', 900, 'ACC1', 'DIVIDEND', 'VTI', 'USD', 0, 0, 12.00, '{}'),
            ('act2', 950, 'ACC1', 'BUY', 'VTI', 'USD', 5, 250.00, -1250.00, '{}');
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
	if byID["act1"].Kind != canonical.TxKindDividend {
		t.Errorf("act1 kind = %q, want dividend", byID["act1"].Kind)
	}
	if v := byID["act1"].NetAmount; v == nil || v.String() != "12" {
		t.Errorf("act1 net_amount = %v, want 12", v)
	}
	if byID["act2"].Kind != canonical.TxKindBuy {
		t.Errorf("act2 kind = %q, want buy", byID["act2"].Kind)
	}
	if v := byID["act2"].NetAmount; v == nil || v.String() != "-1250" {
		t.Errorf("act2 net_amount = %v, want -1250", v)
	}
}

// classifyHistorical covers the statement-PDF shapes: statement rows
// carry no structured type code, so the class comes from instrument-key and description
// shapes alone.
func TestClassifyHistorical(t *testing.T) {
	cases := []struct {
		key, desc string
		want      canonical.AssetClass
	}{
		// Options: OCC key or CALL/PUT-prefixed description.
		{"ABCD300118C100", "CALL (ABCD) PLACEHOLDER CORP JAN 18 30", canonical.AssetClassOption},
		{"", "PUT (ABCD) PLACEHOLDER CORP JAN 18 30", canonical.AssetClassOption},
		// Money-market sweeps + the svb net-cash sleeve.
		{"SPAXX", "FIDELITY GOVERNMENT MONEY MARKET", canonical.AssetClassMoneyMarket},
		{"FDRXX", "FIDELITY GOVERNMENT CASH RESERVES", canonical.AssetClassMoneyMarket},
		{"", "NET CASH POSITION", canonical.AssetClassMoneyMarket},
		// 529 plan sleeves, keyed and keyless.
		{"ABC123456", "STATE PLAN 2030 (FIDELITY BLEND)", canonical.AssetClassFund},
		{"", "STATE PLAN 2030 (FIDELITY FUNDS)", canonical.AssetClassFund},
		// Bonds: CUSIP-9 key or coupon in the description.
		{"000000AA1", "PLACEHOLDER MUNI GO BDS SER. 2021", canonical.AssetClassBond},
		{"", "PLACEHOLDER CORP NOTE 04.12500% 01/15/2042", canonical.AssetClassBond},
		{"", "PLACEHOLDER ST GO BDS 1,234.56 FIXED COUPON", canonical.AssetClassBond},
		// ETFs refine by underlying exposure.
		{"ABCD", "ISHARES TR PLACEHOLDER ETF", canonical.AssetClassETF},
		{"IBIT", "iShares Bitcoin Trust ETF", canonical.AssetClassCrypto},
		{"", "ISHARES 20+ YEAR TREASURY BOND ETF", canonical.AssetClassBondETF},
		// Mutual-fund ticker convention.
		{"ABCDX", "PLACEHOLDER EMERGING MKTS INSTL", canonical.AssetClassFund},
		// The svb $0 closure marker carries no exposure.
		{"", "Account closed — assets transferred", canonical.AssetClassOther},
		// Fall-through: plain stock / ADR rows.
		{"AAPL", "APPLE INC", canonical.AssetClassEquity},
		{"", "PLACEHOLDER AG SPON ADR EACH REP 1 ORD SHS", canonical.AssetClassEquity},
		{"NFLX", "NETFLIX INC", canonical.AssetClassEquity},
	}
	for _, c := range cases {
		if got := classifyHistorical(c.key, c.desc); got != c.want {
			t.Errorf("classifyHistorical(%q, %q) = %q, want %q", c.key, c.desc, got, c.want)
		}
	}
}
