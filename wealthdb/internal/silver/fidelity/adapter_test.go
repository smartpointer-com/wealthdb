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

// TestClassifyHistoricalPair covers the statement-PDF shapes: the
// trust, brokerage, deposit and mortgage historical rows carry no
// structured type code, so the (exposure, Vehicle) pair comes from
// instrument-key and description shapes alone. First match wins.
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
		// The svb stated-$0 row carries no exposure.
		{"", "NO POSITIONS", canonical.AssetClassOther, canonical.VehicleOther},
		// The svb OCRed deposit and mortgage rows. Neither names an
		// instrument, so without these both take the fall-through below —
		// which would book a home loan as public equity.
		{"", "CASH BALANCE", canonical.AssetClassCash, canonical.VehicleDemandDeposit},
		// A whole-account value for a month the archive misses: worth
		// known, composition not, so no exposure to claim.
		{"", "ACCOUNT VALUE (ADVISOR MARK)", canonical.AssetClassOther, canonical.VehicleOther},
		{"", "MORTGAGE PRINCIPAL", canonical.AssetClassRealEstate, canonical.VehicleMortgage},
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

// TestSnapshotsDAFTaxonomy covers the Donor-Advised Fund projection
// (fidelity-web DESIGN.md §12.3): silver portfolios.kind='daf' →
// AccountKind donor_advised_fund + TaxWrapper charitable, the
// management style from the silver column, and a daf_pool position
// mapping to (multi_asset, fund) — all through the single fidelity
// source, alongside the retail rows.
func TestSnapshotsDAFTaxonomy(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 7, '/x/1');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES
            (1000, 'PORT1', '529', '{}'),
            (1000, 'Fidelity Charitable Giving', 'daf', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, management_style, payload) VALUES
            (1000, 'ACC1', 'PORT1', 'College', NULL, '{}'),
            (1000, '9990001', 'Fidelity Charitable Giving', 'Example Giving Account', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, currency, is_core_position, quantity, current_value, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Vanguard Total Market', 'etf', 'USD', 0, 10, 2500.00, '{}'),
            (1000, '9990001', 'POOLX1', 'Example Growth Pool', 'daf_pool', 'USD', 0, 1000, 100000.00, '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 (529 + daf)", len(batch.Accounts))
	}
	var daf *canonical.AccountChange
	for i := range batch.Accounts {
		if batch.Accounts[i].AccountExternalID == "9990001" {
			daf = &batch.Accounts[i]
		}
	}
	if daf == nil {
		t.Fatal("daf account missing from batch")
	}
	if daf.AccountKind != canonical.AccountKindDonorAdvisedFund {
		t.Errorf("account_kind = %q, want donor_advised_fund", daf.AccountKind)
	}
	if daf.TaxWrapper == nil || *daf.TaxWrapper != canonical.TaxWrapperCharitable {
		t.Errorf("tax_wrapper = %v, want charitable", daf.TaxWrapper)
	}
	if daf.ManagementStyle == nil || *daf.ManagementStyle != canonical.ManagementStyleAutomated {
		t.Errorf("management_style = %v, want automated (from silver column)", daf.ManagementStyle)
	}

	var pool *canonical.PositionChange
	for i := range batch.Positions {
		if batch.Positions[i].PositionKey == "POOLX1" {
			pool = &batch.Positions[i]
		}
	}
	if pool == nil {
		t.Fatal("daf pool position missing from batch")
	}
	if pool.AssetClass != canonical.AssetClassMultiAsset || pool.Vehicle != canonical.VehicleFund {
		t.Errorf("daf_pool (exposure, vehicle) = (%q, %q), want (multi_asset, fund)", pool.AssetClass, pool.Vehicle)
	}
	if !canonical.ValidTaxonomyPair(pool.AssetClass, pool.Vehicle) {
		t.Errorf("daf_pool pair (%q, %q) not an admitted taxonomy pair", pool.AssetClass, pool.Vehicle)
	}
}

// TestHistoricalDAFPoolClassification covers the statement-PDF
// reconstruction path for the Donor-Advised Fund (fidelity-web
// DESIGN.md §12): a historical_position_snapshots row on a DAF
// account — pool name as description, retired pool so no
// instrument_key cross-walk — projects as (multi_asset, fund) with
// the DAF taxonomy on the back-projected account master, rather than
// falling through the shape heuristics to (public_equity, stock).
func TestHistoricalDAFPoolClassification(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 7, '/x/1');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES
            (2000, 'Fidelity Charitable Giving', 'daf', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, management_style, payload) VALUES
            (2000, '9990001', 'Fidelity Charitable Giving', 'Example Giving Account', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, currency, is_core_position, quantity, current_value, payload) VALUES
            (2000, '9990001', 'POOLX1', 'Example Pool', 'daf_pool', 'USD', 0, 1000, 100000.00, '{}');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, price, market_value, currency, payload) VALUES
            (1000, '9990001', 'Example Growth', NULL, 500, 100.0, 50000.00, 'USD', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()

	var histPos *canonical.PositionChange
	var histAcct *canonical.AccountChange
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for i := range batch.Positions {
			if batch.Positions[i].SnapshotAt == 1000 {
				histPos = &batch.Positions[i]
			}
		}
		for i := range batch.Accounts {
			if batch.Accounts[i].FirstSeenAt == 1000 {
				histAcct = &batch.Accounts[i]
			}
		}
		if !more {
			break
		}
	}
	if histPos == nil {
		t.Fatal("historical DAF position missing from stream")
	}
	if histPos.AssetClass != canonical.AssetClassMultiAsset || histPos.Vehicle != canonical.VehicleFund {
		t.Errorf("historical daf pool (exposure, vehicle) = (%q, %q), want (multi_asset, fund)",
			histPos.AssetClass, histPos.Vehicle)
	}
	if histAcct == nil {
		t.Fatal("back-projected DAF account master missing at historical date")
	}
	if histAcct.AccountKind != canonical.AccountKindDonorAdvisedFund {
		t.Errorf("historical account_kind = %q, want donor_advised_fund", histAcct.AccountKind)
	}
	if histAcct.TaxWrapper == nil || *histAcct.TaxWrapper != canonical.TaxWrapperCharitable {
		t.Errorf("historical tax_wrapper = %v, want charitable", histAcct.TaxWrapper)
	}
}

// TestAnAccountHoldingAMortgageIsAMortgage pins the one account kind
// this adapter reads off holdings: an account whose historical rows
// carry a home loan's outstanding principal is typed `mortgage` on
// every emission, back-projected ones included, while an account
// holding a deposit balance beside it stays brokerage.
func TestAnAccountHoldingAMortgageIsAMortgage(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (3000, 4, 'svb-sleeves-build');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES
            (3000, 'SVB-Sleeves', 'other', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, payload) VALUES
            (3000, '0000000002', 'SVB-Sleeves', NULL, '{}'),
            (3000, '0000000000', 'SVB-Sleeves', NULL, '{}');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, price, market_value, currency, payload) VALUES
            (1000, '0000000002', 'MORTGAGE PRINCIPAL', NULL, NULL, NULL, -900000.00, 'USD', '{}'),
            (2000, '0000000002', 'MORTGAGE PRINCIPAL', NULL, NULL, NULL, -1.00, 'USD', '{}'),
            (3000, '0000000002', 'MORTGAGE PRINCIPAL', NULL, NULL, NULL, 0.00, 'USD', '{}'),
            (3000, '0000000000', 'CASH BALANCE', NULL, NULL, NULL, 1500.00, 'USD', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	kinds := map[string]map[canonical.AccountKind]int{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range batch.Accounts {
			if kinds[a.AccountExternalID] == nil {
				kinds[a.AccountExternalID] = map[canonical.AccountKind]int{}
			}
			kinds[a.AccountExternalID][a.AccountKind]++
		}
		if !more {
			break
		}
	}
	if got := kinds["0000000002"]; len(got) != 1 || got[canonical.AccountKindMortgage] < 3 {
		t.Errorf("loan account kinds = %v, want mortgage on every emission", got)
	}
	if got := kinds["0000000000"]; len(got) != 1 || got[canonical.AccountKindBrokerage] == 0 {
		t.Errorf("deposit account kinds = %v, want brokerage only", got)
	}
}

// TestOutflowKindsReachSpending pins the two actions that decide
// whether a managed account's real outflows are visible at all.
//
// Both used to fall through kindFor to TxKindOther, and the spending
// population selects on kind — so an account could pay its manager
// four figures a quarter and wire six figures out, and a spending
// report would show it spending nothing.
//
// ADVISOR joins FEE rather than getting a kind of its own: both are
// money out for a fee, and what tells a management fee from a
// security-level ADR pass-through is the NARRATIVE, which the rule
// tier reads. An outbound WIRE becomes a withdrawal so the
// internal-transfer matcher gets first refusal on it — a wire to an
// account wealthdb also tracks pairs and nets out; one to an account
// it does not is spend.
func TestOutflowKindsReachSpending(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('adv',  900, 'ACC1', 'ADVISOR', NULL, 'USD', 0, 0, -1234.00,
             '{"Action": "ADVISOR FEE DEDUCTED Investment Mgr Fee (Cash)"}'),
            ('wire', 910, 'ACC1', 'WIRE',    NULL, 'USD', 0, 0, -5678.00,
             '{"Action": "WIRE TRANSFER TO BANK (Cash)"}'),
            ('wirein', 915, 'ACC1', 'WIRE',   NULL, 'USD', 0, 0,  5678.00,
             '{"Action": "WIRE TRANSFER FROM BANK (Cash)"}'),
            ('achout', 916, 'ACC1', 'DIRECT_DEBIT', NULL, 'USD', 0, 0, -2000.00,
             '{"Action": "DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('achin', 917, 'ACC1', 'DIRECT_DEPOSIT', NULL, 'USD', 0, 0, 2000.00,
             '{"Action": "DIRECT DEPOSIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('achrev', 918, 'ACC1', 'DIRECT_DEBIT', NULL, 'USD', 0, 0, 15.00,
             '{"Action": "DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('adr',  920, 'ACC1', 'FEE',     'XYZ', 'USD', 0, 0, -1.50,
             '{"Action": "FEE CHARGED EXAMPLE CORP SPON ADR (XYZ) (Cash)", "Description": "EXAMPLE CORP SPON ADR"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	got := map[string]canonical.TransactionChange{}
	for _, tx := range batch.Transactions {
		got[tx.TransactionExternalID] = tx
	}
	for id, want := range map[string]canonical.TxKind{
		"adv":  canonical.TxKindFee,
		"wire": canonical.TxKindWithdrawal,
		// Fidelity's verb carries no direction — both wires reduce to
		// `WIRE` — so the SIGN decides. It has to: an inbound wire
		// read as a withdrawal would be a credit in the spending
		// population, netting against the spend.
		"wirein": canonical.TxKindDeposit,
		// The ACH verbs name only the direction the ORIGINATING bank
		// saw, and a reversal books under the verb of the leg it
		// undoes — so, as with WIRE, the sign is what says which way
		// the money went. `achrev` is the case that makes this more
		// than tidiness: a DIRECT DEBIT carrying a credit.
		"achout": canonical.TxKindWithdrawal,
		"achin":  canonical.TxKindDeposit,
		"achrev": canonical.TxKindDeposit,
		"adr":    canonical.TxKindFee,
	} {
		if got[id].Kind != want {
			t.Errorf("%s kind = %q, want %q", id, got[id].Kind, want)
		}
	}

	// The narrative is what separates the two fees, so it has to
	// reach gold — without it a fidelity row carries no merchant, no
	// counterparty and nothing for a rule to match.
	for id, want := range map[string]string{
		"adv":    "ADVISOR FEE DEDUCTED Investment Mgr Fee (Cash)",
		"wire":   "WIRE TRANSFER TO BANK (Cash)",
		"wirein": "WIRE TRANSFER FROM BANK (Cash)",
		"achout": "DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK",
		"adr":    "FEE CHARGED EXAMPLE CORP SPON ADR (XYZ) (Cash)",
	} {
		if got[id].Description == nil {
			t.Errorf("%s carries no description; gold would have nothing to categorise it by", id)
			continue
		}
		if *got[id].Description != want {
			t.Errorf("%s description = %q, want %q", id, *got[id].Description, want)
		}
	}
}

// TestACorrectionNetsAgainstWhatItCorrects: Fidelity signs every
// amount from the account's side, so a row signed against its kind is
// a correction and keeps its sign. Forced to the kind's canonical
// sign, a cancelled sale would book as a second sale, a clawed-back
// dividend as a second dividend and a refunded fee as a second fee.
// An ADJUSTMENT books as the kind it corrects where its Action names
// one, and stays `other` where it does not.
func TestACorrectionNetsAgainstWhatItCorrects(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('sold',    900, 'ACC1', 'SELL',       'XYZ', 'USD', -10, 50, 500.00,
             '{"Action": "YOU SOLD EXAMPLE CORP COM (XYZ) (Cash)"}'),
            ('cancel',  901, 'ACC1', 'SELL',       'XYZ', 'USD', 10, 50, -500.00,
             '{"Action": "SELL CANCEL CANCELLED TRADE AS OF 01-02-24 EXAMPLE CORP COM (XYZ) (Cash)"}'),
            ('divadj',  902, 'ACC1', 'DIVIDEND',   'XYZ', 'USD', 0, 0, -12.00,
             '{"Action": "DIVIDEND ADJUSTMENT as of Jan-02-2024 EXAMPLE CORP COM (XYZ) (Cash)"}'),
            ('feeback', 903, 'ACC1', 'FEE',        'XYZ', 'USD', 0, 0, 7.00,
             '{"Action": "FEE CHARGED EXAMPLE CORP SPON ADR (XYZ) (Cash)"}'),
            ('ftax',    904, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, 3.00,
             '{"Action": "ADJ FOREIGN TAX PAID TAX RCLM EXAMPLE CORP SPON ADR (XYZ) (Cash)"}'),
            ('nrtax',   905, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, 4.00,
             '{"Action": "ADJ NON-RESIDENT TAX EXAMPLE CORP COM"}'),
            ('adrfee',  906, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, 6.00,
             '{"Action": "ADJUST FEE CHARGED as of Jan-02-2024 EXAMPLE CORP SPON ADR (XYZ) (Cash)"}'),
            ('advfee',  907, 'ACC1', 'ADJUSTMENT', NULL,  'USD', 0, 0, 100.00,
             '{"Action": "ADJUSTMENT FEE REVERSAL-EXAMPLE FEE"}'),
            ('divcut',  908, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, -2.00,
             '{"Action": "DIVIDEND ADJUSTMENT EXAMPLE CORP COM"}'),
            ('exer',    909, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, -5.00,
             '{"Action": "ADJUST EXERCISE EXAMPLE CORP SPON ADS (XYZ) (Cash)"}'),
            ('bare',    910, 'ACC1', 'ADJUSTMENT', 'XYZ', 'USD', 0, 0, 1.00,
             '{"Action": "ADJUSTMENT EXAMPLE CORP ADR EACH REPR 1 ORD"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	got := map[string]canonical.TransactionChange{}
	for _, tx := range batch.Transactions {
		got[tx.TransactionExternalID] = tx
	}
	for id, want := range map[string]struct {
		kind canonical.TxKind
		net  string
	}{
		"sold":    {canonical.TxKindSell, "500"},
		"cancel":  {canonical.TxKindSell, "-500"},
		"divadj":  {canonical.TxKindDividend, "-12"},
		"feeback": {canonical.TxKindFee, "7"},
		"ftax":    {canonical.TxKindTax, "3"},
		"nrtax":   {canonical.TxKindTax, "4"},
		"adrfee":  {canonical.TxKindFee, "6"},
		"advfee":  {canonical.TxKindFee, "100"},
		"divcut":  {canonical.TxKindDividend, "-2"},
		"exer":    {canonical.TxKindOther, "-5"},
		"bare":    {canonical.TxKindOther, "1"},
	} {
		tx, ok := got[id]
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if tx.Kind != want.kind {
			t.Errorf("%s kind = %q, want %q", id, tx.Kind, want.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != want.net {
			t.Errorf("%s net_amount = %v, want %s", id, tx.NetAmount, want.net)
		}
	}
}

// TestNarrativePrefersTheActionOverTheSecurity: Fidelity's Description
// column names the SECURITY, which on a fee or a withholding says
// nothing about the movement. The Action is the movement, so it leads
// and Description is only what is left when a row carries no action.
func TestNarrativePrefersTheActionOverTheSecurity(t *testing.T) {
	if got := parseTxPayload(
		`{"Action": "FOREIGN TAX PAID FOO (BAR)", "Description": "FOO ADR"}`,
	).narrative(); got != "FOREIGN TAX PAID FOO (BAR)" {
		t.Errorf("narrative = %q, want the Action", got)
	}
	if got := parseTxPayload(`{"Action": "  ", "Description": "FOO ADR"}`).narrative(); got != "FOO ADR" {
		t.Errorf("narrative = %q, want the Description fallback", got)
	}
	if got := parseTxPayload(`not json`).narrative(); got != "" {
		t.Errorf("narrative = %q, want empty on malformed payload", got)
	}
}

// TestInstrumentHintIsStatedNeverDerived pins where a transaction's
// instrument and instrument_hint come from, whatever the row's kind:
// the row's instrument_key when it has one, else the payload key a
// statement builder writes when its statements could not settle the
// instrument. A keyless export row names its security in Description,
// and that is NOT promoted to a hint — the live source's output must
// not change because another source shares its adapter.
func TestInstrumentHintIsStatedNeverDerived(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('stated', 900, 'ACC1', 'BUY', NULL, 'USD', 5, NULL, -500.00,
             '{"Action":"YOU BOUGHT EXAMPLE CO","Description":"EXAMPLE CO","InstrumentHint":"EXAMPLECO"}'),
            ('linked', 910, 'ACC1', 'BUY', 'AAAA', 'USD', 5, NULL, -500.00,
             '{"Action":"YOU BOUGHT EXAMPLE CO","InstrumentHint":"EXAMPLECO"}'),
            ('export', 920, 'ACC1', 'SELL', NULL, 'USD', -5, 100.00, 500.00,
             '{"Action":"YOU SOLD EXAMPLE CO","Description":"EXAMPLE CO","Symbol":""}'),
            ('div', 930, 'ACC1', 'DIVIDEND', 'AAAA', 'USD', NULL, NULL, 10.00,
             '{"Action":"DIVIDEND RECEIVED EXAMPLE CO","Description":"EXAMPLE CO"}'),
            ('tax', 940, 'ACC1', 'TAX', NULL, 'USD', NULL, NULL, -3.00,
             '{"Action":"NON-RESIDENT TAX OTHER CO","Description":"OTHER CO","InstrumentHint":"OTHERCO"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())
	type link struct{ instrument, hint string }
	got := map[string]link{}
	for _, x := range batch.Transactions {
		l := link{hint: x.InstrumentHint}
		if x.InstrumentExternalID != nil {
			l.instrument = *x.InstrumentExternalID
		}
		got[x.TransactionExternalID] = l
	}
	want := map[string]link{
		"stated": {hint: "EXAMPLECO"}, "linked": {instrument: "AAAA"}, "export": {},
		"div": {instrument: "AAAA"}, "tax": {hint: "OTHERCO"},
	}
	for id, l := range want {
		if got[id] != l {
			t.Errorf("%s: got %+v, want %+v", id, got[id], l)
		}
	}
}

// TestStatementConstructsHaveNoSymbol pins the display of keyless
// historical rows. A row a statement builder makes from a
// statement-level figure has no symbol — its synthetic key is not one
// — while a keyless SECURITY row keeps the synthetic key as its symbol,
// as it always has, and a keyed row shows its key.
func TestStatementConstructsHaveNoSymbol(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 4, 'svb-sleeves-build');
        INSERT INTO historical_position_snapshots(as_of_date, account_external_id, description, instrument_key, quantity, price, market_value, currency, payload) VALUES
            (1000, 'SVM-000000', 'NET CASH POSITION', NULL, NULL, NULL, 100.00, 'USD', '{}'),
            (1000, 'SVM-000000', 'NO POSITIONS', NULL, NULL, NULL, 0.00, 'USD', '{}'),
            (1000, '0000000000', 'CASH BALANCE', NULL, NULL, NULL, 200.00, 'USD', '{}'),
            (1000, '0000000001', 'MORTGAGE PRINCIPAL', NULL, NULL, NULL, -300.00, 'USD', '{}'),
            (1000, 'SVM-000001', 'ACCOUNT VALUE (ADVISOR MARK)', NULL, NULL, NULL, 400.00, 'USD', '{}'),
            (1000, 'SVM-000000', 'EXAMPLE COMPANY CL A', 'AAAA', 10, 5.0, 50.00, 'USD', '{}'),
            (1000, '9990001', 'Example Growth', NULL, 5, 10.0, 50.00, 'USD', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	byName := map[string]canonical.InstrumentChange{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range batch.Instruments {
			byName[*in.Name] = in
		}
		if !more {
			break
		}
	}
	for _, name := range []string{"NET CASH POSITION", "NO POSITIONS", "CASH BALANCE",
		"MORTGAGE PRINCIPAL", "ACCOUNT VALUE (ADVISOR MARK)"} {
		in, ok := byName[name]
		if !ok {
			t.Fatalf("%s: no instrument emitted", name)
		}
		if in.Symbol != nil {
			t.Errorf("%s: symbol = %q, want none", name, *in.Symbol)
		}
		if in.InstrumentExternalID != syntheticHistoricalInstrumentKey(name) {
			t.Errorf("%s: key = %q, want the synthetic one", name, in.InstrumentExternalID)
		}
	}
	if in := byName["NET CASH POSITION"]; in.AssetClass != canonical.AssetClassCash {
		t.Errorf("net cash class = %q, want cash", in.AssetClass)
	}
	if in := byName["EXAMPLE COMPANY CL A"]; in.Symbol == nil || *in.Symbol != "AAAA" {
		t.Errorf("keyed row symbol = %v, want AAAA", in.Symbol)
	}
	pool := byName["Example Growth"]
	if want := syntheticHistoricalInstrumentKey("Example Growth"); pool.Symbol == nil || *pool.Symbol != want {
		t.Errorf("keyless security symbol = %v, want the synthetic key %q", pool.Symbol, want)
	}
}
