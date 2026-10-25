package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// markFixture seeds an api silver with six snapshots of invented
// positions, each priced to exercise one rule of the mark guard. A
// position's mark is its value per unit; the comments give each
// snapshot's (quantity, value), at 1000 to 6000.
//
//	SPIKE2  (10, 1000) (10, 1200) (10, 1220) (10, 1005) (10, 1010)  up two nights, back on the third
//	BLIP    (10, 1000) (10,  880) (10, 1010)                        down one night, back
//	REAL    (10, 1000) (10, 1400) (10, 1420) (10, 1450)             up 40%, stays
//	LATE3   (10, 1000) (10, 1200) (10, 1210) (10, 1220) (10, 1000)  back on the third: too late
//	SPLIT   (10, 1000) (20, 1000) (20, 1010)                        two for one
//	SHORT   (-10, -1000) (-10, -1150) (-10, -1000)                  short, up one night, back
//	OPT     (1,   100) (1,   200) (1,   100)                        an option, doubles and back
//	GONE    (10, 1000)      —     (10, 1500) (10, 1000)             ends, comes back
//	NEWEST                                    ... (10, 1000) (10, 1300)  spikes at the newest snapshot
//	MMKT    a money-market fund at 2000: cash
func markFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, seed := newFixtureSilver(t)
	silvertest.Exec(t, seed, `ALTER TABLE positions ADD COLUMN unrealized_gain_loss REAL`)
	silvertest.Exec(t, seed, `INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
	    (1000, 5, '/x/1'), (2000, 5, '/x/2'), (3000, 5, '/x/3'), (4000, 5, '/x/4'),
	    (5000, 5, '/x/5'), (6000, 5, '/x/6')`)
	type obs struct {
		key       string
		snap      int64
		qty, mv   float64
		assetType string
		openPL    float64
	}
	var rows []obs
	series := func(key, assetType string, first int64, qtyValue ...float64) {
		for i := 0; i < len(qtyValue); i += 2 {
			rows = append(rows, obs{key, first + int64(i/2)*1000, qtyValue[i], qtyValue[i+1], assetType, 0})
		}
	}
	series("SPIKE2", "COLLECTIVE_INVESTMENT", 1000, 10, 1000, 10, 1200, 10, 1220, 10, 1005, 10, 1010)
	series("BLIP", "EQUITY", 1000, 10, 1000, 10, 880, 10, 1010)
	series("REAL", "EQUITY", 1000, 10, 1000, 10, 1400, 10, 1420, 10, 1450)
	series("LATE3", "EQUITY", 1000, 10, 1000, 10, 1200, 10, 1210, 10, 1220, 10, 1000)
	series("SPLIT", "EQUITY", 1000, 10, 1000, 20, 1000, 20, 1010)
	series("SHORT", "EQUITY", 1000, -10, -1000, -10, -1150, -10, -1000)
	series("OPT", "OPTION", 1000, 1, 100, 1, 200, 1, 100)
	series("GONE", "EQUITY", 1000, 10, 1000)
	series("GONE", "EQUITY", 3000, 10, 1500, 10, 1000)
	series("NEWEST", "EQUITY", 4000, 10, 1000, 10, 1000, 10, 1300)
	series("MMKT", "CASH_EQUIVALENT", 2000, 500, 500)
	rows[1].openPL = 250 // SPIKE2 at 2000
	for _, o := range rows {
		silvertest.Exec(t, seed, `INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload,
		    unrealized_gain_loss) VALUES (?, 'ACC', ?, ?, ?)`, o.snap, o.key,
			fmt.Sprintf(`{"longQuantity":%v,"shortQuantity":%v,"marketValue":%v,
			  "instrument":{"assetType":%q,"symbol":%q,"description":"EXAMPLE %s"}}`,
				max(o.qty, 0), max(-o.qty, 0), o.mv, o.assetType, o.key, o.key),
			o.openPL)
	}
	return path, seed
}

// markedPositions reads the positions of window w, keyed "key@snapshot".
func markedPositions(t *testing.T, path string, g silver.MarkGuard, w canonical.Window) map[string]canonical.PositionChange {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path, MarkGuard: g})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	out := map[string]canonical.PositionChange{}
	for _, p := range silvertest.Merged(t, conn, w).Positions {
		out[fmt.Sprintf("%s@%d", p.PositionKey, p.SnapshotAt)] = p
	}
	return out
}

// A mark that moves more than 10% and comes back within 2% of the
// previous mark at one of the next two observations is a spike: the
// observations in between read the previous mark. A move that does not
// come back stands. A spiked row's value is its quantity at the previous
// mark; its valued_at is that mark's snapshot; its payload keeps
// Schwab's value; its book value is Schwab's value less the open P/L.
// The rows are invented.
func TestASpikeThatRevertsReadsThePreviousMark(t *testing.T) {
	path, _ := markFixture(t)
	got := markedPositions(t, path, silver.MarkGuard{}, everything)
	cases := []struct {
		row, value string
		valuedAt   int64
	}{
		{"SPIKE2@1000", "1000", 0},
		{"SPIKE2@2000", "1000", 1000}, // spiked
		{"SPIKE2@3000", "1000", 1000}, // spiked
		{"SPIKE2@4000", "1005", 0},    // back within 2% of 100
		{"SPIKE2@5000", "1010", 0},
		{"BLIP@2000", "1000", 1000}, // spiked
		{"BLIP@3000", "1010", 0},
		{"REAL@2000", "1400", 0}, // a real move is never held
		{"REAL@3000", "1420", 0},
		{"LATE3@2000", "1200", 0}, // back only on the third observation
		{"LATE3@3000", "1210", 0},
		{"LATE3@5000", "1000", 0},
		{"SPLIT@2000", "1000", 0}, // quantity and mark moved together, for good
		{"SPLIT@3000", "1010", 0},
		{"SHORT@2000", "-1000", 1000}, // spiked, short
		{"SHORT@3000", "-1000", 0},
		{"OPT@2000", "200", 0},     // options are not guarded
		{"GONE@3000", "1500", 0},   // a new run: nothing to compare against
		{"GONE@4000", "1000", 0},   // the newest observation of its run
		{"NEWEST@6000", "1300", 0}, // nothing after it yet: as stated
	}
	for _, c := range cases {
		p, ok := got[c.row]
		if !ok {
			t.Errorf("%s: no position", c.row)
			continue
		}
		if decStr(p.MarketValue) != c.value || p.ValuedAt != c.valuedAt {
			t.Errorf("%s: value %s valued_at %d, want %s and %d", c.row, decStr(p.MarketValue), p.ValuedAt,
				c.value, c.valuedAt)
		}
		var stated struct {
			Stated *string `json:"stated_market_value"`
		}
		if err := json.Unmarshal(p.Payload, &stated); err != nil {
			t.Fatal(err)
		}
		if spiked := c.valuedAt != 0; spiked != (stated.Stated != nil) {
			t.Errorf("%s: stated_market_value %v in a row spiked=%v", c.row, stated.Stated, spiked)
		}
	}
	if p := got["SPIKE2@2000"]; decStr(p.BookValue) != "950" || decStr(p.Quantity) != "10" {
		t.Errorf("spiked row: book value %s quantity %s, want 950 (1200 less 250) and 10",
			decStr(p.BookValue), decStr(p.Quantity))
	}
}

// A load whose window starts inside a spike reads the whole history, so
// its rows match a load of the whole history.
func TestAWindowReadsTheMarksAroundIt(t *testing.T) {
	path, _ := markFixture(t)
	whole := markedPositions(t, path, silver.MarkGuard{}, everything)
	for _, start := range []int64{2000, 3000, 4000} {
		w := canonical.Window{Start: start, End: 6000, HasChanges: true}
		for row, p := range markedPositions(t, path, silver.MarkGuard{}, w) {
			if want := whole[row]; decStr(p.MarketValue) != decStr(want.MarketValue) || p.ValuedAt != want.ValuedAt {
				t.Errorf("window from %d, %s: %s at %d, the whole history reads %s at %d", start, row,
					decStr(p.MarketValue), p.ValuedAt, decStr(want.MarketValue), want.ValuedAt)
			}
		}
	}
}

// The newest observation stands as stated until a later one comes. The
// next load's window reaches back two snapshots, so a snapshot that
// shows the newest observation was a spike corrects it.
func TestTheNextLoadCorrectsTheNewest(t *testing.T) {
	path, seed := markFixture(t)
	silvertest.Exec(t, seed, `INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (7000, 5, '/x/7')`)
	silvertest.Exec(t, seed, `INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload,
	    unrealized_gain_loss) VALUES (7000, 'ACC', 'NEWEST', '{"longQuantity":10,"shortQuantity":0,"marketValue":1010,
	    "instrument":{"assetType":"EQUITY","symbol":"NEWEST","description":"EXAMPLE NEWEST"}}', 0)`)
	w, err := (&apiReader{db: seed}).ChangeWindow(context.Background(), 6000)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != 5000 || w.End != 7000 {
		t.Fatalf("window %d..%d, want 5000..7000: two snapshots back from the new one", w.Start, w.End)
	}
	got := markedPositions(t, path, silver.MarkGuard{}, w)
	if p := got["NEWEST@6000"]; decStr(p.MarketValue) != "1000" || p.ValuedAt != 5000 {
		t.Errorf("NEWEST@6000: %s at %d, want 1000 from 5000", decStr(p.MarketValue), p.ValuedAt)
	}
}

// The thresholds are the source's: a spike threshold of 25% lets the
// 20% and 12% moves stand, and a revert band of 0.4% finds no mark close
// enough to the previous one.
func TestTheMarkThresholdsAreConfigured(t *testing.T) {
	path, _ := markFixture(t)
	for _, c := range []struct {
		guard silver.MarkGuard
		want  map[string]string
	}{
		{silver.MarkGuard{SpikePct: 25}, map[string]string{"SPIKE2@2000": "1200", "BLIP@2000": "880"}},
		{silver.MarkGuard{RevertPct: 0.4}, map[string]string{"SPIKE2@2000": "1200", "BLIP@2000": "880"}},
	} {
		got := markedPositions(t, path, c.guard, everything)
		for row, want := range c.want {
			if v := decStr(got[row].MarketValue); v != want {
				t.Errorf("%+v %s: %s, want %s", c.guard, row, v, want)
			}
		}
	}
}

// A row without a value or a quantity states no mark: it is not counted
// among the two observations a spike has to come back in.
func TestARowWithoutAMarkIsNotCounted(t *testing.T) {
	g := newMarkGuard(silver.MarkGuard{})
	k := markSeries{"ACC", "X"}
	dec := func(s string) *canonical.Decimal {
		d, err := canonical.NewDecimalFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}
	ten := *dec("10")
	for _, o := range []struct {
		snap  int64
		qty   canonical.Decimal
		value *canonical.Decimal
	}{
		{1000, ten, dec("1000")}, {2000, ten, dec("1500")}, {3000, ten, nil},
		{4000, canonical.Decimal{}, dec("5")}, {5000, ten, dec("1600")}, {6000, ten, dec("1005")},
	} {
		g.snapshot(o.snap)
		g.observe(k, o.snap, o.qty, o.value)
	}
	fixes := g.fixes()[k]
	for _, snap := range []int64{2000, 5000} {
		if f, ok := fixes[snap]; !ok || decStr(&f.value) != "1000" || f.from != 1000 {
			t.Errorf("%d: %+v, want 1000 from 1000", snap, f)
		}
	}
	if len(fixes) != 2 {
		t.Errorf("fixes %v, want 2000 and 5000 only", fixes)
	}
}
