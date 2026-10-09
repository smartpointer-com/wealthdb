package carta

import (
	"context"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// lotFixture is one company holding two share certificates, one born from
// an option exercise, and an unexercised option grant. Every value is
// invented. exercise adds silver migration 0004's exercise columns.
func lotFixture(t *testing.T, exercise bool) (map[string]canonical.PositionLotChange, canonical.PositionChange) {
	t.Helper()
	path, db := newFixtureSilver(t)
	if exercise {
		if _, err := db.Exec(`
ALTER TABLE securities ADD COLUMN exercise_type TEXT;
ALTER TABLE securities ADD COLUMN exercise_date TEXT;
ALTER TABLE securities ADD COLUMN exercise_fmv  REAL;`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, individual_id, payload)
    VALUES (1700000000, 4, 'run', 'IND1', '{}');
INSERT INTO entities(snapshot_at, entity_external_id, individual_id, is_fund_investment, legal_name, payload)
    VALUES (1700000000, 100, 'IND1', 0, 'Example Co', '{}');
INSERT INTO securities(snapshot_at, entity_external_id, security_type, security_external_id,
    quantity, cost, market_value, exercise_price, position_status, currency, issue_date, payload) VALUES
    (1700000000, 100, 'share',  11, 1000, 500.10, 4000, NULL, 'held', '$', '02/01/2099',
     '{"original_acquisition_date": "01/15/2097"}'),
    (1700000000, 100, 'share',  12,  200, 100.20,  800, NULL, 'held', '$', '03/01/2099', '{}'),
    (1700000000, 100, 'option', 13, 5000, NULL,      0, 0.5,  'held', '$', '01/01/2096', '{}');`); err != nil {
		t.Fatal(err)
	}
	if exercise {
		if _, err := db.Exec(`
UPDATE securities SET exercise_type = 'NSO', exercise_date = '01/10/2097', exercise_fmv = 2.5
 WHERE security_external_id = 11;`); err != nil {
			t.Fatal(err)
		}
	}
	return snapshotLots(t, path)
}

// snapshotLots loads the fixture and returns its one position and that
// position's lots by lot key.
func snapshotLots(t *testing.T, path string) (map[string]canonical.PositionLotChange, canonical.PositionChange) {
	t.Helper()
	conn := openAdapter(t, path)
	ctx := context.Background()
	w, err := conn.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var pos []canonical.PositionChange
	lots := map[string]canonical.PositionLotChange{}
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pos = append(pos, b.Positions...)
		for _, l := range b.PositionLots {
			if _, dup := lots[l.LotKey]; dup {
				t.Errorf("lot %s emitted twice", l.LotKey)
			}
			lots[l.LotKey] = l
		}
		if !more {
			break
		}
	}
	if len(pos) != 1 {
		t.Fatalf("positions = %d, want the one company", len(pos))
	}
	return lots, pos[0]
}

// Each held share certificate is one open lot of its company's position:
// its shares, the cash paid as a stated book value, its value and the date
// the holder acquired it. The option grant is no lot. The lots add up to
// the position exactly.
func TestEachHeldShareCertificateIsALotOfItsPosition(t *testing.T) {
	lots, p := lotFixture(t, true)
	if len(lots) != 2 {
		t.Fatalf("lots = %d (%v), want the two share certificates", len(lots), lots)
	}
	l := lots["11"]
	if l.SnapshotAt != p.SnapshotAt || l.AccountExternalID != p.AccountExternalID ||
		l.PositionKey != p.PositionKey || l.Currency != "USD" ||
		l.InstrumentExternalID == nil || *l.InstrumentExternalID != *p.InstrumentExternalID {
		t.Errorf("lot 11 keys = %+v, want the position's snapshot, account, key, instrument and USD", l)
	}
	if l.Quantity == nil || l.Quantity.String() != "1000" {
		t.Errorf("lot 11 quantity = %v, want 1000", l.Quantity)
	}
	if l.BookValue == nil || l.BookValue.String() != "500.1" || l.BasisOrigin != canonical.BasisStated {
		t.Errorf("lot 11 book value = %v (%q), want 500.1 stated", l.BookValue, l.BasisOrigin)
	}
	if l.MarketValue == nil || l.MarketValue.String() != "4000" {
		t.Errorf("lot 11 market value = %v, want 4000", l.MarketValue)
	}
	if want := time.Date(2097, 1, 15, 0, 0, 0, 0, time.UTC); l.AcquisitionDate == nil || !l.AcquisitionDate.Equal(want) {
		t.Errorf("lot 11 acquisition date = %v, want %v", l.AcquisitionDate, want)
	}
	if got := string(l.Payload); got != `{"exercise_date":"01/10/2097","exercise_fmv":2.5,"exercise_type":"NSO","value_at_exercise":"2500"}` {
		t.Errorf("lot 11 payload = %s, want the exercise facts", got)
	}
	if l := lots["12"]; l.AcquisitionDate != nil || l.Payload != nil {
		t.Errorf("lot 12 = %+v, want no acquisition date and no payload (none stated)", l)
	}

	var qty canonical.Decimal
	for _, l := range lots {
		qty = qty.Add(*l.Quantity)
	}
	if p.Quantity == nil || !p.Quantity.Equal(qty) {
		t.Errorf("position quantity = %v, want the lots' %v", p.Quantity, qty)
	}
	// The exercised certificate counts at its value at exercise (1000 ×
	// 2.5), the bought one at its cash paid.
	if p.BookValue == nil || p.BookValue.String() != "2600.2" || p.Basis != exerciseValueBasis {
		t.Errorf("position book value = %v stamped %+v, want 2600.2 stamped %+v",
			p.BookValue, p.Basis, exerciseValueBasis)
	}
	if want := time.Date(2097, 1, 15, 0, 0, 0, 0, time.UTC); p.AcquisitionDate == nil || !p.AcquisitionDate.Equal(want) {
		t.Errorf("position acquisition date = %v, want its earliest lot's %v", p.AcquisitionDate, want)
	}
}

// A silver older than migration 0004 has no exercise columns: it still
// loads, its lots carry no exercise facts, and the holding counts the
// cash paid.
func TestASilverWithoutExerciseFactsStillEmitsLots(t *testing.T) {
	lots, p := lotFixture(t, false)
	if len(lots) != 2 {
		t.Fatalf("lots = %d, want 2", len(lots))
	}
	if l := lots["11"]; l.Payload != nil || l.BookValue == nil {
		t.Errorf("lot 11 = %+v, want a book value and no payload", l)
	}
	// Without a value at exercise the holding counts its lots' cash paid.
	var book canonical.Decimal
	for _, l := range lots {
		book = book.Add(*l.BookValue)
	}
	if p.BookValue == nil || !p.BookValue.Equal(book) || p.Basis != shareBasis {
		t.Errorf("position book value = %v stamped %+v, want the lots' %v stamped %+v",
			p.BookValue, p.Basis, book, shareBasis)
	}
}
