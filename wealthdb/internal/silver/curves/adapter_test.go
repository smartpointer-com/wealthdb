package curves

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

func TestKindIsCurves(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "curves" {
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

func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?, 1, '/x/1')`, run)
	silvertest.Exec(t, db, `INSERT INTO rates (as_of, series_id, value, payload) VALUES
	    (?, ?, '2.5000000001', ?), (?, ?, '2.6', ?),
	    (?, ?, '0.5', '{"currency":"CHF","tenor":"10Y","basis":"spot","compounding":"annual"}')`,
		d1, eurSeries, eurMeans, d2, eurSeries, eurMeans, d2, chfSeries)
}

func TestEmptyStatus(t *testing.T) {
	silvertest.AssertEmptyStatus(t, &Adapter{}, silverSchemaSQL)
}

func TestStatus(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	st, err := openAdapter(t, path).Status(context.Background())
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
func TestChangeWindowAndSnapshots(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != d1 || w.End != d2 || w.NewChangeNumber != run {
		t.Fatalf("window = %+v, want [%d,%d] up to change number %d", w, d1, d2, run)
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
func TestChangeWindowNoNewLoad(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	w, err := openAdapter(t, path).ChangeWindow(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges {
		t.Errorf("expected no changes when watermark == latest fetch run")
	}
}

func TestTransactionsEmpty(t *testing.T) {
	path, _ := newFixtureSilver(t)
	stream, err := openAdapter(t, path).Transactions(context.Background(), canonical.Window{})
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
