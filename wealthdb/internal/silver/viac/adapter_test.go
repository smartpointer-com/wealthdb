package viac

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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

// TestHistoricalSnapshotsSpanWindow covers the PDF-backfill
// behaviour (silver schema v3): positions tagged
// source='report:<docid>' carry a PAST as-of snapshot_at, and the
// adapter must surface them as additional position snapshots so a
// gold as-of query before live scraping began still finds holdings.
//
// The subtle case is the INCREMENTAL load: a new dump (advancing the
// watermark) arrives carrying a report whose as-of date is in the
// past. A naive (since, now] delta window would skip it; ChangeWindow
// must widen to the full snapshot history whenever there's new work.
func TestHistoricalSnapshotsSpanWindow(t *testing.T) {
	path, seed := newFixtureSilver(t)
	// Live dump at t=2000 with a live holding; a Reporting PDF
	// reconstructs a historical holding at t=1000 (an earlier
	// period-end). A SECOND live dump at t=3000 carries a NEW report
	// reconstructing a holding at t=1500 — also in the past.
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (2000, 3, '/x/2000'), (3000, 3, '/x/3000');
        INSERT INTO accounts(snapshot_at, account_external_id, product_code, name, state, currency_code, management_style, payload) VALUES
            (2000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}'),
            (3000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, asset_class, currency_code, name, quantity, market_value_chf, acquisition_price, asset_price, source, payload) VALUES
            (1000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 5, 600.00, 100.00, 120.00, 'report:DOC1000', '{}'),
            (1500, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 7, 900.00, 100.00, 128.00, 'report:DOC1500', '{}'),
            (2000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 10, 1500.00, 120.00, 150.00, 'live', '{}'),
            (3000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 11, 1700.00, 120.00, 155.00, 'live', '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, currency_code, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Old Fund', 'CHF', 'equity', 1000, 3000, '{}');
        INSERT INTO cash_balances(snapshot_at, account_external_id, currency, balance_kind, amount, source, payload) VALUES
            (1000, 'P3A1', 'CHF', 'cash', 50.00, 'report:DOC1000', NULL),
            (2000, 'P3A1', 'CHF', 'cash', 80.00, 'live', NULL);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	ctx := context.Background()

	// Status: oldest snapshot is the report date (1000), not the
	// oldest dump_run (2000); change number stays the latest dump.
	s, err := conn.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestSnapshotAt != 1000 {
		t.Errorf("OldestSnapshotAt = %d, want 1000 (report date)", s.OldestSnapshotAt)
	}
	if s.LatestChangeNumber != 3000 {
		t.Errorf("LatestChangeNumber = %d, want 3000 (latest dump)", s.LatestChangeNumber)
	}

	// Fresh load (since=-1): window spans the whole history.
	collect := func(since int64) map[int64]bool {
		w, err := conn.ChangeWindow(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		if !w.HasChanges {
			t.Fatalf("ChangeWindow(%d): HasChanges=false, want true", since)
		}
		stream, err := conn.Snapshots(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		seen := map[int64]bool{}
		for {
			batch, more, err := stream.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range batch.Positions {
				seen[p.SnapshotAt] = true
			}
			if !more {
				break
			}
		}
		return seen
	}

	fresh := collect(-1)
	for _, ts := range []int64{1000, 1500, 2000, 3000} {
		if !fresh[ts] {
			t.Errorf("fresh load: missing position snapshot at %d", ts)
		}
	}

	// Incremental load (since=2000, the prior watermark): a new dump
	// at 3000 is the trigger, but the window must still reach back to
	// the past-dated report snapshots (1000, 1500) so they aren't
	// lost on an incremental refresh.
	incr := collect(2000)
	for _, ts := range []int64{1000, 1500, 2000, 3000} {
		if !incr[ts] {
			t.Errorf("incremental load: missing position snapshot at %d", ts)
		}
	}

	// Idle reload (since=3000, current watermark, no new work): no-op.
	w, err := conn.ChangeWindow(ctx, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges {
		t.Errorf("ChangeWindow(3000): HasChanges=true, want false (idle reload)")
	}
}

// TestSnapshotsTaxonomyPairs asserts the 2-D taxonomy
// (asset_class, vehicle) the adapter emits, one representative row
// per taxonomyFor branch. It checks the instrument and the position
// agree on the pair, and that every emitted pair is admitted by
// canonical.ValidTaxonomyPair. All instrument names are synthetic
// placeholders — never a real VIAC fund.
func TestSnapshotsTaxonomyPairs(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 3, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, product_code, name, state, currency_code, management_style, payload) VALUES
            (1000, 'INV1', '1', 'Free Invest', 'ACTIVE', 'CHF', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, asset_class, currency_code, name, quantity, market_value_chf, acquisition_price, asset_price, payload) VALUES
            (1000, 'INV1', 'CH0000000001', 'equity',       'CHF', 'Synthetic Equity Index Fund',            1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000002', 'bond',         'CHF', 'Synthetic Bond Index Fund',              1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000003', 'fund',         'CHF', 'Synthetic Property Index Fund',          1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000004', 'equity',       'CHF', 'Placeholder Listed Private Equity Fund', 1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000005', 'metal',        'CHF', 'Synthetic Metal Index Fund',             1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000006', 'money_market', 'CHF', 'Synthetic Money Market Fund',            1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000007', 'other',        'CHF', 'Synthetic Bitcoin Basket',               1, 1, 1, 1, '{}'),
            (1000, 'INV1', 'CH0000000008', 'other',        'CHF', 'Synthetic Mystery Alternatives',         1, 1, 1, 1, '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, currency_code, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Synthetic Equity Index Fund',            'CHF', 'equity',       1000, 1000, '{}'),
            ('CH0000000002', 'CH0000000002', 'Synthetic Bond Index Fund',              'CHF', 'bond',         1000, 1000, '{}'),
            ('CH0000000003', 'CH0000000003', 'Synthetic Property Index Fund',          'CHF', 'fund',         1000, 1000, '{}'),
            ('CH0000000004', 'CH0000000004', 'Placeholder Listed Private Equity Fund', 'CHF', 'equity',       1000, 1000, '{}'),
            ('CH0000000005', 'CH0000000005', 'Synthetic Metal Index Fund',             'CHF', 'metal',        1000, 1000, '{}'),
            ('CH0000000006', 'CH0000000006', 'Synthetic Money Market Fund',            'CHF', 'money_market', 1000, 1000, '{}'),
            ('CH0000000007', 'CH0000000007', 'Synthetic Bitcoin Basket',               'CHF', 'other',        1000, 1000, '{}'),
            ('CH0000000008', 'CH0000000008', 'Synthetic Mystery Alternatives',         'CHF', 'other',        1000, 1000, '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	type pair struct {
		class   canonical.AssetClass
		vehicle canonical.Vehicle
	}
	want := map[string]pair{
		"CH0000000001": {canonical.AssetClassPublicEquity, canonical.VehicleFund},
		"CH0000000002": {canonical.AssetClassFixedIncome, canonical.VehicleFund},
		"CH0000000003": {canonical.AssetClassRealEstate, canonical.VehicleFund},
		"CH0000000004": {canonical.AssetClassPrivateEquity, canonical.VehicleETF},
		"CH0000000005": {canonical.AssetClassMetal, canonical.VehicleFund},
		"CH0000000006": {canonical.AssetClassCash, canonical.VehicleFund},
		"CH0000000007": {canonical.AssetClassCrypto, canonical.VehicleFund},
		"CH0000000008": {canonical.AssetClassOther, canonical.VehicleOther},
	}

	// Instruments: assert the emitted (asset_class, vehicle) pair and
	// that it's admitted by canonical.ValidTaxonomyPair.
	instByID := map[string]canonical.InstrumentChange{}
	for _, in := range batch.Instruments {
		instByID[in.InstrumentExternalID] = in
	}
	for id, exp := range want {
		in, ok := instByID[id]
		if !ok {
			t.Errorf("instrument %s missing from batch", id)
			continue
		}
		if in.AssetClass != exp.class || in.Vehicle != exp.vehicle {
			t.Errorf("instrument %s pair = (%q, %q), want (%q, %q)",
				id, in.AssetClass, in.Vehicle, exp.class, exp.vehicle)
		}
		if !canonical.ValidTaxonomyPair(in.AssetClass, in.Vehicle) {
			t.Errorf("instrument %s pair (%q, %q) not admitted by ValidTaxonomyPair",
				id, in.AssetClass, in.Vehicle)
		}
	}
	// The listed-PE ETF carries private-equity exposure via an etf
	// wrapper (the name check overrides viac's coarse fund default).
	if got := instByID["CH0000000004"].AssetClass; got != canonical.AssetClassPrivateEquity {
		t.Errorf("PE ETF asset_class = %q, want private_equity", got)
	}

	// Positions: must carry the same pair as their instrument.
	posByID := map[string]canonical.PositionChange{}
	for _, p := range batch.Positions {
		if p.InstrumentExternalID != nil {
			posByID[*p.InstrumentExternalID] = p
		}
	}
	for id, exp := range want {
		p, ok := posByID[id]
		if !ok {
			t.Errorf("position %s missing from batch", id)
			continue
		}
		if p.AssetClass != exp.class || p.Vehicle != exp.vehicle {
			t.Errorf("position %s pair = (%q, %q), want (%q, %q)",
				id, p.AssetClass, p.Vehicle, exp.class, exp.vehicle)
		}
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

// TestTransactionsCarryTheInstrumentTheyNamed covers the link silver
// resolves at load. VIAC names the fund in free text and nowhere else,
// so a trade that did not carry the id reached gold as an untracked
// destination — which is what the third row still is, and should be.
func TestTransactionsCarryTheInstrumentTheyNamed(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 5, '/x/1');
        INSERT INTO instruments(instrument_external_id, isin, name, currency_code, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Example Equity Index', 'CHF', 'equity', 1, 1, '{}'),
            ('CH0000000002', 'CH0000000002', 'Example Bond Index',   'CHF', 'bond',   1, 1, '{}');
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at, account_external_id, type, kind, amount_chf, currency, payload, instrument_external_id) VALUES
            ('tx1', 1000, 900, 'P3A1', 'TRADE_BUY',  'buy',  -100.00, 'CHF', '{"description":"Example Equity Index"}', 'CH0000000001'),
            ('tx2', 1000, 910, 'P3A1', 'TRADE_SELL', 'sell',   50.00, 'CHF', '{"description":"Example Bond Index"}',   'CH0000000002'),
            ('tx3', 1000, 920, 'P3A1', 'TRADE_BUY',  'buy',   -25.00, 'CHF', '{"description":"Something Unresolved"}', NULL);
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	byID := map[string]canonical.TransactionChange{}
	for _, x := range batch.Transactions {
		byID[x.TransactionExternalID] = x
	}
	for _, tc := range []struct{ id, instrument string }{
		{"tx1", "CH0000000001"},
		{"tx2", "CH0000000002"},
	} {
		got := byID[tc.id]
		if got.InstrumentExternalID == nil || *got.InstrumentExternalID != tc.instrument {
			t.Errorf("%s instrument = %v, want %s", tc.id, got.InstrumentExternalID, tc.instrument)
			continue
		}
		// A resolved row adds nothing of its own: the instrument row
		// carries the pair, and a copy here would only go stale
		// against it.
		if got.AssetClass != "" || got.Vehicle != "" {
			t.Errorf("%s restated the instrument's pair as (%q, %q), want both empty",
				tc.id, got.AssetClass, got.Vehicle)
		}
	}
	// An unresolved row states no instrument rather than guessing one,
	// and offers the name it failed on so a config link can close it.
	u := byID["tx3"]
	if u.InstrumentExternalID != nil || u.AssetClass != "" || u.Vehicle != "" {
		t.Errorf("an unresolved trade carried (%v, %q, %q), want all empty",
			u.InstrumentExternalID, u.AssetClass, u.Vehicle)
	}
	if u.InstrumentHint != "Something Unresolved" {
		t.Errorf("an unresolved trade hinted %q, want the name it failed on", u.InstrumentHint)
	}
}

// A fee, an interest credit or a contribution carries no description;
// its type is then the narrative. Every row carries the type as its
// provider category, so a row whose narrative is nothing more reads as
// VIAC's own filing.
func TestTransactionsCarryANarrative(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 2, '/x/1');
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at, account_external_id, type, kind, amount_chf, currency, payload) VALUES
            ('tx1', 1000, 900, 'P3A1', 'FEE_CHARGE', 'fee', -4.00, 'CHF', '{}'),
            ('tx2', 1000, 950, 'P3A1', 'DIVIDEND', 'dividend', 12.00, 'CHF', '{"description":"Example Equity Index"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	for _, tc := range []struct{ id, desc, category string }{
		{"tx1", "Fee charge", "FEE_CHARGE"},
		{"tx2", "Example Equity Index", "DIVIDEND"},
	} {
		var got *canonical.TransactionChange
		for i := range batch.Transactions {
			if batch.Transactions[i].TransactionExternalID == tc.id {
				got = &batch.Transactions[i]
			}
		}
		if got == nil {
			t.Fatalf("missing %s", tc.id)
		}
		if got.Description == nil || *got.Description != tc.desc {
			t.Errorf("%s description = %v, want %q", tc.id, got.Description, tc.desc)
		}
		if got.ProviderCategory == nil || *got.ProviderCategory != tc.category {
			t.Errorf("%s provider category = %v, want %q", tc.id, got.ProviderCategory, tc.category)
		}
	}
}
