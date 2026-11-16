package manual

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestABookThatEmptiesEndsEveryPosition: when the last positions close on
// one date, that date carries the exit-day zero snapshot, so gold ends
// them there rather than carrying their last marks forward.
func TestABookThatEmptiesEndsEveryPosition(t *testing.T) {
	path, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO load_runs(load_at, silver_schema_version, bronze_dir, payload)
            VALUES (1700000000, 1, '/tmp', '{}');
        INSERT INTO positions(id, kind, vehicle, display_name, currency, acquired_at, closed_at, notes, payload) VALUES
            ('re-1', 'real_estate', 'physical', 'Property A', 'CHF', '2020-01-01', '2024-06-30', '', '{}'),
            ('pf-1', 'private_fund', 'fund',    'Fund A',     'USD', '2021-01-01', '2024-06-30', '', '{}');
        INSERT INTO valuations(position_id, as_of_date, value, currency, notes, payload) VALUES
            ('re-1', '2020-01-01', '100', 'CHF', '', '{}'),
            ('pf-1', '2021-01-01', '200', 'USD', '', '{}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(t.Context(), -1)
	batches := silvertest.Snapshots(t, conn, w)
	if len(batches) == 0 {
		t.Fatal("no batches")
	}
	last := batches[len(batches)-1]
	exit := silvertest.ISO(t, "2024-06-30")
	if len(last.Positions) != 2 {
		t.Fatalf("last batch holds %d positions, want the 2 closed ones at zero", len(last.Positions))
	}
	for _, p := range last.Positions {
		if p.SnapshotAt != exit || p.MarketValue == nil || !p.MarketValue.IsZero() ||
			string(p.Payload) != string(silver.ClosureMarkerPayload) {
			t.Errorf("%s: snapshot %d value %v payload %s, want a zero closure marker at the exit date",
				p.PositionKey, p.SnapshotAt, p.MarketValue, p.Payload)
		}
	}
}
