package schwab

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// newFixtureSilver creates a fresh on-disk silver SQLite under
// t.TempDir(), applies the embedded schema, and returns an open
// *sql.DB plus the path. The DB is closed by t.Cleanup so the
// adapter can re-open it via its own read-only DSN.
func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

func TestKindIsSchwab(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "schwab" {
		t.Errorf("Kind() = %q, want %q", got, "schwab")
	}
}

// A configuration names the api subsource, the web one or both; with
// neither the source has nothing to read and is refused.
func TestOpenTakesEitherSubsource(t *testing.T) {
	path, _ := newFixtureSilver(t)
	a := &Adapter{}
	for _, kind := range []string{"schwab-api", "schwab-web"} {
		conn, err := a.Open(context.Background(), silver.OpenSpec{Subsources: []silver.Subsource{
			{Kind: kind, Path: path},
		}})
		if err != nil {
			t.Fatalf("Open with %s alone: %v", kind, err)
		}
		conn.Close()
	}
	if conn, err := a.Open(context.Background(), silver.OpenSpec{Subsources: []silver.Subsource{}}); err == nil {
		conn.Close()
		t.Fatal("Open accepted a source with no subsource")
	}
}

func TestStatusEmpty(t *testing.T) {
	silvertest.AssertEmptyStatus(t, &Adapter{}, silverSchemaSQL)
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
	// The first change is tx 1500; Start reaches back to the dump
	// snapshot before it, 1000, so the mark guard can correct it.
	if w.Start != 1000 || w.End != 2500 {
		t.Errorf("window = [%d, %d], want [1000, 2500] (covers tx 1500/2500, new snapshot 2000, "+
			"and the dump snapshot before the first change)", w.Start, w.End)
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
	// An EQUITY row is public_equity × stock on both the instrument
	// and the position (they must agree).
	if got := batch.Instruments[0].AssetClass; got != canonical.AssetClassPublicEquity {
		t.Errorf("instrument asset_class = %q, want public_equity", got)
	}
	if got := batch.Instruments[0].Vehicle; got != canonical.VehicleStock {
		t.Errorf("instrument vehicle = %q, want stock", got)
	}
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %+v", batch.Positions)
	}
	p := batch.Positions[0]
	if p.AssetClass != canonical.AssetClassPublicEquity || p.Vehicle != canonical.VehicleStock {
		t.Errorf("position (asset_class, vehicle) = (%q, %q), want (public_equity, stock)",
			p.AssetClass, p.Vehicle)
	}
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

// TestTaxonomyForLive pins the (exposure, vehicle) pairs the
// live-api path emits. Security names are synthetic placeholders;
// only the exposure keywords (BITCOIN / GOLD / TREASURY BOND /
// plain) matter to RefineETFExposure. Every emitted pair must
// satisfy canonical.ValidTaxonomyPair.
func TestTaxonomyForLive(t *testing.T) {
	cases := []struct {
		assetType   string
		instrType   string
		name        string
		wantClass   canonical.AssetClass
		wantVehicle canonical.Vehicle
	}{
		{"EQUITY", "", "", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		// COLLECTIVE_INVESTMENT + EXCHANGE_TRADED_FUND → etf, exposure by name.
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER BITCOIN ETF", canonical.AssetClassCrypto, canonical.VehicleETF},
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER GROWTH ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		// Schwab's abbreviated bond words read as fixed income.
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER TOTAL BD MKT ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER SHORT-TERM TREAS ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"COLLECTIVE_INVESTMENT", "EXCHANGE_TRADED_FUND", "PLACEHOLDER INFL PROT SECS ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"MUTUAL_FUND", "", "PLACEHOLDER INTERMEDIATE GOVT FD", canonical.AssetClassFixedIncome, canonical.VehicleFund},
		{"MUTUAL_FUND", "", "PLACEHOLDER GOVT MONEY MKT FD", canonical.AssetClassCash, canonical.VehicleFund},
		// Standalone ETF assetType behaves the same.
		{"ETF", "", "PLACEHOLDER GOLD ETF", canonical.AssetClassMetal, canonical.VehicleETF},
		// COLLECTIVE_INVESTMENT (other) / MUTUAL_FUND → fund, exposure by name.
		{"COLLECTIVE_INVESTMENT", "UNIT_INVESTMENT_TRUST", "PLACEHOLDER 500 INDEX FUND", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		{"MUTUAL_FUND", "", "PLACEHOLDER GOLD FUND", canonical.AssetClassMetal, canonical.VehicleFund},
		{"BOND", "", "", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"FIXED_INCOME", "", "", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"OPTION", "", "", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		{"FUTURE", "", "", canonical.AssetClassPublicEquity, canonical.VehicleFuture},
		{"INDEX", "", "", canonical.AssetClassOther, canonical.VehicleOther},
		{"NEW_TYPE_2030", "", "", canonical.AssetClassOther, canonical.VehicleOther},
	}
	for _, c := range cases {
		gotClass, gotVehicle := taxonomyFor(c.assetType, c.instrType, c.name)
		if gotClass != c.wantClass || gotVehicle != c.wantVehicle {
			t.Errorf("taxonomyFor(%q, %q, %q) = (%q, %q), want (%q, %q)",
				c.assetType, c.instrType, c.name, gotClass, gotVehicle, c.wantClass, c.wantVehicle)
		}
		if !canonical.ValidTaxonomyPair(gotClass, gotVehicle) {
			t.Errorf("taxonomyFor(%q, %q, %q) → invalid pair (%q, %q)",
				c.assetType, c.instrType, c.name, gotClass, gotVehicle)
		}
	}
}

// RefineClass reads OpenFIGI's type over the pair Schwab's type and the
// name give an instrument: a bond's or a share's type sets the pair; a
// fund's keeps an ETF or a fund and makes anything else a fund, its
// exposure read from the name; any other type, and a pair the taxonomy
// refuses, leaves the pair. Names are invented.
func TestRefineClassTakesTheLookupsTypeFirst(t *testing.T) {
	pe, fi := canonical.AssetClassPublicEquity, canonical.AssetClassFixedIncome
	cases := []struct {
		figi    string
		ac      canonical.AssetClass
		v       canonical.Vehicle
		name    string
		wantAC  canonical.AssetClass
		wantVeh canonical.Vehicle
	}{
		// A crypto trust Schwab types as an equity is a fund.
		{"Mutual Fund", pe, canonical.VehicleStock, "EXAMPLE BITCOIN TRUST", canonical.AssetClassCrypto, canonical.VehicleFund},
		{"ETP", pe, canonical.VehicleStock, "EXAMPLE ETHEREUM TRUST", canonical.AssetClassCrypto, canonical.VehicleETF},
		{"Closed-End Fund", pe, canonical.VehicleStock, "EXAMPLE MUNICIPAL INCOME", fi, canonical.VehicleFund},
		// An ETF Schwab types keeps its pair under OpenFIGI's fund type,
		// its exposure read from its abbreviated name.
		{"Mutual Fund", fi, canonical.VehicleETF, "EXAMPLE TOTAL BD MKT ETF", fi, canonical.VehicleETF},
		{"ETP", pe, canonical.VehicleFund, "EXAMPLE UNIT TRUST", pe, canonical.VehicleETF},
		{"Common Stock", pe, canonical.VehicleETF, "EXAMPLE HOLDINGS", pe, canonical.VehicleStock},
		{"Depositary Receipt", canonical.AssetClassOther, canonical.VehicleOther, "EXAMPLE ADR", pe, canonical.VehicleStock},
		{"Corp", pe, canonical.VehicleStock, "EXAMPLE 4% 2041", fi, canonical.VehicleBond},
		{"Option", pe, canonical.VehicleOption, "EXAMPLE CALL", pe, canonical.VehicleOption},
		{"", pe, canonical.VehicleStock, "EXAMPLE BITCOIN TRUST", pe, canonical.VehicleStock},
		// A money-market fund OpenFIGI calls an ETP stays a fund: cash is
		// no exchange-traded exposure.
		{"ETP", canonical.AssetClassCash, canonical.VehicleFund, "EXAMPLE MONEY MARKET", canonical.AssetClassCash, canonical.VehicleFund},
	}
	for _, c := range cases {
		ac, v := (&Adapter{}).RefineClass(c.figi, c.ac, c.v, c.name)
		if ac != c.wantAC || v != c.wantVeh {
			t.Errorf("RefineClass(%q, %s, %s, %q) = (%s, %s), want (%s, %s)",
				c.figi, c.ac, c.v, c.name, ac, v, c.wantAC, c.wantVeh)
		}
		if again, againV := (&Adapter{}).RefineClass(c.figi, ac, v, c.name); again != ac || againV != v {
			t.Errorf("RefineClass(%q) over its own (%s, %s) = (%s, %s)", c.figi, ac, v, again, againV)
		}
	}
}

// TestTaxonomyHistorical pins the statement-history shape classifier
// that produces the (exposure, vehicle) pairs for historical rows.
// Instrument keys and descriptions are synthetic placeholders shaped
// to trip each heuristic (OCC symbol, CUSIP-9, coupon line,
// money-market / word-ETF description, mutual-fund ticker). Every
// emitted pair must satisfy canonical.ValidTaxonomyPair.
func TestTaxonomyHistorical(t *testing.T) {
	cases := []struct {
		section     string
		key         string
		desc        string
		wantClass   canonical.AssetClass
		wantVehicle canonical.Vehicle
	}{
		// Authoritative statement `section` pins the vehicle.
		{"Equities", "TICKR", "PLACEHOLDER CORP", canonical.AssetClassPublicEquity, canonical.VehicleStock},
		// Name-shy bond ETF (no "ETF" token) — section keeps it an ETF,
		// exposure from the name.
		{"Exchange Traded Funds", "AGG", "PLACEHOLDER CORE AGGREGATE BOND", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"Exchange Traded Funds", "SPY", "PLACEHOLDER LARGE CAP", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"Exchange Traded Funds", "BND", "PLACEHOLDER TOTAL BD MKT", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		{"Investments", "EXBDX", "PLACEHOLDER TOTAL BD MKT INDEX", canonical.AssetClassFixedIncome, canonical.VehicleFund},
		{"Fixed Income", "EXBND", "PLACEHOLDER 2042", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		{"Options", "EXOPT", "PLACEHOLDER 50", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		// Ambiguous / absent section → fall back to key/description shapes.
		// OCC option symbol → equity option.
		{"", "XYZ250117C50000", "", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		// CALL/PUT description → equity option.
		{"Investments", "EXOPT", "CALL PLACEHOLDER CORP 50 EXP 01/17/25", canonical.AssetClassPublicEquity, canonical.VehicleOption},
		// Money-market sweep → cash × fund.
		{"", "EXMMF", "PLACEHOLDER GOVERNMENT MONEY MARKET", canonical.AssetClassCash, canonical.VehicleFund},
		// A money fund whose name lacks the MONEY MARKET token but
		// reads as a money fund → cash × fund (not a bond guess).
		{"Investments", "SWVXX", "PLACEHOLDER VALUE MONEY FUND", canonical.AssetClassCash, canonical.VehicleFund},
		// XX-ticker money fund whose truncated description names no
		// money token at all → still cash × fund.
		{"Investments", "EXMXX", "PLACEHOLDER VALUE ADVANTAGE◊", canonical.AssetClassCash, canonical.VehicleFund},
		// ETF-only issuer without the "ETF" token → etf; a 5-letter
		// X-ticker fund from the same issuer keeps the fund vehicle.
		{"Investments", "EXA", "ISHARES TRUST DJ US EXAMPLE", canonical.AssetClassPublicEquity, canonical.VehicleETF},
		{"Investments", "EXMPX", "VANGUARD EXAMPLE ADMIRAL SHARES", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		// CUSIP-9 key → fixed income bond.
		{"", "ABCDEFGH1", "", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		// Coupon-bearing description → fixed income bond.
		{"", "EXBND", "PLACEHOLDER NOTE 04.12500% 01/15/2042", canonical.AssetClassFixedIncome, canonical.VehicleBond},
		// Word-ETF description → exposure by name inside etf.
		{"", "EXETF", "PLACEHOLDER TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF},
		// Mutual-fund ticker (4 letters + X) → exposure by name inside fund.
		{"", "ABCDX", "PLACEHOLDER BITCOIN FUND", canonical.AssetClassCrypto, canonical.VehicleFund},
		{"", "WXYZX", "", canonical.AssetClassPublicEquity, canonical.VehicleFund},
		// Fall-through → plain equity stock.
		{"Other Assets", "TICKR", "PLACEHOLDER CORP", canonical.AssetClassPublicEquity, canonical.VehicleStock},
	}
	for _, c := range cases {
		gotClass, gotVehicle := taxonomyHistorical(c.section, c.key, c.desc)
		if gotClass != c.wantClass || gotVehicle != c.wantVehicle {
			t.Errorf("taxonomyHistorical(%q, %q, %q) = (%q, %q), want (%q, %q)",
				c.section, c.key, c.desc, gotClass, gotVehicle, c.wantClass, c.wantVehicle)
		}
		if !canonical.ValidTaxonomyPair(gotClass, gotVehicle) {
			t.Errorf("taxonomyHistorical(%q, %q, %q) → invalid pair (%q, %q)",
				c.section, c.key, c.desc, gotClass, gotVehicle)
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
		{"TRADE", zero, "System transfer", canonical.TxKindOther},
		{"JOURNAL", zero, "", canonical.TxKindJournal},
		{"JOURNAL", negative, "JOURNAL FRM EXAMPLE ACCOUNT", canonical.TxKindJournal},
		// A JOURNAL that withholds, charges or pays in lieu splits by
		// description, in Schwab's short spelling and in full.
		{"JOURNAL", negative, "NRA Tax Adj EXAMPLE ADR", canonical.TxKindTax},
		{"JOURNAL", positive, "PR YR NRA TAX EXAMPLE ADR", canonical.TxKindTax},
		{"JOURNAL", negative, "NON-RESIDENT ALIEN TAX", canonical.TxKindTax},
		{"JOURNAL", negative, "PRIOR YEAR TAX ADJ EXAMPLE ADR", canonical.TxKindTax},
		{"JOURNAL", negative, "FOREIGN TAX PAID EXAMPLE ADR", canonical.TxKindTax},
		{"JOURNAL", negative, "ADR Mgmt Fee EXAMPLE ADR", canonical.TxKindFee},
		{"JOURNAL", negative, "DEPOSITARY RECEIPT FEE", canonical.TxKindFee},
		{"JOURNAL", positive, "CASH IN LIEU OF FRACTIONAL SHARES", canonical.TxKindCorporateAction},
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
		{"SMA_ADJUSTMENT", zero, "", canonical.TxKindOther},
		{"MEMORANDUM", zero, "", canonical.TxKindOther},
	}
	for _, c := range cases {
		if got := kindFor(c.schwabKind, c.amount, nil, c.description); got != c.want {
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

// Withholding, its adjustment for a prior year, a foreign tax, a
// depositary's fee and cash in lieu reach the API as JOURNAL rows. Each
// books as its own kind, with the API's sign: the prior-year adjustment
// is positive. A JOURNAL that moves cash between accounts stays a
// journal. The rows are invented.
func TestJournalRowsBookByTheirDescription(t *testing.T) {
	_, seed := newFixtureSilver(t)
	silvertest.Exec(t, seed, `
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('J1', 1000, 'ACC', 'JOURNAL', '{"netAmount":-4.50,"description":"NRA Tax Adj EXAMPLE DEPOSITARY ADR"}'),
            ('J2', 1100, 'ACC', 'JOURNAL', '{"netAmount":1.25,"description":"Pr Yr NRA Tax EXAMPLE DEPOSITARY ADR"}'),
            ('J3', 1200, 'ACC', 'JOURNAL', '{"netAmount":-2.10,"description":"Foreign Tax Paid EXAMPLE DEPOSITARY ADR"}'),
            ('J4', 1300, 'ACC', 'JOURNAL', '{"netAmount":-0.80,"description":"ADR Mgmt Fee EXAMPLE DEPOSITARY ADR"}'),
            ('J5', 1400, 'ACC', 'JOURNAL', '{"netAmount":12.34,"description":"Cash In Lieu EXAMPLE HOLDINGS INC"}'),
            ('J6', 1500, 'ACC', 'JOURNAL', '{"netAmount":-500.00,"description":"JOURNAL TO EXAMPLE ACCOUNT"}'),
            ('J7', 1600, 'ACC', 'JOURNAL', '{"netAmount":-3.00,"description":"EXAMPLE HOLDINGS INC CLASS A","transferItems":[{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD"},"amount":-3.00}]}'),
            ('J8', 1700, 'ACC', 'JOURNAL', '{"netAmount":0.90,"description":"EXAMPLE HOLDINGS INC CLASS A"}'),
            ('J9', 1800, 'ACC', 'JOURNAL', '{"netAmount":-0.40,"description":"EXAMPLE CO LTD SPONSORED ADR"}'),
            ('J10', 1900, 'ACC', 'JOURNAL', '{"netAmount":-50.00,"description":"BANK SWEEP FR BROKERAGE"}'),
            ('J11', 2000, 'ACC', 'JOURNAL', '{"netAmount":75.00,"description":"TRF FDS TO TYPE 7"}'),
            ('J12', 2100, 'ACC', 'JOURNAL', '{"netAmount":0,"description":"EXAMPLE HOLDINGS INC CLASS A","transferItems":[{"instrument":{"assetType":"EQUITY","symbol":"EXMPL"},"amount":5}]}'),
            ('J13', 2200, 'ACC', 'JOURNAL', '{"netAmount":-20.00}'),
            ('J14', 2300, 'ACC', 'JOURNAL', '{"netAmount":-1.10,"description":"EXAMPLE SMALL-CAP VALUE ETF FUND"}'),
            ('J15', 2400, 'ACC', 'JOURNAL', '{"netAmount":-0.30,"description":"EXAMPLE CO LTD FUNSPONSORED"}'),
            ('J16', 2500, 'ACC', 'JOURNAL', '{"netAmount":-0.20,"description":"EXAMPLE TELECOM ADS"}'),
            ('J17', 2600, 'ACC', 'JOURNAL', '{"netAmount":-2.00,"description":"EXAMPLE BANK DEPOSITARY SHARES REPSTG 1/1000TH PFD"}'),
            ('J18', 2700, 'ACC', 'JOURNAL', '{"netAmount":0,"description":"EXAMPLE HOLDINGS INC CLASS A"}'),
            ('J19', 2800, 'ACC', 'JOURNAL', '{"netAmount":-900.00,"description":"J0EXCESS TO00000001"}'),
            ('J20', 2900, 'ACC', 'JOURNAL', '{"netAmount":900.00,"description":"J0EXCESS FR00000002"}');`)
	stream, err := (&apiReader{db: seed}).Transactions(context.Background(), everything)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"J1": "tax -4.5", "J2": "tax 1.25", "J3": "tax -2.1", "J4": "fee -0.8",
		"J5": "corporate_action 12.34", "J6": "journal -500",
		// A cash JOURNAL that names only a security: a depositary
		// receipt's is a fee, any other's a tax, refund or not. A move
		// of cash, a move of shares and a row with no description stay
		// journals.
		"J7": "tax -3", "J8": "tax 0.9", "J9": "fee -0.4",
		"J10": "journal -50", "J11": "journal 75", "J12": "journal 0", "J13": "journal -20",
		"J14": "tax -1.1", "J15": "fee -0.3", "J16": "fee -0.2", "J17": "tax -2", "J18": "journal 0",
		"J19": "journal -900", "J20": "journal 900",
	}
	if len(batch.Transactions) != len(want) {
		t.Fatalf("%d transactions, want %d", len(batch.Transactions), len(want))
	}
	for _, tx := range batch.Transactions {
		if got := string(tx.Kind) + " " + decStr(tx.NetAmount); got != want[tx.TransactionExternalID] {
			t.Errorf("%s: %s, want %s", tx.TransactionExternalID, got, want[tx.TransactionExternalID])
		}
	}
}

// TestADividendClawbackKeepsItsSign: the API signs netAmount from the
// account's side, so a negative dividend is Schwab taking one back.
// Forced to the dividend's canonical sign it would book as a second
// dividend instead of netting against the first.
func TestADividendClawbackKeepsItsSign(t *testing.T) {
	for _, c := range []struct {
		kind, payload, want string
	}{
		{"DIVIDEND_OR_INTEREST", `{"netAmount":-7.25,"description":"EXAMPLE EQUAL WEIGHT ETF"}`, "-7.25"},
		{"DIVIDEND_OR_INTEREST", `{"netAmount":7.25,"description":"EXAMPLE EQUAL WEIGHT ETF"}`, "7.25"},
		{"WIRE_OUT", `{"netAmount":-1000.00}`, "-1000"},
	} {
		tx, err := buildTransaction("A1", 1000, "ACC", c.kind, c.payload)
		if err != nil {
			t.Fatal(err)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != c.want {
			t.Errorf("%s %s: net_amount = %v, want %s", c.kind, c.payload, tx.NetAmount, c.want)
		}
	}
}

// A description two positions state under different keys resolves to
// neither, even when the instruments table states it too; a description
// only the instruments table states still resolves. Keys are invented.
func TestDescriptionResolverKeepsAmbiguityAcrossTables(t *testing.T) {
	_, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC', 'EXMPL0001', '{"instrument":{"assetType":"EQUITY","cusip":"EXMPL0001","description":"EXAMPLE HOLDINGS INC"}}'),
            (2000, 'ACC', 'EXMPL0002', '{"instrument":{"assetType":"EQUITY","cusip":"EXMPL0002","description":"EXAMPLE HOLDINGS INC"}}');
        INSERT INTO instruments(snapshot_at, symbol, payload) VALUES
            (1000, 'EXA', '{"cusip":"EXMPL0003","description":"EXAMPLE HOLDINGS INC"}'),
            (1000, 'OTH', '{"cusip":"EXMPL0004","description":"OTHER EXAMPLE CORP"}');
    `); err != nil {
		t.Fatal(err)
	}
	got, err := (&apiReader{db: seed}).buildDescriptionToInstrumentKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := got[normalizeSchwabDescription("EXAMPLE HOLDINGS INC")]; ok {
		t.Errorf("ambiguous description resolved to %q, want no key", key)
	}
	if key := got[normalizeSchwabDescription("OTHER EXAMPLE CORP")]; key != "EXMPL0004" {
		t.Errorf("instruments-only description = %q, want EXMPL0004", key)
	}
}

// A Treasury's coupon the API books as the account's interest, with no
// instrument, links to the one bond of the account its description names
// by coupon and maturity: the bond's description, its maturityDate,
// which wins over the description's date, and its variableRate. Cash
// interest, a bond the account does not hold and a coupon two bonds
// could pay stay unlinked. Keys, descriptions and terms are invented.
func TestACouponLinksToItsBond(t *testing.T) {
	_, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC', '000000001', '{"instrument":{"assetType":"FIXED_INCOME","cusip":"000000001","description":"US TREASURY NOTE 9.875% 09/09/99"}}'),
            (1000, 'ACC', '000000002', '{"instrument":{"assetType":"FIXED_INCOME","cusip":"000000002","description":"US TREASURY BOND 08/01/98","maturityDate":"2098-08-08T05:00:00.000+00:00","variableRate":9.75}}'),
            (1000, 'ACC', '000000003', '{"instrument":{"assetType":"FIXED_INCOME","cusip":"000000003","description":"US TREASURY NOTE 1.125% 07/07/97"}}'),
            (1000, 'ACC', '000000004', '{"instrument":{"assetType":"FIXED_INCOME","cusip":"000000004","description":"US TREASURY NOTE 1.125% 07/07/97"}}'),
            (1000, 'OTH', '000000005', '{"instrument":{"assetType":"FIXED_INCOME","cusip":"000000005","description":"US TREASURY NOTE 1.375% 03/03/97"}}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('C1', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":98.75,"description":"US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99"}'),
            ('C2', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":97.50,"description":"US TREASURY 9.75%08/98UST BOND DUE 08/08/98"}'),
            ('C3', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":11.25,"description":"US TREASU NT 1.125%07/97UST NOTE DUE 07/07/97"}'),
            ('C4', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":13.75,"description":"US TREASU NT 1.375%03/97UST NOTE DUE 03/03/97"}'),
            ('C5', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":3.10,"description":"SCHWAB1 INT 01/01-02/01"}'),
            ('C6', 2000, 'ACC', 'DIVIDEND_OR_INTEREST', '{"netAmount":2.40,"description":"BANK INT 010100-020100 SCHWAB BANK"}');
    `); err != nil {
		t.Fatal(err)
	}
	stream, err := (&apiReader{db: seed}).Transactions(context.Background(),
		canonical.Window{HasChanges: true, Start: 0, End: 9999})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tx := range batch.Transactions {
		if tx.Kind != canonical.TxKindInterest {
			t.Errorf("%s: kind %s", tx.TransactionExternalID, tx.Kind)
		}
		got[tx.TransactionExternalID] = ""
		if tx.InstrumentExternalID != nil {
			got[tx.TransactionExternalID] = *tx.InstrumentExternalID
		}
	}
	want := map[string]string{"C1": "000000001", "C2": "000000002", "C3": "", "C4": "", "C5": "", "C6": ""}
	for id, key := range want {
		if got[id] != key {
			t.Errorf("%s links %q, want %q", id, got[id], key)
		}
	}
}

// A bond's instrument states the maturity and the coupon the API gives
// it under the payload keys the yield pass reads terms by; a bond the API
// gives neither, and any other instrument, state none. The terms are
// invented.
func TestABondsInstrumentStatesItsTerms(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC', '{"hashValue":"ACC"}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC', '000000002', '{"longQuantity":1000,"shortQuantity":0,"marketValue":990.00,
               "instrument":{"assetType":"FIXED_INCOME","cusip":"000000002","description":"US TREASURY BOND",
               "maturityDate":"2098-08-08T05:00:00.000+00:00","variableRate":9.75}}'),
            (1000, 'ACC', '000000003', '{"longQuantity":1000,"shortQuantity":0,"marketValue":990.00,
               "instrument":{"assetType":"FIXED_INCOME","cusip":"000000003","description":"US TREASURY NOTE 1.125% 07/07/97"}}'),
            (1000, 'ACC', '000000009', '{"longQuantity":10,"shortQuantity":0,"marketValue":100.00,
               "instrument":{"assetType":"EQUITY","cusip":"000000009","symbol":"EXMP","description":"EXAMPLE CO"}}');
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
	got := map[string]string{}
	for _, i := range batch.Instruments {
		got[i.InstrumentExternalID] = string(i.Payload)
	}
	want := map[string]string{"000000002": `{"interest_rate":"9.75","maturity_date":"2098-08-08"}`, "000000003": "",
		"000000009": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s payload %s, want %s", k, got[k], v)
		}
	}
}

// A bond the instruments table describes reaches gold as an instrument
// with its name and its terms, though no position holds it: a statement
// names it by the same CUSIP and leaves both empty. Another instrument
// the table describes adds nothing. The rows are invented.
func TestABondNoPositionHoldsStatesItsTerms(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO instruments(snapshot_at, symbol, payload) VALUES
            (1000, '000000007', '{"symbol":"000000007","cusip":"000000007","assetType":"FIXED_INCOME",
               "description":"US Treasury 9.75% 08/08/2098","maturityDate":"2098-08-08T05:00:00.000+00:00","variableRate":9.75}'),
            (1000, 'EXMP', '{"symbol":"EXMP","cusip":"000000009","assetType":"EQUITY","description":"EXAMPLE CO"}');
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
	if len(batch.Instruments) != 1 {
		t.Fatalf("instruments %+v", batch.Instruments)
	}
	i := batch.Instruments[0]
	if i.InstrumentExternalID != "000000007" || i.Vehicle != canonical.VehicleBond || i.Name == nil ||
		*i.Name != "US Treasury 9.75% 08/08/2098" ||
		string(i.Payload) != `{"interest_rate":"9.75","maturity_date":"2098-08-08"}` {
		t.Errorf("the bond %+v %s", i, i.Payload)
	}
}
