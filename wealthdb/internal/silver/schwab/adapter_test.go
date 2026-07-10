package schwab

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

// newFixtureSilver creates a fresh on-disk silver SQLite under
// t.TempDir(), applies the embedded schema, and returns an open
// *sql.DB plus the path. The DB is closed by t.Cleanup so the
// adapter can re-open it via its own read-only DSN.
func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schwab.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seed DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("apply seed schema: %v", err)
	}
	return path, db
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	a := &Adapter{}
	conn, err := a.Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Adapter.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestKindIsSchwab(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "schwab" {
		t.Errorf("Kind() = %q, want %q", got, "schwab")
	}
}

func TestStatusEmpty(t *testing.T) {
	path, _ := newFixtureSilver(t)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
}

func TestStatusPopulated(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1000, 1, '/x/1'),
            (2000, 1, '/x/2'),
            (3000, 1, '/x/3');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC', 'TRADE',   '{"netAmount":-100}'),
            ('A2', 2500, 'ACC', 'JOURNAL', '{"netAmount":50}');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.OldestSnapshotAt != 1000 || s.LatestSnapshotAt != 3000 {
		t.Errorf("snapshot extrema = (%d,%d), want (1000,3000)", s.OldestSnapshotAt, s.LatestSnapshotAt)
	}
	if s.OldestTransactionAt != 1500 || s.LatestTransactionAt != 2500 {
		t.Errorf("tx extrema = (%d,%d), want (1500,2500)", s.OldestTransactionAt, s.LatestTransactionAt)
	}
	if s.LatestChangeNumber != 3000 {
		t.Errorf("change number = %d, want 3000", s.LatestChangeNumber)
	}
}

func TestChangeWindowNoChanges(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), 1000)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if w.HasChanges {
		t.Errorf("HasChanges = true, want false (nothing strictly newer than 1000)")
	}
	if w.NewChangeNumber != 1000 {
		t.Errorf("NewChangeNumber = %d, want 1000", w.NewChangeNumber)
	}
}

func TestChangeWindowAllChanges(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1'), (2000, 1, '/x/2');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC', 'TRADE', '{"netAmount":-100}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if !w.HasChanges {
		t.Fatal("HasChanges = false, want true")
	}
	if w.Start != 1000 || w.End != 2000 {
		t.Errorf("window = [%d, %d], want [1000, 2000]", w.Start, w.End)
	}
	if w.NewChangeNumber != 2000 {
		t.Errorf("NewChangeNumber = %d, want 2000", w.NewChangeNumber)
	}
}

func TestChangeWindowIncremental(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1000, 1, '/x/1'),
            (2000, 1, '/x/2');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC', 'TRADE', '{"netAmount":-100}'),
            ('A2', 2500, 'ACC', 'JOURNAL', '{"netAmount":50}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), 1000)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if !w.HasChanges {
		t.Fatal("expected changes since 1000")
	}
	if w.Start != 1500 || w.End != 2500 {
		t.Errorf("window = [%d, %d], want [1500, 2500] (covers new snapshot 2000 and tx 1500/2500)",
			w.Start, w.End)
	}
}

func TestSnapshotsBasic(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1","accountNumber":"redacted"}');
        INSERT INTO account_balances(snapshot_at, account_external_id, balance_kind, payload) VALUES
            (1000, 'ACC1', 'current', '{"cashBalance":1234.56}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL","description":"Apple Inc"}}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()

	batch, more, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if more {
		t.Errorf("more = true, want false (single snapshot)")
	}
	if len(batch.Accounts) != 1 || batch.Accounts[0].AccountExternalID != "ACC1" {
		t.Errorf("accounts = %+v", batch.Accounts)
	}
	if batch.Accounts[0].AccountKind != canonical.AccountKindBrokerage {
		t.Errorf("account kind = %q, want brokerage", batch.Accounts[0].AccountKind)
	}
	if len(batch.CashBalances) != 1 || batch.CashBalances[0].Amount.String() != "1234.56" {
		t.Errorf("cash_balances = %+v", batch.CashBalances)
	}
	if len(batch.Instruments) != 1 || batch.Instruments[0].InstrumentExternalID != "037833100" {
		t.Errorf("instruments = %+v", batch.Instruments)
	}
	if batch.Instruments[0].AssetClass != canonical.AssetClassEquity {
		t.Errorf("asset_class = %q, want equity", batch.Instruments[0].AssetClass)
	}
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %+v", batch.Positions)
	}
	p := batch.Positions[0]
	if p.PositionKey != "037833100" {
		t.Errorf("position key = %q", p.PositionKey)
	}
	if p.Quantity == nil || p.Quantity.String() != "10" {
		t.Errorf("quantity = %v, want 10", p.Quantity)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "1500" {
		t.Errorf("market_value = %v, want 1500", p.MarketValue)
	}
}

// TestSnapshotsAccountNicknameAndInstrumentEnrichment covers the
// schwab-api v3 enhancements: the promoted `nickname` column on
// accounts populates AccountChange.Nickname, and the optional
// `instruments` table fills in InstrumentChange.Name when the
// per-position descriptor has no description (typical for the
// EQUITY rows returned by /accounts).
func TestSnapshotsAccountNicknameAndInstrumentEnrichment(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload, account_type, preference_type, nickname) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1","accountNumber":"redacted"}', 'CASH', 'INDIVIDUAL', 'Main brokerage');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL","description":""}}');
        INSERT INTO instruments(snapshot_at, symbol, payload) VALUES
            (900, 'AAPL', '{"description":"Apple Inc (stale)"}'),
            (1000, 'AAPL', '{"description":"Apple Inc"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(batch.Accounts) != 1 {
		t.Fatalf("accounts = %+v", batch.Accounts)
	}
	if batch.Accounts[0].Nickname == nil || *batch.Accounts[0].Nickname != "Main brokerage" {
		t.Errorf("Nickname = %v, want 'Main brokerage'", batch.Accounts[0].Nickname)
	}
	if batch.Accounts[0].AccountCategory != nil {
		t.Errorf("AccountCategory = %v, want nil (Schwab leaves category to overrides)", batch.Accounts[0].AccountCategory)
	}
	if len(batch.Instruments) != 1 {
		t.Fatalf("instruments = %+v", batch.Instruments)
	}
	if batch.Instruments[0].Name == nil || *batch.Instruments[0].Name != "Apple Inc" {
		t.Errorf("Name = %v, want 'Apple Inc' (latest-known-per-symbol)", batch.Instruments[0].Name)
	}
}

func TestSnapshotsCashRouting(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', 'MMSXX',
             '{"longQuantity":5000,"shortQuantity":0,"marketValue":5000.00,
               "instrument":{"assetType":"CASH_EQUIVALENT","cusip":"","symbol":"MMSXX"}}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(batch.Positions) != 0 {
		t.Errorf("CASH_EQUIVALENT should not appear in positions, got %+v", batch.Positions)
	}
	if len(batch.CashBalances) != 1 || batch.CashBalances[0].Amount.String() != "5000" {
		t.Errorf("expected one cash_balance row of 5000, got %+v", batch.CashBalances)
	}
}

func TestSnapshotsAssetClassMapping(t *testing.T) {
	cases := []struct {
		schwabType     string
		instrumentType string
		want           canonical.AssetClass
	}{
		{"EQUITY", "", canonical.AssetClassEquity},
		{"ETF", "", canonical.AssetClassETF},
		{"MUTUAL_FUND", "", canonical.AssetClassFund},
		// Real Trader API dumps type ETFs as COLLECTIVE_INVESTMENT
		// with the ETF-ness one level down in instrument.type.
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", canonical.AssetClassETF},
		{"COLLECTIVE_INVESTMENT", "", canonical.AssetClassFund},
		{"COLLECTIVE_INVESTMENT", "UNIT_INVESTMENT_TRUST", canonical.AssetClassFund},
		{"BOND", "", canonical.AssetClassBond},
		{"OPTION", "", canonical.AssetClassOption},
		{"FUTURE", "", canonical.AssetClassFuture},
		{"INDEX", "", canonical.AssetClassOther},
		{"NEW_TYPE_2030", "", canonical.AssetClassOther},
	}
	for _, c := range cases {
		if got := assetClassFor(c.schwabType, c.instrumentType); got != c.want {
			t.Errorf("assetClassFor(%q, %q) = %q, want %q", c.schwabType, c.instrumentType, got, c.want)
		}
	}
}

func TestTransactionsKindMapping(t *testing.T) {
	negative := canonical.NewDecimalFromInt(-100)
	positive := canonical.NewDecimalFromInt(100)
	zero := canonical.NewDecimalFromInt(0)

	cases := []struct {
		schwabKind  string
		amount      canonical.Decimal
		description string
		want        canonical.TxKind
	}{
		{"TRADE", negative, "", canonical.TxKindBuy},
		{"TRADE", positive, "", canonical.TxKindSell},
		{"JOURNAL", zero, "", canonical.TxKindJournal},
		// DIVIDEND_OR_INTEREST splits by description.
		{"DIVIDEND_OR_INTEREST", positive, "VANGUARD S&P 500 ETF", canonical.TxKindDividend},
		{"DIVIDEND_OR_INTEREST", positive, "META PLATFORMS INC CLASS A", canonical.TxKindDividend},
		{"DIVIDEND_OR_INTEREST", positive, "BANK INT 010100-020100 SCHWAB BANK", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", positive, "SCHWAB1 INT 01/01-02/01", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", positive, "INTEREST 01/01THRU 02/01", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", negative, "MARGIN INTEREST 03/01THRU 04/01", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", positive, "US TREASU NT 9.999%01/99UST NOTE DUE 01/15/99", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", positive, "US TREASURY 9.999%01/99UST BOND DUE 01/15/99", canonical.TxKindInterest},
		{"DIVIDEND_OR_INTEREST", positive, "", canonical.TxKindDividend},
		{"WIRE_IN", positive, "", canonical.TxKindDeposit},
		{"WIRE_OUT", negative, "", canonical.TxKindWithdrawal},
		{"CASH_RECEIPT", positive, "", canonical.TxKindDeposit},
		{"CASH_DISBURSEMENT", negative, "", canonical.TxKindWithdrawal},
		{"ELECTRONIC_FUND", negative, "", canonical.TxKindWithdrawal},
		{"ELECTRONIC_FUND", positive, "", canonical.TxKindDeposit},
		{"RECEIVE_AND_DELIVER", negative, "", canonical.TxKindTransferOut},
		{"RECEIVE_AND_DELIVER", positive, "", canonical.TxKindTransferIn},
		{"SMA_ADJUSTMENT", zero, "", canonical.TxKindOther},
		{"MEMORANDUM", zero, "", canonical.TxKindOther},
	}
	for _, c := range cases {
		if got := kindFor(c.schwabKind, c.amount, c.description); got != c.want {
			t.Errorf("kindFor(%q, %s, %q) = %q, want %q", c.schwabKind, c.amount.String(), c.description, got, c.want)
		}
	}
}

func TestTransactionsEndToEnd(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC', 'TRADE',
             '{"netAmount":-1505.00,"transferItems":[
                {"instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"},
                 "amount":10,"cost":-1500.00,"price":150.00,"positionEffect":"OPENING"},
                {"instrument":{"assetType":"CURRENCY","cusip":"9ZZZFD494"},
                 "amount":-1505.00,"cost":-1505.00}]}'),
            ('A2', 2000, 'ACC', 'DIVIDEND_OR_INTEREST',
             '{"netAmount":50.00,"transferItems":[]}');
    `); err != nil {
		t.Fatal(err)
	}
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

	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Transactions) != 2 {
		t.Fatalf("got %d transactions, want 2", len(batch.Transactions))
	}

	// First: TRADE → buy (negative netAmount), instrument from EQUITY leg
	tx := batch.Transactions[0]
	if tx.Kind != canonical.TxKindBuy {
		t.Errorf("TX1 kind = %q, want buy", tx.Kind)
	}
	if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != "037833100" {
		t.Errorf("TX1 instrument = %v, want 037833100", tx.InstrumentExternalID)
	}
	if tx.Quantity == nil || tx.Quantity.String() != "10" {
		t.Errorf("TX1 quantity = %v, want 10", tx.Quantity)
	}
	if tx.Price == nil || tx.Price.String() != "150" {
		t.Errorf("TX1 price = %v, want 150", tx.Price)
	}

	// Second: DIVIDEND_OR_INTEREST → dividend, no transferItems
	tx = batch.Transactions[1]
	if tx.Kind != canonical.TxKindDividend {
		t.Errorf("TX2 kind = %q, want dividend", tx.Kind)
	}
	if tx.NetAmount == nil || tx.NetAmount.String() != "50" {
		t.Errorf("TX2 netAmount = %v, want 50", tx.NetAmount)
	}
}
