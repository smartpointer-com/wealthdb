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
	if p.MarketValue == nil || p.MarketValue.String() != "2500" {
		t.Errorf("market_value = %v, want 2500", p.MarketValue)
	}
	// An equity-exposure ETF wrapper → (public_equity, etf),
	// agreeing on instrument + position.
	if p.AssetClass != canonical.AssetClassPublicEquity || p.Vehicle != canonical.VehicleETF {
		t.Errorf("position (exposure, vehicle) = (%q, %q), want (public_equity, etf)", p.AssetClass, p.Vehicle)
	}
	if !canonical.ValidTaxonomyPair(p.AssetClass, p.Vehicle) {
		t.Errorf("position pair (%q, %q) not an admitted taxonomy pair", p.AssetClass, p.Vehicle)
	}
	if len(batch.Instruments) != 1 {
		t.Fatalf("instruments = %d, want 1", len(batch.Instruments))
	}
	inst := batch.Instruments[0]
	if inst.AssetClass != p.AssetClass || inst.Vehicle != p.Vehicle {
		t.Errorf("instrument pair = (%q, %q), want it to agree with position (%q, %q)",
			inst.AssetClass, inst.Vehicle, p.AssetClass, p.Vehicle)
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

// TestDistributionKinds covers the DISTRIBUTION overload: share
// legs of splits / ADR ratio changes (nonzero quantity) and
// spinoffs (SPINOFF action) are corporate actions with the source
// amount preserved; pure-cash capital-gain payouts stay
// dividend-class income.
func TestDistributionKinds(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('split',   900, 'ACC1', 'DISTRIBUTION', 'XYZ',   'USD', 10, 0, 1000.00,
             '{"Action": "DISTRIBUTION EXAMPLE CORP COM NEW (XYZ) (Cash)"}'),
            ('spinoff', 910, 'ACC1', 'DISTRIBUTION', 'XYZA',  'USD', 0,  0, 0.00,
             '{"Action": "DISTRIBUTION SPINOFF FROM:(XYZ ) EXAMPLE AERO INC COM (XYZA) (Cash)"}'),
            ('capgain', 920, 'ACC1', 'DISTRIBUTION', 'XFNDX', 'USD', 0,  0, 250.00,
             '{"Action": "LONG-TERM CAP GAIN as of Dec-19-2024 EXAMPLE FUND (XFNDX) (Cash)"}');
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
	byID := map[string]canonical.TransactionChange{}
	for _, x := range batch.Transactions {
		byID[x.TransactionExternalID] = x
	}
	for id, want := range map[string]canonical.TxKind{
		"split":   canonical.TxKindCorporateAction,
		"spinoff": canonical.TxKindCorporateAction,
		"capgain": canonical.TxKindDividend,
	} {
		if got := byID[id].Kind; got != want {
			t.Errorf("%s kind = %q, want %q", id, got, want)
		}
	}
	// Corporate actions pass the source amount through unchanged
	// (the MERGER convention): the split leg keeps its informative
	// share-value amount, the income row its cash amount.
	if v := byID["split"].NetAmount; v == nil || v.String() != "1000" {
		t.Errorf("split net_amount = %v, want 1000", v)
	}
	if v := byID["capgain"].NetAmount; v == nil || v.String() != "250" {
		t.Errorf("capgain net_amount = %v, want 250", v)
	}
}

// TestAssetClassVehicleFor covers the live-path taxonomy helper:
// each silver class maps to an (exposure, Vehicle) pair, with the
// fund/ETF exposure refined from the security name. All synthetic
// names / placeholder tickers.
func TestAssetClassVehicleFor(t *testing.T) {
	cases := []struct {
		silverClass, name string
		wantExposure      canonical.AssetClass
		wantVehicle       canonical.Vehicle
	}{
		{"equity", "PLACEHOLDER INC", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		// ETF wrapper, exposure refined by name.
		{"etf", "PLACEHOLDER BROAD MARKET ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"etf", "PLACEHOLDER BITCOIN TRUST ETF", canonical.AssetClassCrypto, canonical.VehicleETF},
		{"etf", "PLACEHOLDER 20+ YEAR TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		// Mutual-fund wrapper, exposure refined by name.
		{"mutual_fund", "PLACEHOLDER EMERGING MKTS INSTL", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		{"mutual_fund", "PLACEHOLDER GOLD BULLION FUND", canonical.AssetClassMetal, canonical.VehicleFund},
		// A purchased (non-core) money-market fund arrives as mutual_fund;
		// it is a cash equivalent, not the bond exposure the TREASURY
		// keyword would otherwise suggest.
		{"mutual_fund", "PLACEHOLDER TREASURY MONEY MARKET FUND", canonical.AssetClassCash, canonical.VehicleFund},
		// 529 investment-option sleeve → blended multi-asset fund.
		{"plan_fund", "STATE PLAN 2099 (FIDELITY BLEND)", canonical.AssetClassMultiAsset, canonical.VehicleFund},
		{"bond", "PLACEHOLDER CORP NOTE 04.12500% 01/15/2042", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		// money_market never reaches the position build, but is mapped
		// for parity with assetClassFor.
		{"money_market", "PLACEHOLDER GOVERNMENT MONEY MARKET", canonical.AssetClassCash, canonical.VehicleFund},
		// Unknown class → (other, other).
		{"widget", "PLACEHOLDER THING", canonical.AssetClassOther, canonical.VehicleOther},
	}
	for _, c := range cases {
		gotExp, gotVeh := assetClassVehicleFor(c.silverClass, c.name)
		if gotExp != c.wantExposure || gotVeh != c.wantVehicle {
			t.Errorf("assetClassVehicleFor(%q, %q) = (%q, %q), want (%q, %q)",
				c.silverClass, c.name, gotExp, gotVeh, c.wantExposure, c.wantVehicle)
		}
		if !canonical.ValidTaxonomyPair(gotExp, gotVeh) {
			t.Errorf("assetClassVehicleFor(%q, %q) → (%q, %q) is not an admitted taxonomy pair",
				c.silverClass, c.name, gotExp, gotVeh)
		}
	}
}

// TestClassifyHistoricalPair covers the statement-PDF shapes: statement rows carry no
// structured type code, so the (exposure, Vehicle) pair comes from instrument-key
// and description shapes alone. First match wins.
func TestClassifyHistoricalPair(t *testing.T) {
	cases := []struct {
		key, desc    string
		wantExposure canonical.AssetClass
		wantVehicle  canonical.Vehicle
	}{
		// Options → equity-underlying option legs.
		{"ABCD300118C100", "CALL (ABCD) PLACEHOLDER CORP JAN 18 30", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		{"", "PUT (ABCD) PLACEHOLDER CORP JAN 18 30", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		// Money-market sweeps + the svb net-cash sleeve → cash / fund.
		{"SPAXX", "FIDELITY GOVERNMENT MONEY MARKET", canonical.AssetClassCash, canonical.VehicleFund},
		{"", "NET CASH POSITION", canonical.AssetClassCash, canonical.VehicleFund},
		// XX-ticker money fund whose truncated description names no
		// money-market token.
		{"EXMXX", "PLACEHOLDER VALUE FUND◊", canonical.AssetClassCash, canonical.VehicleFund},
		// 529 plan sleeves, keyed and keyless → multi-asset / fund.
		{"ABC123456", "STATE PLAN 2099 (FIDELITY BLEND)", canonical.AssetClassMultiAsset, canonical.VehicleFund},
		{"", "STATE PLAN 2099 (FIDELITY FUNDS)", canonical.AssetClassMultiAsset, canonical.VehicleFund},
		// Bonds → fixed_income / bond.
		{"000000AA1", "PLACEHOLDER MUNI GO BDS SER. 2021", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"", "PLACEHOLDER CORP NOTE 04.12500% 01/15/2042", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		// ETF-by-name → etf wrapper, exposure refined from the name.
		{"ABCD", "ISHARES TR PLACEHOLDER ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"IBIT", "iShares Bitcoin Trust ETF", canonical.AssetClassCrypto, canonical.VehicleETF},
		{"", "ISHARES 20+ YEAR TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		// Mutual-fund ticker → fund wrapper, exposure refined from the name.
		{"ABCDX", "PLACEHOLDER EMERGING MKTS INSTL", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		// ETF-only issuer families without the "ETF" token → etf; an
		// issuer's mutual fund (X-ticker, previous case shape) still
		// wins the fund vehicle first.
		{"EXA", "ISHARES TRUST DJ US EXAMPLE", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"EXB", "VANGUARD INTL EQUITY INDEX FDS EXAMPLE", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"EXC", "ISHARES TREASURY FLOATING RATE EXAMPLE", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"EXMPX", "VANGUARD EXAMPLE ADMIRAL SHARES", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		// The svb $0 closure marker carries no exposure.
		{"", "Account closed — assets transferred", canonical.AssetClassOther, canonical.VehicleOther},
		// Fall-through: plain stock / ADR rows → public_equity / stock.
		{"AAPL", "APPLE INC", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		{"", "PLACEHOLDER AG SPON ADR EACH REP 1 ORD SHS", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		{"NFLX", "NETFLIX INC", canonical.AssetClassPublicEquity, canonical.VehicleStock},
	}
	for _, c := range cases {
		gotExp, gotVeh := classifyHistoricalPair(c.key, c.desc)
		if gotExp != c.wantExposure || gotVeh != c.wantVehicle {
			t.Errorf("classifyHistoricalPair(%q, %q) = (%q, %q), want (%q, %q)",
				c.key, c.desc, gotExp, gotVeh, c.wantExposure, c.wantVehicle)
		}
		if !canonical.ValidTaxonomyPair(gotExp, gotVeh) {
			t.Errorf("classifyHistoricalPair(%q, %q) → (%q, %q) is not an admitted taxonomy pair",
				c.key, c.desc, gotExp, gotVeh)
		}
	}
}
