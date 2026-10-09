package loader_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The lot tables' load semantics, driven by an in-memory adapter: open
// lots ride the snapshot window with their positions, realized lots are
// replaced whole, and both answer to supersession, exclusion and reset.
// Every id and figure below is invented.

// lotsKind borrows a kind gold's silver_sources CHECK admits and no
// adapter this package's tests import registers.
const lotsKind = "synthetic"

// lotsState is what the fake adapter serves on the next load.
var lotsState struct {
	changeNumber int64
	window       canonical.Window
	snapshots    canonical.SnapshotBatch
	realized     []canonical.RealizedLotChange
}

type lotsAdapter struct{}

func (lotsAdapter) Kind() string { return lotsKind }
func (lotsAdapter) Open(context.Context, silver.OpenSpec) (silver.Connection, error) {
	return lotsConn{}, nil
}

type lotsConn struct{}

func (lotsConn) Close() error { return nil }
func (lotsConn) Status(context.Context) (canonical.Status, error) {
	return canonical.Status{LatestChangeNumber: lotsState.changeNumber}, nil
}
func (lotsConn) ChangeWindow(context.Context, int64) (canonical.Window, error) {
	return lotsState.window, nil
}
func (lotsConn) Snapshots(context.Context, canonical.Window) (silver.SnapshotStream, error) {
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{lotsState.snapshots}), nil
}
func (lotsConn) Transactions(context.Context, canonical.Window) (silver.TransactionStream, error) {
	return silver.NewTransactionStream(canonical.TransactionBatch{}), nil
}
func (lotsConn) RealizedLots(context.Context) ([]canonical.RealizedLotChange, error) {
	return lotsState.realized, nil
}

func init() { silver.Register(lotsAdapter{}) }

func lotsGold(t *testing.T) (*loader.Loader, *sql.DB) {
	t.Helper()
	g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return loader.New(g), g
}

func lotsCount(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func lotsLoad(t *testing.T, l *loader.Loader, spec loader.SourceSpec) *loader.LoadResult {
	t.Helper()
	spec.ID, spec.Kind = "lots-src", lotsKind
	res, err := l.Load(context.Background(), spec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res
}

func heldAt(at int64, acct string, lots ...string) ([]canonical.PositionChange, []canonical.PositionLotChange) {
	q := canonical.NewDecimalFromInt(1)
	pos := []canonical.PositionChange{{
		SnapshotAt: at, AccountExternalID: acct, PositionKey: "XYZ", Currency: "USD", Quantity: &q,
		AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
	}}
	var out []canonical.PositionLotChange
	for _, k := range lots {
		out = append(out, canonical.PositionLotChange{
			SnapshotAt: at, AccountExternalID: acct, PositionKey: "XYZ", LotKey: k,
			Currency: "USD", Quantity: &q,
		})
	}
	return pos, out
}

func sale(id, acct string, sold *time.Time) canonical.RealizedLotChange {
	return canonical.RealizedLotChange{
		RealizedLotExternalID: id, AccountExternalID: acct, DocumentKind: canonical.RealizedForm1099B,
		TaxYear: 2024, DisposalDate: sold, Currency: "USD", InstrumentHint: "SOME CORP", IsPrimary: true,
	}
}

func accountsAt(at int64, ids ...string) []canonical.AccountChange {
	var out []canonical.AccountChange
	for _, id := range ids {
		out = append(out, canonical.AccountChange{AccountExternalID: id,
			AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: at, LastSeenAt: at})
	}
	return out
}

func TestPositionLotsRideThePositionWindow(t *testing.T) {
	l, db := lotsGold(t)
	pos, lots := heldAt(1000, "A", "1", "2")
	lotsState.changeNumber = 1
	lotsState.window = canonical.Window{Start: 1000, End: 1000, NewChangeNumber: 1, HasChanges: true}
	lotsState.snapshots = canonical.SnapshotBatch{Accounts: accountsAt(1000, "A"), Positions: pos, PositionLots: lots}
	lotsState.realized = nil
	res := lotsLoad(t, l, loader.SourceSpec{})
	if res.SnapshotsLoaded != 3 {
		t.Errorf("snapshot rows = %d, want 3 (one position, two lots)", res.SnapshotsLoaded)
	}

	// The second load re-states snapshot 1000 with one lot only: the
	// window delete drops the other.
	pos, lots = heldAt(1000, "A", "1")
	lotsState.changeNumber = 2
	lotsState.window = canonical.Window{Start: 1000, End: 1000, NewChangeNumber: 2, HasChanges: true}
	lotsState.snapshots = canonical.SnapshotBatch{Accounts: accountsAt(1000, "A"), Positions: pos, PositionLots: lots}
	lotsLoad(t, l, loader.SourceSpec{})
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM position_lots`); n != 1 {
		t.Errorf("position_lots = %d, want 1", n)
	}
}

func TestRealizedLotsAreReplacedWhole(t *testing.T) {
	l, db := lotsGold(t)
	d := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	lotsState.changeNumber = 1
	lotsState.window = canonical.Window{Start: 1000, End: 1000, NewChangeNumber: 1, HasChanges: true}
	lotsState.snapshots = canonical.SnapshotBatch{Accounts: accountsAt(1000, "A")}
	lotsState.realized = []canonical.RealizedLotChange{sale("L1", "A", &d), sale("L2", "A", &d)}
	if res := lotsLoad(t, l, loader.SourceSpec{}); res.RealizedLotsLoaded != 2 {
		t.Errorf("realized = %d, want 2", res.RealizedLotsLoaded)
	}

	// A later load whose window covers none of the sale dates still
	// replaces the set: L1 is gone, and the hint now links.
	lotsState.changeNumber = 2
	lotsState.window = canonical.Window{Start: 9000, End: 9000, NewChangeNumber: 2, HasChanges: true}
	lotsState.realized = []canonical.RealizedLotChange{sale("L2", "A", &d)}
	lotsLoad(t, l, loader.SourceSpec{TransactionInstruments: map[string]string{"SOME CORP": "INSTR1"}})
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM realized_lots`); n != 1 {
		t.Errorf("realized_lots = %d, want 1", n)
	}
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM realized_lots WHERE instrument_external_id = 'INSTR1'`); n != 1 {
		t.Error("the config link did not reach the realized lot")
	}

	// A load with no changes leaves the set alone.
	lotsState.window = canonical.Window{NewChangeNumber: 3}
	lotsState.changeNumber = 3
	lotsState.realized = nil
	lotsLoad(t, l, loader.SourceSpec{})
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM realized_lots`); n != 1 {
		t.Errorf("realized_lots after a no-change load = %d, want 1", n)
	}
}

func TestLotsAnswerToSupersessionAndExclusion(t *testing.T) {
	l, db := lotsGold(t)
	handover := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	before := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	posA1, lotsA1 := heldAt(handover-86400, "A", "1")
	posA2, lotsA2 := heldAt(handover, "A", "1")
	posB, lotsB := heldAt(handover, "B", "1")
	lotsState.changeNumber = 1
	lotsState.window = canonical.Window{Start: 0, End: handover, NewChangeNumber: 1, HasChanges: true}
	lotsState.snapshots = canonical.SnapshotBatch{
		Accounts:     accountsAt(handover, "A", "B"),
		Positions:    append(append(posA1, posA2...), posB...),
		PositionLots: append(append(lotsA1, lotsA2...), lotsB...),
	}
	lotsState.realized = []canonical.RealizedLotChange{
		sale("before", "A", &before), sale("after", "A", &after), sale("undated", "A", nil),
		sale("excluded", "B", &before),
	}
	lotsLoad(t, l, loader.SourceSpec{
		Supersession: map[string]int64{"A": handover},
		Overrides:    map[string]loader.AccountOverride{"B": {Exclude: true}},
	})
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM position_lots`); n != 1 {
		t.Errorf("position_lots = %d, want 1 (A before the handover)", n)
	}
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM realized_lots
	      WHERE realized_lot_external_id IN ('before', 'undated')`); n != 2 {
		t.Errorf("kept realized lots = %d, want 2", n)
	}
	if n := lotsCount(t, db, `SELECT COUNT(*) FROM realized_lots`); n != 2 {
		t.Errorf("realized_lots = %d, want 2", n)
	}

	if err := l.Reset(context.Background(), "lots-src"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"position_lots", "realized_lots"} {
		if n := lotsCount(t, db, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("after reset %s = %d", table, n)
		}
	}
}
