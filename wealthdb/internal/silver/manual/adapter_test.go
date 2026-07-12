package manual

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/manual.db"
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

// iso parses a calendar date to unix seconds at UTC midnight — what the
// adapter's strftime('%s', <date>) yields for a bare ISO date.
func iso(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("iso(%q): %v", s, err)
	}
	return tm.Unix()
}

// seed builds a multi-currency book exercising every path:
//
//   - re-1  (real_estate, CHF): acquired @100, re-marked @120 — book stays at
//     the acquired-date valuation (cost), market forward-fills.
//   - cn-1  (convertible_note, CHF): acquired, then CONVERTED (closed_at) — it
//     drops out at the conversion date.
//   - pe-1  (private_equity, CHF): opened by that conversion, re-marked later.
//   - pf-1  (private_fund, USD): a fund LP interest.
//   - spv-1 (spv, USD): a single-deal SPV.
//   - esc-1 (other, USD): an escrow receivable held at par.
//
// There are no transactions — the collector records none.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO load_runs(load_at, silver_schema_version, bronze_dir, payload)
            VALUES (1700000000, 1, '/tmp', '{}');
        INSERT INTO positions(id, kind, vehicle, display_name, currency, acquired_at, closed_at, notes, payload) VALUES
            ('re-1',  'real_estate',      'physical',         'Property A',  'CHF', '2020-01-01', NULL,         '', '{"property_type":"residential"}'),
            ('cn-1',  'convertible_note', 'convertible_note', 'Note A',      'CHF', '2020-06-01', '2022-03-01', '', '{"interest_rate":0}'),
            ('pe-1',  'private_equity',   'stock',            'Stake A',     'CHF', '2022-03-01', NULL,         '', '{"converted_from_position_id":"cn-1"}'),
            ('pf-1',  'private_fund',     'fund',             'Fund A',      'USD', '2021-01-01', NULL,         '', '{"role":"limited_partner"}'),
            ('spv-1', 'spv',              'spv',              'SPV A',       'USD', '2021-06-01', NULL,         '', '{"company":"Acme"}'),
            ('esc-1', 'other',            'escrow',           'Escrow A',    'USD', '2022-06-01', NULL,         '', '{"holding_type":"escrow_receivable"}');
        INSERT INTO valuations(position_id, as_of_date, value, currency, notes, payload) VALUES
            ('re-1',  '2020-01-01', '100', 'CHF', '', '{}'),
            ('re-1',  '2022-01-01', '120', 'CHF', '', '{}'),
            ('cn-1',  '2020-06-01', '30',  'CHF', '', '{}'),
            ('pe-1',  '2022-03-01', '30',  'CHF', '', '{}'),
            ('pe-1',  '2023-01-01', '40',  'CHF', '', '{}'),
            ('pf-1',  '2021-01-01', '200', 'USD', '', '{}'),
            ('spv-1', '2021-06-01', '50',  'USD', '', '{}'),
            ('esc-1', '2022-06-01', '300', 'USD', '', '{}');
    `); err != nil {
		t.Fatal(err)
	}
}

func TestKindIsManual(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "manual" {
		t.Errorf("Kind() = %q, want manual", got)
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

func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot extrema span the earliest acquisition to the latest valuation.
	if s.OldestSnapshotAt != iso(t, "2020-01-01") || s.LatestSnapshotAt != iso(t, "2023-01-01") {
		t.Errorf("snapshot extrema = [%d,%d], want [%d,%d]",
			s.OldestSnapshotAt, s.LatestSnapshotAt, iso(t, "2020-01-01"), iso(t, "2023-01-01"))
	}
	// No transactions → the transaction range is the no-state sentinel.
	if s.OldestTransactionAt != -1 || s.LatestTransactionAt != -1 {
		t.Errorf("tx extrema = [%d,%d], want [-1,-1] (manual has no transactions)",
			s.OldestTransactionAt, s.LatestTransactionAt)
	}
	if s.LatestChangeNumber != 1700000000 {
		t.Errorf("LatestChangeNumber = %d, want 1700000000 (the load_run)", s.LatestChangeNumber)
	}

	// Window spans the snapshot stream: Start = first event, End = last valuation.
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != iso(t, "2020-01-01") || w.End != iso(t, "2023-01-01") || w.NewChangeNumber != 1700000000 {
		t.Errorf("ChangeWindow(-1) = %+v, want HasChanges Start=%d End=%d NewCN=1700000000",
			w, iso(t, "2020-01-01"), iso(t, "2023-01-01"))
	}
	// Idle reload (watermark already at the latest load) → no changes.
	w2, err := conn.ChangeWindow(context.Background(), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if w2.HasChanges {
		t.Errorf("ChangeWindow(1700000000).HasChanges = true, want false (no new load)")
	}

	// Transactions() always yields an empty stream.
	ts, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	b, more, err := ts.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if more || len(b.Transactions) != 0 {
		t.Errorf("Transactions = %d (more=%v), want 0", len(b.Transactions), more)
	}
}

func collectSnapshots(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	var batches []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, b)
		if !more {
			break
		}
	}
	return batches
}

func TestSnapshotsForwardFillPerEventDate(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, _ := conn.ChangeWindow(context.Background(), -1)
	batches := collectSnapshots(t, conn, w)

	// One batch per distinct event date: 2020-01-01, 2020-06-01, 2021-01-01,
	// 2021-06-01, 2022-01-01, 2022-03-01, 2022-06-01, 2023-01-01.
	if len(batches) != 8 {
		t.Fatalf("batches = %d, want 8 distinct event dates", len(batches))
	}

	posByT := map[int64]map[string]canonical.PositionChange{}
	accts := map[string]canonical.AccountChange{}
	for i := range batches {
		for _, p := range batches[i].Positions {
			if posByT[p.SnapshotAt] == nil {
				posByT[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			posByT[p.SnapshotAt][p.PositionKey] = p
		}
		// every batch with positions carries one instrument per position
		if n := len(batches[i].Instruments); n != len(batches[i].Positions) {
			t.Errorf("batch %d: %d instruments for %d positions", i, n, len(batches[i].Positions))
		}
		for _, a := range batches[i].Accounts {
			accts[a.AccountExternalID] = a
		}
	}

	at := func(date string) map[string]canonical.PositionChange { return posByT[iso(t, date)] }
	dec := func(p canonical.PositionChange, which string, d *canonical.Decimal) string {
		if d == nil {
			t.Fatalf("%s %s is nil", p.PositionKey, which)
			return ""
		}
		return d.StringFixed(2)
	}
	mv := func(date, id string) string {
		p := at(date)[id]
		return dec(p, "market_value", p.MarketValue)
	}
	bv := func(date, id string) string {
		p := at(date)[id]
		return dec(p, "book_value", p.BookValue)
	}

	// 2020-01-01: only re-1. market = book = cost 100.
	if got := len(at("2020-01-01")); got != 1 {
		t.Errorf("2020-01-01 positions = %d, want 1 (re-1)", got)
	}
	if mv("2020-01-01", "re-1") != "100.00" || bv("2020-01-01", "re-1") != "100.00" {
		t.Errorf("2020-01-01 re-1 mv/bv = %s/%s, want 100.00/100.00",
			mv("2020-01-01", "re-1"), bv("2020-01-01", "re-1"))
	}

	// 2021-06-01: re-1 (ff), cn-1, pf-1, spv-1.
	if got := len(at("2021-06-01")); got != 4 {
		t.Errorf("2021-06-01 positions = %d, want 4", got)
	}
	if mv("2021-06-01", "re-1") != "100.00" {
		t.Errorf("2021-06-01 re-1 mv = %s, want 100.00 (forward-filled)", mv("2021-06-01", "re-1"))
	}

	// 2022-03-01: cn-1 CONVERTS — it drops out, pe-1 appears (book = its
	// acquired-date valuation 30).
	conv := at("2022-03-01")
	if _, ok := conv["cn-1"]; ok {
		t.Error("2022-03-01 still contains the converted cn-1")
	}
	if _, ok := conv["pe-1"]; !ok {
		t.Error("2022-03-01 missing the converted-in pe-1")
	}
	if bv("2022-03-01", "pe-1") != "30.00" {
		t.Errorf("2022-03-01 pe-1 book = %s, want 30.00", bv("2022-03-01", "pe-1"))
	}

	// 2022-06-01: re-1 (re-marked market 120, book still 100), pe-1, pf-1,
	// spv-1, esc-1.
	if got := len(at("2022-06-01")); got != 5 {
		t.Errorf("2022-06-01 positions = %d, want 5", got)
	}
	if mv("2022-06-01", "re-1") != "120.00" || bv("2022-06-01", "re-1") != "100.00" {
		t.Errorf("2022-06-01 re-1 mv/bv = %s/%s, want 120.00/100.00 (re-marked; cost held)",
			mv("2022-06-01", "re-1"), bv("2022-06-01", "re-1"))
	}

	// 2023-01-01: pe-1 re-marked to 40 (book still 30).
	if mv("2023-01-01", "pe-1") != "40.00" || bv("2023-01-01", "pe-1") != "30.00" {
		t.Errorf("2023-01-01 pe-1 mv/bv = %s/%s, want 40.00/30.00",
			mv("2023-01-01", "pe-1"), bv("2023-01-01", "pe-1"))
	}

	// Acquisition date is the position's acquired_at.
	if ad := at("2023-01-01")["re-1"].AcquisitionDate; ad == nil || ad.Format("2006-01-02") != "2020-01-01" {
		t.Errorf("re-1 acquisition_date = %v, want 2020-01-01", ad)
	}

	// Exactly ONE account — the holding account of kind 'other'. No funding
	// sentinel (the collector projects no transactions).
	if len(accts) != 1 {
		t.Errorf("accounts = %d, want 1 (just the holding account)", len(accts))
	}
	a, ok := accts[accountKey]
	if !ok || a.AccountKind != canonical.AccountKindOther {
		t.Errorf("holding account = %+v (ok=%v), want kind other", a, ok)
	}
	if a.TaxWrapper == nil || *a.TaxWrapper != canonical.TaxWrapperTaxablePersonal {
		t.Errorf("account tax_wrapper = %v, want taxable_personal", a.TaxWrapper)
	}
}

// TestTaxonomyPair locks in the 2-D (asset_class, vehicle) projection for
// every manual kind, on BOTH the position and its instrument (they must agree),
// and asserts each emitted pair is admitted by the taxonomy.
func TestTaxonomyPair(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	batches := collectSnapshots(t, conn, w)

	posByKey := map[string]canonical.PositionChange{}
	instByKey := map[string]canonical.InstrumentChange{}
	for _, b := range batches {
		for _, p := range b.Positions {
			posByKey[p.PositionKey] = p
		}
		for _, in := range b.Instruments {
			instByKey[in.InstrumentExternalID] = in
		}
	}

	// kind -> expected (exposure, vehicle) pair. real_estate & mortgage share
	// the real_estate exposure; private_fund & spv share private_equity; the
	// escrow row (kind 'other', vehicle 'escrow') resolves to private_debt.
	type want struct {
		ac  canonical.AssetClass
		veh canonical.Vehicle
	}
	cases := map[string]want{
		"re-1":  {canonical.AssetClassRealEstate, canonical.VehiclePhysical},
		"cn-1":  {canonical.AssetClassPrivateDebt, canonical.VehicleConvertibleNote},
		"pe-1":  {canonical.AssetClassPrivateEquity, canonical.VehicleStock},
		"pf-1":  {canonical.AssetClassPrivateEquity, canonical.VehicleFund},
		"spv-1": {canonical.AssetClassPrivateEquity, canonical.VehicleSPV},
		"esc-1": {canonical.AssetClassPrivateDebt, canonical.VehicleEscrow},
	}
	for key, wnt := range cases {
		p, ok := posByKey[key]
		if !ok {
			t.Fatalf("%s: no position emitted", key)
		}
		if p.AssetClass != wnt.ac || p.Vehicle != wnt.veh {
			t.Errorf("%s position pair = (%q,%q), want (%q,%q)",
				key, p.AssetClass, p.Vehicle, wnt.ac, wnt.veh)
		}
		if !canonical.ValidTaxonomyPair(p.AssetClass, p.Vehicle) {
			t.Errorf("%s position pair (%q,%q) is not an admitted taxonomy pair",
				key, p.AssetClass, p.Vehicle)
		}
		in := instByKey[key]
		if in.AssetClass != p.AssetClass || in.Vehicle != p.Vehicle {
			t.Errorf("%s instrument pair = (%q,%q), disagrees with position (%q,%q)",
				key, in.AssetClass, in.Vehicle, p.AssetClass, p.Vehicle)
		}
	}
}

// TestTaxonomyOtherLoanAndFallback covers the two derivation branches the seed
// book does not: a kind 'other' held as a bilateral loan (→ private_debt, per
// the vehicle), and a legacy row whose `vehicle` column is NULL (pre-migration)
// falling back to a kind-derived default so the pair stays valid.
func TestTaxonomyOtherLoanAndFallback(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO load_runs(load_at, silver_schema_version, bronze_dir, payload)
            VALUES (1700000000, 1, '/tmp', '{}');
        INSERT INTO positions(id, kind, vehicle, display_name, currency, acquired_at, closed_at, notes, payload) VALUES
            ('loan-y', 'other',        'loan', 'Loan Y',  'USD', '2021-01-01', NULL, '', '{}'),
            ('old-pe', 'private_equity', NULL, 'Stake Y', 'USD', '2021-01-01', NULL, '', '{}');
        INSERT INTO valuations(position_id, as_of_date, value, currency, notes, payload) VALUES
            ('loan-y', '2021-01-01', '100', 'USD', '', '{}'),
            ('old-pe', '2021-01-01', '200', 'USD', '', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	batches := collectSnapshots(t, conn, w)

	posByKey := map[string]canonical.PositionChange{}
	for _, b := range batches {
		for _, p := range b.Positions {
			posByKey[p.PositionKey] = p
		}
	}

	if p := posByKey["loan-y"]; p.AssetClass != canonical.AssetClassPrivateDebt || p.Vehicle != canonical.VehicleLoan {
		t.Errorf("loan-y pair = (%q,%q), want (private_debt,loan)", p.AssetClass, p.Vehicle)
	}
	// NULL vehicle → kind-derived default (private_equity → stock).
	if p := posByKey["old-pe"]; p.AssetClass != canonical.AssetClassPrivateEquity || p.Vehicle != canonical.VehicleStock {
		t.Errorf("old-pe pair = (%q,%q), want (private_equity,stock) via fallback", p.AssetClass, p.Vehicle)
	}
	for _, key := range []string{"loan-y", "old-pe"} {
		p := posByKey[key]
		if !canonical.ValidTaxonomyPair(p.AssetClass, p.Vehicle) {
			t.Errorf("%s pair (%q,%q) not admitted", key, p.AssetClass, p.Vehicle)
		}
	}
}

// TestMortgageLiabilityNegated locks in the liability path: a `mortgage`
// position is entered as a positive balance but projects to a NEGATIVE
// market/book value, so it nets against the property it secures; the asset
// itself is untouched, and the mortgage drops out at its payoff (closed_at).
func TestMortgageLiabilityNegated(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO load_runs(load_at, silver_schema_version, bronze_dir, payload)
            VALUES (1700000000, 1, '/tmp', '{}');
        INSERT INTO positions(id, kind, vehicle, display_name, currency, acquired_at, closed_at, notes, payload) VALUES
            ('re-x',   'real_estate', 'physical', 'Property X', 'USD', '2021-01-01', NULL,         '', '{}'),
            ('loan-x', 'mortgage',    'mortgage', 'Loan X',     'USD', '2021-01-01', '2022-01-01', '', '{"secures_position_id":"re-x"}');
        INSERT INTO valuations(position_id, as_of_date, value, currency, notes, payload) VALUES
            ('re-x',   '2021-01-01', '1000', 'USD', '', '{}'),
            ('loan-x', '2021-01-01', '800',  'USD', '', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	batches := collectSnapshots(t, conn, w)

	posByT := map[int64]map[string]canonical.PositionChange{}
	for _, b := range batches {
		for _, p := range b.Positions {
			if posByT[p.SnapshotAt] == nil {
				posByT[p.SnapshotAt] = map[string]canonical.PositionChange{}
			}
			posByT[p.SnapshotAt][p.PositionKey] = p
		}
	}

	at1 := posByT[iso(t, "2021-01-01")]
	loan := at1["loan-x"]
	// 2-D pair: a mortgage is negative real_estate exposure held via the
	// mortgage vehicle (TAXONOMY.md §1.5).
	if loan.AssetClass != canonical.AssetClassRealEstate || loan.Vehicle != canonical.VehicleMortgage {
		t.Errorf("loan pair = (%q,%q), want (real_estate,mortgage)", loan.AssetClass, loan.Vehicle)
	}
	if loan.MarketValue == nil || loan.MarketValue.StringFixed(2) != "-800.00" {
		t.Errorf("loan market_value = %v, want -800.00 (liability negated)", loan.MarketValue)
	}
	if loan.BookValue == nil || loan.BookValue.StringFixed(2) != "-800.00" {
		t.Errorf("loan book_value = %v, want -800.00", loan.BookValue)
	}
	// the secured asset stays a positive value
	if re := at1["re-x"]; re.MarketValue == nil || re.MarketValue.StringFixed(2) != "1000.00" {
		t.Errorf("property market_value = %v, want 1000.00 (asset unchanged)", at1["re-x"].MarketValue)
	}
	// at payoff (closed_at) the mortgage drops out; only the property remains
	at2 := posByT[iso(t, "2022-01-01")]
	if _, ok := at2["loan-x"]; ok {
		t.Errorf("loan-x present at payoff date, want dropped out")
	}
	if _, ok := at2["re-x"]; !ok {
		t.Errorf("re-x missing at 2022-01-01")
	}
}
