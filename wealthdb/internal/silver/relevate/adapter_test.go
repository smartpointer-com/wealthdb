package relevate

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
	path := filepath.Join(t.TempDir(), "relevate.db")
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

func TestKindIsRelevate(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "relevate" {
		t.Errorf("Kind() = %q, want %q", got, "relevate")
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

// TestSnapshotsDerivedMarketValue covers relevate's distinctive
// projection: positions carry a target allocation, not a held
// quantity, so the adapter derives market_value = the account's
// securities balance × the fund's allocation. The 'cash' balance
// becomes a CashBalanceChange; the account is brokerage /
// vested_benefits / automated. asset_class maps Stocks→equity,
// everything else→fund.
func TestSnapshotsDerivedMarketValue(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, currency_code, name, product_name, management_style, payload) VALUES
            (1000, 'ACC1', 'CHF', 'Strategy 50', 'PensFree', NULL, '{}');
        INSERT INTO cash_balances(snapshot_at, account_external_id, currency, balance_kind, amount, payload) VALUES
            (1000, 'ACC1', 'CHF', 'cash', 200.00, NULL),
            (1000, 'ACC1', 'CHF', 'securities', 1000.00, NULL);
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, isin, asset_class, allocation, payload) VALUES
            (1000, 'ACC1', 'I1', 'CH0000000001', 'Stocks', 0.6, '{}'),
            (1000, 'ACC1', 'I2', '',             'Strategy', 0.4, '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('I1', 'CH0000000001', 'Equity Fund', 'Stocks', 1000, 1000, '{}'),
            ('I2', '', 'Strategy Fund', 'Strategy', 1000, 1000, '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	// Account taxonomy.
	if len(batch.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(batch.Accounts))
	}
	a := batch.Accounts[0]
	if a.AccountKind != canonical.AccountKindBrokerage {
		t.Errorf("account_kind = %q, want brokerage", a.AccountKind)
	}
	if a.TaxWrapper == nil || *a.TaxWrapper != canonical.TaxWrapperVestedBenefits {
		t.Errorf("tax_wrapper = %v, want vested_benefits", a.TaxWrapper)
	}
	if a.ManagementStyle == nil || *a.ManagementStyle != canonical.ManagementStyleAutomated {
		t.Errorf("management_style = %v, want automated", a.ManagementStyle)
	}

	// Derived positions: market_value = securities(1000) × allocation.
	byKey := map[string]canonical.PositionChange{}
	for _, p := range batch.Positions {
		byKey[p.PositionKey] = p
	}
	if len(byKey) != 2 {
		t.Fatalf("positions = %d, want 2", len(byKey))
	}
	// I1 keyed by ISIN, Stocks→public_equity (fund wrapper), 1000×0.6 = 600.
	i1 := byKey["CH0000000001"]
	if i1.AssetClass != canonical.AssetClassPublicEquity {
		t.Errorf("I1 asset_class = %q, want public_equity", i1.AssetClass)
	}
	if i1.MarketValue == nil || i1.MarketValue.String() != "600" {
		t.Errorf("I1 market_value = %v, want 600", i1.MarketValue)
	}
	// I2 keyed by internal id (no ISIN), default label→multi_asset, 1000×0.4 = 400.
	i2 := byKey["I2"]
	if i2.AssetClass != canonical.AssetClassMultiAsset {
		t.Errorf("I2 asset_class = %q, want multi_asset", i2.AssetClass)
	}
	if i2.MarketValue == nil || i2.MarketValue.String() != "400" {
		t.Errorf("I2 market_value = %v, want 400", i2.MarketValue)
	}

	// 'cash' balance → CashBalanceChange; 'securities' is not a cash row.
	if len(batch.CashBalances) != 1 {
		t.Fatalf("cash_balances = %d, want 1 (cash only)", len(batch.CashBalances))
	}
	c := batch.CashBalances[0]
	if c.Currency != "CHF" || c.Amount.String() != "200" {
		t.Errorf("cash = %s %s, want CHF 200", c.Currency, c.Amount.String())
	}
}

// TestTransactionsCreditNote exercises the credit-note adapter
// path: silver `kind='contribution'` (Pillar-2 vocabulary) projects
// to canonical TxKindDeposit (cash IN to the account, positive
// NetAmount per sign convention).
func TestTransactionsCreditNote(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 1, '/x/1');
        INSERT INTO transactions (transaction_external_id, snapshot_at, occurred_at,
            account_external_id, instrument_external_id, kind, currency,
            gross_amount, net_amount, quantity, price, source, payload)
        VALUES ('credit_note:42', 2000, 1500, 'ACC1', NULL, 'contribution',
                'CHF', 1234.56, 1234.56, NULL, NULL, 'credit_note_pdf', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	if !w.HasChanges {
		t.Fatalf("ChangeWindow.HasChanges = false, want true (transaction at occurred_at=1500 should trigger a window)")
	}
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())
	if len(batch.Transactions) != 1 {
		t.Fatalf("transactions = %d, want 1", len(batch.Transactions))
	}
	tx := batch.Transactions[0]
	if tx.Kind != canonical.TxKindDeposit {
		t.Errorf("kind = %q, want %q", tx.Kind, canonical.TxKindDeposit)
	}
	if tx.AccountExternalID != "ACC1" {
		t.Errorf("account = %q, want ACC1", tx.AccountExternalID)
	}
	if tx.Currency != "CHF" {
		t.Errorf("currency = %q, want CHF", tx.Currency)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "1234.56" {
		got := "<nil>"
		if tx.NetAmount != nil {
			got = tx.NetAmount.String()
		}
		t.Errorf("net_amount = %s, want 1234.56", got)
	}
	if tx.InstrumentExternalID != nil {
		t.Errorf("instrument = %v, want nil (cash event)", *tx.InstrumentExternalID)
	}
}

// Every Relevate sleeve is structurally a fund; the label (and,
// for "Alternatives", the fund name) picks the exposure class.
func TestAssetClassMapping(t *testing.T) {
	cases := []struct {
		raw, name string
		want      canonical.AssetClass
	}{
		{"Stocks", "Swisscanto (CH) Index Equity Fund Placeholder CHF", canonical.AssetClassEquity},
		{"Bonds", "Swisscanto (CH) Index Bond Fund Placeholder CHF", canonical.AssetClassBond},
		{"Liquidity", "Liquidity in CHF", canonical.AssetClassMoneyMarket},
		// The physical-bullion sleeve hides under "Alternatives".
		{"Alternatives", "Swisscanto (CH) Index Precious Metal Fund Gold Physical CHF hedged", canonical.AssetClassMetal},
		{"Alternatives", "Swisscanto (CH) Placeholder Hedge Strategies Fund CHF", canonical.AssetClassFund},
		// Indirect listed real estate stays fund — `real_estate`
		// is reserved for directly-held property.
		{"Real Estate", "Swisscanto (CH) Index Real Estate Fund Placeholder CHF", canonical.AssetClassFund},
		{"Something New", "Swisscanto (CH) Future Sleeve CHF", canonical.AssetClassFund},
	}
	for _, c := range cases {
		if got := assetClassFor(c.raw, c.name); got != c.want {
			t.Errorf("assetClassFor(%q, %q) = %q, want %q", c.raw, c.name, got, c.want)
		}
	}
}

// TestTaxonomyMapping covers the 2-D (exposure, vehicle) pair the
// migration double-writes alongside the legacy asset_class. Every
// sleeve is a Swisscanto index fund → vehicle `fund`, except the
// uninvested Liquidity sleeve → `cash`/`demand_deposit`; the label
// (and, for Alternatives, the fund name) picks the exposure.
func TestTaxonomyMapping(t *testing.T) {
	cases := []struct {
		raw, name string
		wantAC    canonical.AssetClass
		wantVeh   canonical.Vehicle
	}{
		{"Stocks", "Swisscanto (CH) Index Equity Fund Placeholder CHF", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		{"Bonds", "Swisscanto (CH) Index Bond Fund Placeholder CHF", canonical.AssetClassFixedIncome, canonical.VehicleFund},
		{"Liquidity", "Liquidity in CHF", canonical.AssetClassCash, canonical.VehicleDemandDeposit},
		{"Liquidity ", "Liquidity in CHF", canonical.AssetClassCash, canonical.VehicleDemandDeposit},
		// The physical-bullion sleeve hides under "Alternatives".
		{"Alternatives", "Swisscanto (CH) Index Precious Metal Fund Gold Physical CHF hedged", canonical.AssetClassMetal, canonical.VehicleFund},
		// Any other Alternatives sleeve is a manager-strategy fund.
		{"Alternatives", "Swisscanto (CH) Placeholder Hedge Strategies Fund CHF", canonical.AssetClassHedgeFund, canonical.VehicleFund},
		// Indirect listed real estate: real_estate exposure, fund wrapper.
		{"Real Estate", "Swisscanto (CH) Index Real Estate Fund Placeholder CHF", canonical.AssetClassRealEstate, canonical.VehicleFund},
		// Unrecognised label defaults to a blended robo sleeve.
		{"Something New", "Swisscanto (CH) Future Sleeve CHF", canonical.AssetClassMultiAsset, canonical.VehicleFund},
	}
	for _, c := range cases {
		gotAC, gotVeh := taxonomyFor(c.raw, c.name)
		if gotAC != c.wantAC || gotVeh != c.wantVeh {
			t.Errorf("taxonomyFor(%q, %q) = (%q, %q), want (%q, %q)",
				c.raw, c.name, gotAC, gotVeh, c.wantAC, c.wantVeh)
		}
		if !canonical.ValidTaxonomyPair(gotAC, gotVeh) {
			t.Errorf("taxonomyFor(%q, %q) = (%q, %q) is not a valid taxonomy pair",
				c.raw, c.name, gotAC, gotVeh)
		}
	}
}
