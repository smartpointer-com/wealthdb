package series

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// curvesSchemaSQL is the curves fixture schema. It lives in curves/testdata, where
// the collector's schema test (collectors/curves/tests/test_schema.py)
// compares it with the collector's migrations.
var curvesSchemaSQL = mustRead("../curves/testdata/silver_schema.sql")

func newCurvesSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, curvesSchemaSQL)
}

func openCurves(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{kind: "curves"}, path)
}

// The kind is registered under its name, and its adapter says so.
func TestCurvesKind(t *testing.T) {
	if _, err := silver.Get("curves"); err != nil {
		t.Fatal(err)
	}
	if got := (&Adapter{kind: "curves"}).Kind(); got != "curves" {
		t.Fatalf("Kind() = %q, want curves", got)
	}
}

// Two observation days of a euro and a franc series, loaded by a fetch
// run at 1_790_000_000. The values are invented.
const (
	run       = int64(1_790_000_000)
	d1, d2    = int64(1_767_312_000), int64(1_767_571_200) // 2026-01-02, 2026-01-05
	eurSeries = "YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y"
	chfSeries = "SNB1A.SNB.NSS.KZS.EID.J10M0"
	eurMeans  = `{"currency":"EUR","tenor":"10Y","basis":"spot","compounding":"continuous"}`
)

func seedCurves(t *testing.T, db *sql.DB) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1')`, run)
	silvertest.Exec(t, db, `INSERT INTO rates (as_of, series_id, value, payload) VALUES
	    (?, ?, '2.5000000001', ?), (?, ?, '2.6', ?),
	    (?, ?, '0.5', '{"currency":"CHF","tenor":"10Y","basis":"spot","compounding":"annual"}')`,
		d1, eurSeries, eurMeans, d2, eurSeries, eurMeans, d2, chfSeries)
}

func TestCurvesEmptyStatus(t *testing.T) {
	silvertest.AssertEmptyStatus(t, &Adapter{kind: "curves"}, curvesSchemaSQL)
}

func TestCurvesStatus(t *testing.T) {
	path, db := newCurvesSilver(t)
	seedCurves(t, db)
	st, err := openCurves(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != d1 || st.LatestSnapshotAt != d2 {
		t.Errorf("snapshot range = [%d,%d], want [%d,%d]", st.OldestSnapshotAt, st.LatestSnapshotAt, d1, d2)
	}
	if st.LatestChangeNumber != run || st.ObservedSnapshotsThrough != run {
		t.Errorf("change number = %d, observed %d, want the fetch run", st.LatestChangeNumber, st.ObservedSnapshotsThrough)
	}
	if st.OldestTransactionAt != -1 || st.LatestTransactionAt != -1 {
		t.Errorf("transactions should be the -1 sentinel, got [%d,%d]", st.OldestTransactionAt, st.LatestTransactionAt)
	}
}

// A new fetch run re-emits every rate, its payload as silver states it.
func TestCurvesChangeWindowAndSnapshots(t *testing.T) {
	path, db := newCurvesSilver(t)
	seedCurves(t, db)
	conn := openCurves(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || !w.Full || w.NewChangeNumber != run {
		t.Fatalf("window = %+v, want the whole history, [%d,%d] in it, up to change number %d", w, d1, d2, run)
	}
	b := silvertest.Merged(t, conn, w)
	if len(b.FxRates) != 0 || len(b.Positions) != 0 {
		t.Errorf("curves states rates only, got %d FX rates and %d positions", len(b.FxRates), len(b.Positions))
	}
	if len(b.RateObservations) != 3 {
		t.Fatalf("rates = %+v, want 3", b.RateObservations)
	}
	r := b.RateObservations[0]
	if r.SeriesID != eurSeries || r.AsOf != d1 || r.Value.String() != "2.5000000001" || string(r.Payload) != eurMeans {
		t.Errorf("first rate = %+v", r)
	}

	// A narrower window holds only its own day.
	got := silvertest.Merged(t, conn, canonical.Window{Start: d2, End: d2, HasChanges: true}).RateObservations
	if len(got) != 2 || got[0].SeriesID != chfSeries || got[1].SeriesID != eurSeries {
		t.Errorf("rates on 2026-01-05 = %+v", got)
	}
}

// No new fetch since the watermark => no changes (an idle reload is a
// no-op).
func TestCurvesChangeWindowNoNewLoad(t *testing.T) {
	path, db := newCurvesSilver(t)
	seedCurves(t, db)
	w, err := openCurves(t, path).ChangeWindow(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges {
		t.Errorf("expected no changes when watermark == latest fetch run")
	}
}

func TestCurvesTransactionsEmpty(t *testing.T) {
	path, _ := newCurvesSilver(t)
	stream, err := openCurves(t, path).Transactions(context.Background(), canonical.Window{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	b, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Transactions) != 0 {
		t.Errorf("curves emits no transactions, got %d", len(b.Transactions))
	}
}

// TestCurvesCanonicalGolden holds every record the adapter yields over the
// shared seedCurves fixture to curves/testdata/canonical.jsonl.
func TestCurvesCanonicalGolden(t *testing.T) {
	path, db := newCurvesSilver(t)
	seedCurves(t, db)
	silvertest.Golden(t, openCurves(t, path), "../curves/testdata/canonical.jsonl")
}
