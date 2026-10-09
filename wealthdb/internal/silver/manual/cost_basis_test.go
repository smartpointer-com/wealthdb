package manual

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A position the cost_basis series covers takes its book value from the
// latest entry on or before the date, and has none before the first; every
// entry date is a snapshot day. A position without a series keeps the
// valuation at its acquired_at.
func TestACostBasisSeriesSetsTheBookValue(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	if _, err := db.Exec(`
        INSERT INTO cost_basis(position_id, as_of_date, amount, currency, notes) VALUES
            ('pf-1', '2021-02-01', '50',  'USD', 'first call'),
            ('pf-1', '2022-08-01', '120', 'USD', 'second call');`); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	book := map[int64]map[string]*canonical.Decimal{}
	for _, b := range collectSnapshots(t, conn, w) {
		for _, p := range b.Positions {
			if book[p.SnapshotAt] == nil {
				book[p.SnapshotAt] = map[string]*canonical.Decimal{}
			}
			book[p.SnapshotAt][p.PositionKey] = p.BookValue
		}
	}
	for _, c := range []struct{ date, id, want string }{
		{"2021-01-01", "pf-1", ""}, // acquired, before the first entry
		{"2021-02-01", "pf-1", "50.00"},
		{"2022-06-01", "pf-1", "50.00"},
		{"2022-08-01", "pf-1", "120.00"},
		{"2023-01-01", "pf-1", "120.00"},
		{"2023-01-01", "re-1", "100.00"}, // no series: the acquired_at valuation
	} {
		at, ok := book[iso(t, c.date)]
		if !ok {
			t.Errorf("%s: no snapshot", c.date)
			continue
		}
		got := ""
		if bv := at[c.id]; bv != nil {
			got = bv.StringFixed(2)
		}
		if got != c.want {
			t.Errorf("%s %s book value = %q, want %q", c.date, c.id, got, c.want)
		}
	}
}

// A silver last loaded before the cost_basis table existed reads as a book
// with no series rather than failing.
func TestASilverWithoutTheCostBasisTableStillLoads(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	if _, err := db.Exec(`DROP TABLE cost_basis`); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	var pf *canonical.Decimal
	for _, b := range collectSnapshots(t, conn, w) {
		for _, p := range b.Positions {
			if p.PositionKey == "pf-1" && p.SnapshotAt == iso(t, "2023-01-01") {
				pf = p.BookValue
			}
		}
	}
	if pf == nil || pf.StringFixed(2) != "200.00" {
		t.Errorf("pf-1 book value = %v, want 200.00 (the acquired_at valuation)", pf)
	}
}

// The stamp says which of the two figures a book value is: the paid-in
// series, or the valuation at acquisition.
func TestTheBookValueStampNamesItsSource(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	if _, err := db.Exec(`
        INSERT INTO cost_basis(position_id, as_of_date, amount, currency, notes) VALUES
            ('pf-1', '2021-02-01', '50', 'USD', 'first call');`); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	want := map[string]canonical.BasisMethod{
		"pf-1": canonical.BasisMethodPaidIn,
		"re-1": canonical.BasisMethodAcquisitionValue,
	}
	for _, b := range collectSnapshots(t, conn, w) {
		for _, p := range b.Positions {
			if p.SnapshotAt != iso(t, "2023-01-01") || want[p.PositionKey] == "" {
				continue
			}
			if p.Basis.Method != want[p.PositionKey] || p.Basis.Origin != canonical.BasisStated {
				t.Errorf("%s stamp = %+v, want stated %s", p.PositionKey, p.Basis, want[p.PositionKey])
			}
		}
	}
}
