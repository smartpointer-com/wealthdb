package plaid

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// taxLots sets a holding's tax_lots, a JSON array as Plaid sends it.
func taxLots(t *testing.T, db *sql.DB, securityID string, seq int, array string) {
	t.Helper()
	exec(t, db, `UPDATE holdings SET tax_lots = ? WHERE security_id = ? AND seq = ?`,
		array, securityID, seq)
}

// lotsFixture is one brokerage run holding sec-a, with a cost, in a
// fixture the caller adds lots to.
func lotsFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-brk", "investment", "brokerage", "0")
	sec(t, db, "sec-a", "PLACEHOLDER CORP", "EXA", "equity")
	return path, db
}

// snapshotsOf drains the snapshot stream into its positions and lots.
func snapshotsOf(t *testing.T, path string) ([]canonical.PositionChange, []canonical.PositionLotChange) {
	t.Helper()
	conn := openConn(t, path)
	stream, err := conn.Snapshots(context.Background(), window(t, conn))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var (
		positions []canonical.PositionChange
		lots      []canonical.PositionLotChange
	)
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, b.Positions...)
		lots = append(lots, b.PositionLots...)
		if !more {
			return positions, lots
		}
	}
}

func date(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

// jsonHas reports whether a JSON object states key as the string want.
func jsonHas(t *testing.T, raw json.RawMessage, key, want string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	return m[key] == want
}

// A holding's book value is Plaid's cost basis, stamped stated with
// method and fees unknown.
func TestAHoldingsCostIsStatedOfUnknownMethod(t *testing.T) {
	path, db := lotsFixture(t)
	hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "10", "1500", "1000")
	positions, _ := snapshotsOf(t, path)
	if len(positions) != 1 {
		t.Fatalf("positions = %+v, want one", positions)
	}
	assertAmount(t, "book", positions[0].BookValue, "1000")
	if positions[0].Basis != holdingBasis {
		t.Errorf("basis = %+v, want %+v", positions[0].Basis, holdingBasis)
	}
}

// Each tax lot is an open lot of its position, keyed by its order: its
// quantity (a short lot's negative), its cost (stated), its value and
// its purchase day. A lot that states no cost carries no basis origin.
// The position's acquisition date is its earliest lot's.
func TestTaxLotsAreThePositionsLots(t *testing.T) {
	path, db := lotsFixture(t)
	hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "8", "1200", "900")
	taxLots(t, db, "sec-a", 0, `[
		{"cost_basis":1000,"current_value":1500,"institution_lot_id":"LOT-1",
		 "original_purchase_datetime":"2030-04-15T00:00:00.000Z","position_type":"LONG",
		 "purchase_price":100,"quantity":10},
		{"cost_basis":-100,"current_value":-300,"institution_lot_id":"LOT-2",
		 "original_purchase_datetime":"2029-02-10T00:00:00Z","position_type":"SHORT",
		 "purchase_price":50,"quantity":-2},
		{"cost_basis":null,"current_value":null,"institution_lot_id":null,
		 "original_purchase_datetime":null,"position_type":null,
		 "purchase_price":null,"quantity":null}]`)
	positions, lots := snapshotsOf(t, path)
	if len(positions) != 1 || len(lots) != 3 {
		t.Fatalf("positions = %d, lots = %d; want 1 and 3", len(positions), len(lots))
	}
	pos := positions[0]
	if pos.AcquisitionDate == nil || !pos.AcquisitionDate.Equal(*date(2029, 2, 10)) {
		t.Errorf("acquisition date = %v, want the earliest lot's", pos.AcquisitionDate)
	}
	want := []struct {
		key, quantity, book, value string
		acquired                   *time.Time
	}{
		{"1", "10", "1000", "1500", date(2030, 4, 15)},
		{"2", "-2", "-100", "-300", date(2029, 2, 10)},
		{"3", "", "", "", nil},
	}
	for i, w := range want {
		l := lots[i]
		if l.LotKey != w.key || l.SnapshotAt != pos.SnapshotAt ||
			l.AccountExternalID != pos.AccountExternalID || l.PositionKey != pos.PositionKey ||
			l.InstrumentExternalID == nil || *l.InstrumentExternalID != "EXA" || l.Currency != "USD" {
			t.Errorf("lot %d = %+v, want key %s beside %s", i, l, w.key, pos.PositionKey)
		}
		assertAmount(t, "lot "+w.key+" quantity", l.Quantity, w.quantity)
		assertAmount(t, "lot "+w.key+" book", l.BookValue, w.book)
		assertAmount(t, "lot "+w.key+" value", l.MarketValue, w.value)
		if (l.AcquisitionDate == nil) != (w.acquired == nil) ||
			w.acquired != nil && !l.AcquisitionDate.Equal(*w.acquired) {
			t.Errorf("lot %s acquired %v, want %v", w.key, l.AcquisitionDate, w.acquired)
		}
		wantOrigin := canonical.BasisOrigin("")
		if w.book != "" {
			wantOrigin = canonical.BasisStated
		}
		if l.BasisOrigin != wantOrigin || l.Term != "" || l.Covered != nil {
			t.Errorf("lot %s origin %q term %q covered %v; want %q and no term or coverage",
				w.key, l.BasisOrigin, l.Term, l.Covered, wantOrigin)
		}
	}
	if !jsonHas(t, lots[0].Payload, "institution_lot_id", "LOT-1") {
		t.Errorf("lot 1 payload = %s, want Plaid's element", lots[0].Payload)
	}
}

// An empty tax_lots states no lots: the position has none, and no
// acquisition date.
func TestAHoldingWithoutTaxLotsHasNoLots(t *testing.T) {
	path, db := lotsFixture(t)
	hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "10", "1500", "1000")
	positions, lots := snapshotsOf(t, path)
	if len(lots) != 0 {
		t.Errorf("lots = %+v, want none", lots)
	}
	if len(positions) != 1 || positions[0].AcquisitionDate != nil {
		t.Errorf("positions = %+v, want one with no acquisition date", positions)
	}
}

// The lots of two holdings summed into one position are that
// position's lots, numbered across both in silver's order. When one of
// them states no lots, the position has none: the others would not add
// up to it.
func TestSummedHoldingsPoolTheirLots(t *testing.T) {
	for _, complete := range []bool{true, false} {
		path, db := lotsFixture(t)
		sec(t, db, "sec-b", "PLACEHOLDER CORP", "EXA", "equity")
		hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "1", "150", "100")
		hold(t, db, runAt(10), "acct-brk", "sec-b", 0, "2", "300", "200")
		taxLots(t, db, "sec-a", 0, `[{"quantity":1,"cost_basis":100}]`)
		if complete {
			taxLots(t, db, "sec-b", 0, `[{"quantity":1,"cost_basis":90},{"quantity":1,"cost_basis":110}]`)
		}
		positions, lots := snapshotsOf(t, path)
		if len(positions) != 1 {
			t.Fatalf("positions = %+v, want one", positions)
		}
		if !complete {
			if len(lots) != 0 {
				t.Errorf("lots = %+v, want none when a holding states none", lots)
			}
			continue
		}
		if len(lots) != 3 {
			t.Fatalf("lots = %+v, want three", lots)
		}
		for i, book := range []string{"100", "90", "110"} {
			if lots[i].LotKey != []string{"1", "2", "3"}[i] {
				t.Errorf("lot %d key = %q", i, lots[i].LotKey)
			}
			assertAmount(t, "lot book", lots[i].BookValue, book)
		}
	}
}

// A position that holds the vested part of a holding carries its cost
// pro rata, and no lots: Plaid's lots are the whole holding's.
func TestAProRatedPositionHasNoLots(t *testing.T) {
	path, db := lotsFixture(t)
	hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "100", "5000", "4000")
	exec(t, db, `UPDATE holdings SET institution_price = '50', vested_quantity = '40'`)
	taxLots(t, db, "sec-a", 0, `[{"quantity":60,"cost_basis":2400},{"quantity":40,"cost_basis":1600}]`)
	positions, lots := snapshotsOf(t, path)
	if len(positions) != 1 {
		t.Fatalf("positions = %+v, want one", positions)
	}
	assertAmount(t, "book", positions[0].BookValue, "1600")
	if len(lots) != 0 || positions[0].AcquisitionDate != nil {
		t.Errorf("lots = %+v, acquisition = %v; want neither", lots, positions[0].AcquisitionDate)
	}
}

func TestTaxLotsThatAreNotAnArrayFailTheLoad(t *testing.T) {
	path, db := lotsFixture(t)
	hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "10", "1500", "1000")
	taxLots(t, db, "sec-a", 0, `{"quantity":10}`)
	conn := openConn(t, path)
	if _, err := conn.Snapshots(context.Background(), window(t, conn)); err == nil {
		t.Error("Snapshots accepted a tax_lots object")
	}
}
