package schwab

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// An api position's book value is its market value less the open P/L
// silver promotes beside it, stamped derived. A short position's basis
// is what the short sale raised, signed negative like its quantity,
// whichever sign its market value carries. Every figure is invented.
func TestAPIPositionBookValue(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        ALTER TABLE positions ADD COLUMN average_cost         REAL;
        ALTER TABLE positions ADD COLUMN unrealized_gain_loss REAL;
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 5, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload, unrealized_gain_loss) VALUES
            (1000, 'ACC', 'LONG',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500,"instrument":{"assetType":"EQUITY","cusip":"LONG","symbol":"VTI"}}', 500),
            (1000, 'ACC', 'LOSS',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":700,"instrument":{"assetType":"EQUITY","cusip":"LOSS","symbol":"QQQ"}}', -300),
            (1000, 'ACC', 'SHORT',
             '{"longQuantity":0,"shortQuantity":10,"marketValue":-800,"instrument":{"assetType":"EQUITY","cusip":"SHORT","symbol":"SPX"}}', 200),
            (1000, 'ACC', 'SHORTPOS',
             '{"longQuantity":0,"shortQuantity":10,"marketValue":800,"instrument":{"assetType":"EQUITY","cusip":"SHORTPOS","symbol":"XMPS"}}', 200),
            (1000, 'ACC', 'BOTH',
             '{"longQuantity":5,"shortQuantity":5,"marketValue":0,"instrument":{"assetType":"EQUITY","cusip":"BOTH","symbol":"XMPB"}}', 50),
            (1000, 'ACC', 'NOPL',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":900,"instrument":{"assetType":"EQUITY","cusip":"NOPL","symbol":"XMPN"}}', NULL);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	batch := firstBatch(t, openAdapter(t, path))

	want := map[string]struct{ qty, book string }{
		"LONG":     {"10", "1000"},
		"LOSS":     {"10", "1000"},
		"SHORT":    {"-10", "-1000"},
		"SHORTPOS": {"-10", "-1000"},
		"BOTH":     {"0", "<nil>"},
		"NOPL":     {"10", "<nil>"},
	}
	if len(batch.Positions) != len(want) {
		t.Fatalf("positions = %d, want %d", len(batch.Positions), len(want))
	}
	for _, p := range batch.Positions {
		w := want[p.PositionKey]
		if got := decStr(p.Quantity); got != w.qty {
			t.Errorf("%s: quantity = %s, want %s", p.PositionKey, got, w.qty)
		}
		if got := decStr(p.BookValue); got != w.book {
			t.Errorf("%s: book value = %s, want %s", p.PositionKey, got, w.book)
		}
		if err := canonical.ValidateBookValue(p.BookValue, p.Basis); err != nil {
			t.Errorf("%s: %v", p.PositionKey, err)
		}
		if p.BookValue != nil && p.Basis != derivedBasis {
			t.Errorf("%s: stamp = %+v, want %+v", p.PositionKey, p.Basis, derivedBasis)
		}
	}
}

// A silver from before the open P/L was promoted states no basis: its
// positions load without a book value or a stamp.
func TestAPIPositionBookValueOldSilver(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 4, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC', 'LONG',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500,"longOpenProfitLoss":500,"instrument":{"assetType":"EQUITY","cusip":"LONG","symbol":"VTI"}}');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	batch := firstBatch(t, openAdapter(t, path))
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(batch.Positions))
	}
	if p := batch.Positions[0]; p.BookValue != nil || !p.Basis.IsZero() {
		t.Errorf("old silver: book value %s with stamp %+v, want neither", decStr(p.BookValue), p.Basis)
	}
}

// firstBatch returns the first snapshot batch over every timestamp.
func firstBatch(t *testing.T, conn silver.Connection) canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), everything)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	b, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return b
}

// A statement holding's book value is the cost basis it prints;
// without one, its market value less its unrealized gain; without
// either, none. Its lots ride beside it in the same batch, under the
// same snapshot, account and position key, and the earliest lot's date
// is the holding's. Every figure, date and line is invented.
func TestStatementHoldingsBasisAndOpenLots(t *testing.T) {
	f := newMergedFixture(t)
	asOf := utcDay("2023-12-31")
	if _, err := f.web.Exec(`
        INSERT INTO historical_position_snapshots
            (as_of_date, account_external_id, instrument_key, quantity, market_value, cost_basis,
             unrealized_gain_loss, source_sha256, payload) VALUES
            (?1, '5678', 'VTI',  10, 1500, 1000, 500,  'sha-stmt', '{"section":"Exchange Traded Funds"}'),
            (?1, '5678', 'QQQ',   5,  700, NULL, 200,  'sha-stmt', '{"section":"Exchange Traded Funds"}'),
            (?1, '5678', 'SPX 01/16/2026 50.00 C', -2, -300, NULL, 100, 'sha-stmt', '{"section":"Options"}'),
            (?1, '5678', 'XMPL',  1, NULL, NULL, NULL, 'sha-stmt', '{"section":"Equities"}'),
            (?1, '9999', 'VTI',   1,  150,  100,  50,  'sha-other', '{"section":"Exchange Traded Funds"}');
        INSERT INTO open_lots
            (as_of_date, account_external_id, instrument_key, lot_index, quantity, unit_cost, cost_basis,
             acquired_date, unrealized_gain_loss, term, covered, footnotes, source_sha256, payload) VALUES
            (?1, '5678', 'VTI', 0, 4, 100, 400, '2021-03-02', 200, 'LONG',  NULL, NULL, 'sha-stmt',
             '{"holding_days":1000,"raw_line":"lot line 0"}'),
            (?1, '5678', 'VTI', 1, 6, 100, 600, '2020-05-01', 300, 'SHORT', NULL, 't',  'sha-stmt',
             '{"holding_days":1300,"raw_line":"lot line 1"}'),
            (?1, '5678', 'QQQ', 0, 5, NULL, NULL, NULL, NULL, NULL, NULL, 'r', 'sha-stmt',
             '{"holding_days":null,"raw_line":"lot line r"}'),
            (?1, '5678', 'SPX 01/16/2026 50.00 C', 0, -2, 2, -400, '2023-11-01', 100, NULL, NULL, 'S', 'sha-stmt',
             '{"holding_days":60,"raw_line":"lot line S"}'),
            (?1, '9999', 'VTI', 0, 1, 100, 100, '2020-01-02', 50, 'LONG', NULL, NULL, 'sha-other',
             '{"holding_days":1400,"raw_line":"unbridged"}');
    `, asOf); err != nil {
		t.Fatalf("seed web: %v", err)
	}

	stream, err := f.open(t).Snapshots(context.Background(), everything)
	if err != nil {
		t.Fatal(err)
	}
	var batch *canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Positions) > 0 && b.Positions[0].SnapshotAt == asOf {
			batch = &b
		}
		if !more {
			break
		}
	}
	if batch == nil {
		t.Fatal("no batch at the statement's period end")
	}

	type pos struct{ book, acq string }
	wantPos := map[string]pos{
		"VTI":                    {"1000", "2020-05-01"},
		"QQQ":                    {"500", "<nil>"},
		"SPX 01/16/2026 50.00 C": {"-400", "2023-11-01"},
		"XMPL":                   {"<nil>", "<nil>"},
	}
	wantStamp := map[string]canonical.Basis{
		"VTI": statementBasis, "QQQ": derivedBasis, "SPX 01/16/2026 50.00 C": derivedBasis, "XMPL": {},
	}
	if len(batch.Positions) != len(wantPos) {
		t.Fatalf("positions = %d, want %d (the unbridged holding stays out)", len(batch.Positions), len(wantPos))
	}
	for _, p := range batch.Positions {
		w := wantPos[p.PositionKey]
		if got := decStr(p.BookValue); got != w.book {
			t.Errorf("%s: book value = %s, want %s", p.PositionKey, got, w.book)
		}
		if got := dateStr(p.AcquisitionDate); got != w.acq {
			t.Errorf("%s: acquisition date = %s, want %s", p.PositionKey, got, w.acq)
		}
		if p.Basis != wantStamp[p.PositionKey] {
			t.Errorf("%s: stamp = %+v, want %+v", p.PositionKey, p.Basis, wantStamp[p.PositionKey])
		}
		if p.AccountExternalID != "HASH1" {
			t.Errorf("%s: account = %q, want the bridged hash", p.PositionKey, p.AccountExternalID)
		}
	}

	type lot struct {
		qty, book, acq string
		term           canonical.LotTerm
		origin         canonical.BasisOrigin
	}
	wantLots := map[string]lot{
		"VTI/0":                    {"4", "400", "2021-03-02", canonical.LotTermLong, canonical.BasisStated},
		"VTI/1":                    {"6", "600", "2020-05-01", canonical.LotTermShort, canonical.BasisStated},
		"QQQ/0":                    {"5", "<nil>", "<nil>", "", ""},
		"SPX 01/16/2026 50.00 C/0": {"-2", "-400", "2023-11-01", "", canonical.BasisStated},
	}
	if len(batch.PositionLots) != len(wantLots) {
		t.Fatalf("lots = %d, want %d (the unbridged holding's lot stays out)", len(batch.PositionLots), len(wantLots))
	}
	for _, l := range batch.PositionLots {
		k := l.PositionKey + "/" + l.LotKey
		w, ok := wantLots[k]
		if !ok {
			t.Errorf("unexpected lot %s", k)
			continue
		}
		if l.SnapshotAt != asOf || l.AccountExternalID != "HASH1" {
			t.Errorf("%s: placed at (%d, %s), want beside its position", k, l.SnapshotAt, l.AccountExternalID)
		}
		if l.InstrumentExternalID == nil || *l.InstrumentExternalID != l.PositionKey {
			t.Errorf("%s: instrument = %v, want the position key", k, l.InstrumentExternalID)
		}
		if got := decStr(l.Quantity); got != w.qty {
			t.Errorf("%s: quantity = %s, want %s", k, got, w.qty)
		}
		if got := decStr(l.BookValue); got != w.book {
			t.Errorf("%s: book value = %s, want %s", k, got, w.book)
		}
		if got := dateStr(l.AcquisitionDate); got != w.acq {
			t.Errorf("%s: acquired = %s, want %s", k, got, w.acq)
		}
		if l.Term != w.term || l.BasisOrigin != w.origin {
			t.Errorf("%s: term/origin = %q/%q, want %q/%q", k, l.Term, l.BasisOrigin, w.term, w.origin)
		}
		if l.Covered != nil || l.Currency != "USD" || l.SourceDocument == nil || *l.SourceDocument != "sha-stmt" {
			t.Errorf("%s: covered %v, currency %q, source %v", k, l.Covered, l.Currency, l.SourceDocument)
		}
	}

	// The payload keeps what gold has no column for.
	for _, l := range batch.PositionLots {
		if l.PositionKey != "VTI" || l.LotKey != "1" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p["footnotes"] != "t" || p["unit_cost"] != 100.0 || p["unrealized_gain_loss"] != 300.0 ||
			p["holding_days"] != 1300.0 || p["raw_line"] != "lot line 1" {
			t.Errorf("payload = %v", p)
		}
	}
}
