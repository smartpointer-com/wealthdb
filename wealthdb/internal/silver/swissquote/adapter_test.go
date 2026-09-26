package swissquote

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
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
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
	if byKey["IUSQ@USD"].Quantity == nil || byKey["IUSQ@USD"].Quantity.String() != "10" {
		t.Errorf("IUSQ quantity = %v, want 10", byKey["IUSQ@USD"].Quantity)
	}
	if byKey["IUSQ@USD"].MarketValue == nil || byKey["IUSQ@USD"].MarketValue.String() != "1500" {
		t.Errorf("IUSQ market_value = %v, want 1500", byKey["IUSQ@USD"].MarketValue)
	}
}

// TestSnapshotsTaxonomyPair verifies the 2-D taxonomy:
// each XLS section header maps to the expected (AssetClass,
// Vehicle) pair on BOTH the InstrumentChange and the matching
// PositionChange, the two agree, and every pair is admitted by
// canonical.ValidTaxonomyPair. Collective-vehicle sections ("ETFs",
// "Funds") get their exposure refined from the security name. All
// names/symbols here are synthetic placeholders.
func TestSnapshotsTaxonomyPair(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, '1234567', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload, name, isin) VALUES
            (1000, '1234567', 'SYNSHR', 'CHF',
             '{"asset_class":"Shares","currency":"CHF","symbol":"SYNSHR","quantity":1,"total_value":1}',
             'Placeholder Equity Co', NULL),
            (1000, '1234567', 'SYNETFEQ', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFEQ","quantity":1,"total_value":1}',
             'Placeholder Broad World ETF', NULL),
            (1000, '1234567', 'SYNETFAU', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFAU","quantity":1,"total_value":1}',
             'Placeholder Physical Gold ETF', NULL),
            (1000, '1234567', 'SYNETFBTC', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFBTC","quantity":1,"total_value":1}',
             'Placeholder Bitcoin ETF', NULL),
            (1000, '1234567', 'SYNBOND', 'CHF',
             '{"asset_class":"Bonds","currency":"CHF","symbol":"SYNBOND","quantity":1,"total_value":1}',
             'Placeholder Corp Note', NULL),
            (1000, '1234567', 'SYNFNDEQ', 'CHF',
             '{"asset_class":"Funds","currency":"CHF","symbol":"SYNFNDEQ","quantity":1,"total_value":1}',
             'Placeholder Growth Fund', NULL),
            (1000, '1234567', 'SYNFNDBND', 'CHF',
             '{"asset_class":"Funds","currency":"CHF","symbol":"SYNFNDBND","quantity":1,"total_value":1}',
             'Placeholder Government Bond Fund', NULL),
            (1000, '1234567', 'SYNOPT', 'USD',
             '{"asset_class":"Options","currency":"USD","symbol":"SYNOPT","quantity":1,"total_value":1}',
             'Placeholder Call Option', NULL),
            (1000, '1234567', 'SYNPM', 'CHF',
             '{"asset_class":"Precious Metals","currency":"CHF","symbol":"SYNPM","quantity":1,"total_value":1}',
             'Placeholder Bullion Bar', NULL),
            (1000, '1234567', 'SYNSP', 'CHF',
             '{"asset_class":"Structured Products","currency":"CHF","symbol":"SYNSP","quantity":1,"total_value":1}',
             'Placeholder Structured Note', NULL);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	type pair struct {
		ac  canonical.AssetClass
		veh canonical.Vehicle
	}
	want := map[string]pair{
		"SYNSHR@CHF":    {canonical.AssetClassPublicEquity, canonical.VehicleStock},
		"SYNETFEQ@USD":  {canonical.AssetClassPublicEquity, canonical.VehicleETF},
		"SYNETFAU@USD":  {canonical.AssetClassMetal, canonical.VehicleETF},
		"SYNETFBTC@USD": {canonical.AssetClassCrypto, canonical.VehicleETF},
		"SYNBOND@CHF":   {canonical.AssetClassFixedIncome, canonical.VehicleBond},
		"SYNFNDEQ@CHF":  {canonical.AssetClassPublicEquity, canonical.VehicleFund},
		"SYNFNDBND@CHF": {canonical.AssetClassFixedIncome, canonical.VehicleFund},
		"SYNOPT@USD":    {canonical.AssetClassPublicEquity, canonical.VehicleOption},
		"SYNPM@CHF":     {canonical.AssetClassMetal, canonical.VehiclePhysical},
		"SYNSP@CHF":     {canonical.AssetClassPublicEquity, canonical.VehicleStructuredProduct},
	}

	posByKey := map[string]canonical.PositionChange{}
	for _, p := range batch.Positions {
		posByKey[p.PositionKey] = p
	}
	instByKey := map[string]canonical.InstrumentChange{}
	for _, i := range batch.Instruments {
		instByKey[i.InstrumentExternalID] = i
	}
	if len(posByKey) != len(want) || len(instByKey) != len(want) {
		t.Fatalf("emitted %d positions / %d instruments, want %d each", len(posByKey), len(instByKey), len(want))
	}

	for key, wp := range want {
		if !canonical.ValidTaxonomyPair(wp.ac, wp.veh) {
			t.Fatalf("test bug: (%s, %s) is not a valid taxonomy pair", wp.ac, wp.veh)
		}
		p := posByKey[key]
		if p.AssetClass != wp.ac || p.Vehicle != wp.veh {
			t.Errorf("position %s: (AssetClass, Vehicle) = (%s, %s), want (%s, %s)",
				key, p.AssetClass, p.Vehicle, wp.ac, wp.veh)
		}
		i := instByKey[key]
		if i.AssetClass != wp.ac || i.Vehicle != wp.veh {
			t.Errorf("instrument %s: (AssetClass, Vehicle) = (%s, %s), want (%s, %s)",
				key, i.AssetClass, i.Vehicle, wp.ac, wp.veh)
		}
		// Instrument and position must agree on the pair.
		if p.AssetClass != i.AssetClass || p.Vehicle != i.Vehicle {
			t.Errorf("%s: position/instrument pair disagree: (%s,%s) vs (%s,%s)",
				key, p.AssetClass, p.Vehicle, i.AssetClass, i.Vehicle)
		}
	}
}

// TestSnapshotsInstrumentNameAndISIN verifies the swissquote
// v3 `name` and `isin` columns surface on the InstrumentChange,
// and that the per-bank identifier becomes the ISIN when one is
// known (column-level on this row, or inherited from another row
// sharing the same symbol+currency). Rows whose symbol+currency
// has no ISIN anywhere in the silver fall back to symbol@currency.
func TestSnapshotsInstrumentNameAndISIN(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, '1234567', '{"customer_id":"1234567"}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload, name, isin) VALUES
            (1000, '1234567', 'IUSQ', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"IUSQ","quantity":10,"total_value":1500.00}',
             'iShares Core MSCI World UCITS ETF', 'IE00B4L5Y983'),
            (1000, '1234567', 'PREMIG', 'CHF',
             '{"asset_class":"Shares","currency":"CHF","symbol":"PREMIG","quantity":50,"total_value":1000.00}',
             NULL, NULL);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	byKey := map[string]canonical.InstrumentChange{}
	for _, i := range batch.Instruments {
		byKey[i.InstrumentExternalID] = i
	}
	iusq, ok := byKey["IE00B4L5Y983"]
	if !ok {
		t.Fatalf("instrument with ISIN-keyed external_id 'IE00B4L5Y983' not found; got keys = %v", keysOf(byKey))
	}
	if iusq.Name == nil || *iusq.Name != "iShares Core MSCI World UCITS ETF" {
		t.Errorf("IUSQ name = %v, want 'iShares Core MSCI World UCITS ETF'", iusq.Name)
	}
	if iusq.ISIN == nil || *iusq.ISIN != "IE00B4L5Y983" {
		t.Errorf("IUSQ isin = %v, want 'IE00B4L5Y983'", iusq.ISIN)
	}
	if iusq.Symbol == nil || *iusq.Symbol != "IUSQ" {
		t.Errorf("IUSQ symbol = %v, want 'IUSQ'", iusq.Symbol)
	}
	premig, ok := byKey["PREMIG@CHF"]
	if !ok {
		t.Fatalf("ISIN-less instrument 'PREMIG@CHF' not found; got keys = %v", keysOf(byKey))
	}
	if premig.Name != nil {
		t.Errorf("PREMIG name = %v, want nil (pre-migration row)", premig.Name)
	}
	if premig.ISIN != nil {
		t.Errorf("PREMIG isin = %v, want nil (pre-migration row)", premig.ISIN)
	}
}

// TestSnapshotsHistoricalPositionsConsolidatedByISIN verifies the
// silver-migration-0004 historical positions (source='pp:...')
// land in the same gold position_key as the matching live row by
// virtue of the ISIN. Without this, the same logical instrument
// would fragment into two gold positions — historical rows store
// the long instrument name in the `symbol` column while live XLS
// rows store the ticker.
func TestSnapshotsHistoricalPositionsConsolidatedByISIN(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 4, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (2000, '1234567', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload, name, isin, source) VALUES
            -- Live row, ticker symbol.
            (2000, '1234567', 'SMMCHA', 'CHF',
             '{"asset_class":"ETFs","currency":"CHF","symbol":"SMMCHA","quantity":100,"total_value":10000.00}',
             'Example Fund CHF dis', 'CH0000000060', 'live'),
            -- Historical row from a Portfolio Performance PDF —
            -- silver writes the long name into the symbol column,
            -- which is a different (symbol, currency) tuple than
            -- the live row but shares the ISIN.
            (1500, '1234567', 'Example Fund CHF DIS', 'CHF',
             '{"asset_class":"ETFs","currency":"CHF","symbol":"Example Fund CHF DIS","quantity":80,"total_value":7500.00}',
             'Example Fund CHF DIS', 'CH0000000060', 'pp:doc-abc');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()

	var allPositions []canonical.PositionChange
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		allPositions = append(allPositions, batch.Positions...)
		if !more {
			break
		}
	}

	if len(allPositions) != 2 {
		t.Fatalf("positions = %d, want 2", len(allPositions))
	}
	for _, p := range allPositions {
		if p.PositionKey != "CH0000000060" {
			t.Errorf("position at %d has key %q, want 'CH0000000060' (ISIN-keyed)", p.SnapshotAt, p.PositionKey)
		}
	}
}

func keysOf(m map[string]canonical.InstrumentChange) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSnapshotsAccountCategoryPassthrough verifies the
// swissquote v2 `account_type` column is forwarded as
// AccountCategory, and that the empty string maps to nil (older
// snapshots predate the column).
func TestSnapshotsAccountCategoryPassthrough(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 2, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload, account_type) VALUES
            (1000, '1234567', '{"customer_id":"1234567"}', 'Trading'),
            (1000, '7654321', '{"customer_id":"7654321"}', '');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	byID := map[string]*string{}
	for i := range batch.Accounts {
		byID[batch.Accounts[i].AccountExternalID] = batch.Accounts[i].AccountCategory
	}
	if got := byID["1234567"]; got == nil || *got != "Trading" {
		t.Errorf("1234567 category = %v, want 'Trading'", got)
	}
	if got := byID["7654321"]; got != nil {
		t.Errorf("7654321 category = %v, want nil (empty account_type)", got)
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

// The export states a movement and, where one is involved, a security —
// nothing else a reader could use. Both reach gold: the booking type
// leads the narrative and is the provider category, so a row whose
// narrative is nothing more reads as the bank's own filing.
func TestTransactionsCarryANarrative(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(account_external_id, occurred_at, transaction_type, isin, symbol, currency, net_amount, payload) VALUES
            ('1234567', 1700, 'Dividend', 'IE00BJK9H753', 'IUSQ', 'USD', 25.00,
             '{"name":"iShares Core MSCI World"}'),
            ('1234567', 1900, 'Custody Fees', NULL, NULL, 'CHF', -30.00, '{"name":""}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	want := map[string]string{
		"Dividend":     "Dividend iShares Core MSCI World",
		"Custody Fees": "Custody Fees",
	}
	for _, tx := range batch.Transactions {
		if tx.ProviderCategory == nil {
			t.Fatalf("%s carries no provider category", tx.TransactionExternalID)
		}
		wantDesc := want[*tx.ProviderCategory]
		if tx.Description == nil || *tx.Description != wantDesc {
			t.Errorf("%s description = %v, want %q", *tx.ProviderCategory, tx.Description, wantDesc)
		}
	}
}
