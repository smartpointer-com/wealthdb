package series

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// fredSchemaSQL is the fred fixture schema. It lives in fred/testdata, where
// the collector's schema test (collectors/fred/tests/test_schema.py)
// compares it with the collector's migrations.
var fredSchemaSQL = mustRead("../fred/testdata/silver_schema.sql")

func newFredSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, fredSchemaSQL)
}

func openFred(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{kind: "fred"}, path)
}

// Two observation dates (2020-01-02/03) loaded by a fetch run at
// 1_700_000_000. Mixed quote directions, like the real series.
func seedFred(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir)
            VALUES (1700000000, 1, '/x/1');
        INSERT INTO fx_rates(snapshot_at, base_currency_iso, quote_currency_iso, mid, payload) VALUES
            (1577923200, 'CHF', 'USD', '0.9000', '{"series_id":"DEXSZUS","value":"0.9000"}'),
            (1578009600, 'CHF', 'USD', '0.9100', '{"series_id":"DEXSZUS","value":"0.9100"}'),
            (1577923200, 'USD', 'EUR', '1.1200', '{"series_id":"DEXUSEU","value":"1.1200"}');
    `); err != nil {
		t.Fatal(err)
	}
}

func TestFredStatus(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	st, err := openFred(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != 1577923200 || st.LatestSnapshotAt != 1578009600 {
		t.Errorf("snapshot range = [%d,%d]", st.OldestSnapshotAt, st.LatestSnapshotAt)
	}
	if st.LatestChangeNumber != 1700000000 {
		t.Errorf("change number = %d, want fetch-run ts", st.LatestChangeNumber)
	}
	if st.OldestTransactionAt != -1 || st.LatestTransactionAt != -1 {
		t.Errorf("transactions should be the -1 sentinel, got [%d,%d]",
			st.OldestTransactionAt, st.LatestTransactionAt)
	}
}

func TestFredChangeWindowAndSnapshots(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	conn := openFred(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.NewChangeNumber != 1700000000 {
		t.Fatalf("window = %+v, want changes + new change number 1700000000", w)
	}

	fx := silvertest.Merged(t, conn, w).FxRates
	if len(fx) != 3 {
		t.Fatalf("fx rows = %d, want 3", len(fx))
	}
	// The CHF/USD @ 2020-01-02 row maps straight across (base CHF, quote
	// USD, mid 0.9000 = 1 USD = 0.90 CHF).
	want, _ := canonical.NewDecimalFromString("0.9000")
	var found bool
	for _, r := range fx {
		if r.BaseCurrency == "CHF" && r.SnapshotAt == 1577923200 {
			found = true
			if r.QuoteCurrency != "USD" || !r.MidRate.Equal(want) {
				t.Errorf("CHF/USD = %+v, want quote USD @ 0.9000", r)
			}
		}
	}
	if !found {
		t.Error("CHF/USD @ 2020-01-02 not emitted")
	}
}

// No new fetch since the watermark => no changes (idle reload is a no-op).
func TestFredChangeWindowNoNewLoad(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	w, err := openFred(t, path).ChangeWindow(context.Background(), 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if w.HasChanges {
		t.Errorf("expected no changes when watermark == latest fetch run")
	}
}

func TestFredTransactionsEmpty(t *testing.T) {
	path, _ := newFredSilver(t)
	stream, err := openFred(t, path).Transactions(context.Background(), canonical.Window{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	b, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Transactions) != 0 {
		t.Errorf("fred emits no transactions, got %d", len(b.Transactions))
	}
}

// The Treasury series of a v2 silver: a ten-year yield from before 1970,
// when as_of is negative, and two days of the three-month bill inside the
// FX range.
const (
	d1962 = int64(-252_374_400)  // 1962-01-02
	d2020 = int64(1_577_923_200) // 2020-01-02
)

func seedFredRates(t *testing.T, db *sql.DB) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO rates (as_of, series_id, value, payload) VALUES
	    (?, 'DGS10', '4.06', '{"currency":"USD","tenor":"10Y","basis":"constant_maturity"}'),
	    (?, 'DTB3', '1.54', '{}'),
	    (?, 'DTB3', '1.52', '{}')`, d1962, d2020, d2020+86400)
}

// The rates rows ride the stream beside the FX rates, and the status and
// the change window span both tables.
func TestFredRatesInTheWindow(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	seedFredRates(t, db)
	conn := openFred(t, path)

	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != d1962 || st.LatestSnapshotAt != 1578009600 {
		t.Errorf("snapshot range = [%d,%d], want [%d,1578009600]", st.OldestSnapshotAt, st.LatestSnapshotAt, d1962)
	}
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || !w.Full {
		t.Fatalf("window = %+v, want the whole history, [%d, 1578009600] in it", w, d1962)
	}
	if fx := silvertest.Merged(t, conn, w).FxRates; len(fx) != 3 {
		t.Errorf("fx rows = %d, want 3", len(fx))
	}
	rates := silvertest.Merged(t, conn, w).RateObservations
	if len(rates) != 3 {
		t.Fatalf("rates = %+v, want 3", rates)
	}
	r := rates[0]
	if r.SeriesID != "DGS10" || r.AsOf != d1962 || r.Value.String() != "4.06" ||
		string(r.Payload) != `{"currency":"USD","tenor":"10Y","basis":"constant_maturity"}` {
		t.Errorf("first rate = %+v", r)
	}

	// A narrower window holds only its own days.
	got := silvertest.Merged(t, conn, canonical.Window{Start: d2020, End: d2020, HasChanges: true}).RateObservations
	if len(got) != 1 || got[0].SeriesID != "DTB3" || got[0].Value.String() != "1.54" {
		t.Errorf("rates on 2020-01-02 = %+v", got)
	}
}

// The rates come a page at a time, and the keyset cursor neither skips
// nor repeats a row: the pages end inside a day.
func TestFredRatesPage(t *testing.T) {
	lowered := silver.RateBatchSize
	silver.RateBatchSize = 2
	t.Cleanup(func() { silver.RateBatchSize = lowered })
	path, db := newFredSilver(t)
	seedFred(t, db)
	for d := int64(0); d < 3; d++ {
		for _, series := range []string{"DGS1", "DGS10", "DTB3"} {
			silvertest.Exec(t, db, `INSERT INTO rates (as_of, series_id, value) VALUES (?, ?, '4')`, d2020+d*86400, series)
		}
	}
	conn := openFred(t, path)
	stream, err := conn.Snapshots(context.Background(), canonical.Window{Start: d2020, End: d2020 + 2*86400, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	seen, pages := map[string]int{}, 0
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(b.RateObservations) > 0 {
			pages++
		}
		for _, r := range b.RateObservations {
			seen[fmt.Sprintf("%d|%s", r.AsOf, r.SeriesID)]++
		}
		if !more {
			break
		}
	}
	if len(seen) != 9 || pages != 5 {
		t.Errorf("rates = %v in %d pages, want 9 in 5", seen, pages)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("rate %s read %d times", k, n)
		}
	}
}

// A rate value that is no decimal fails the load with an error that names
// the row.
func TestFredRateValueFailsTheLoad(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	silvertest.Exec(t, db, `INSERT INTO rates (as_of, series_id, value) VALUES (?, 'DGS10', '.')`, d2020)
	stream, err := openFred(t, path).Snapshots(context.Background(),
		canonical.Window{Start: d2020, End: d2020, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, more, err := stream.Next(context.Background())
		if err != nil {
			if !strings.Contains(err.Error(), "DGS10") || !strings.Contains(err.Error(), fmt.Sprint(d2020)) {
				t.Errorf("error %q does not name the row", err)
			}
			return
		}
		if !more {
			t.Fatal("the stream ended without an error")
		}
	}
}

// A silver its collector last loaded before migration 0002 has no rates
// table. It loads as before: FX alone, with the status and window theirs.
func TestFredV1SilverWithoutRates(t *testing.T) {
	path, db := newFredSilver(t)
	silvertest.Exec(t, db, `DROP TABLE rates`)
	seedFred(t, db)
	conn := openFred(t, path)
	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestSnapshotAt != 1577923200 || st.LatestSnapshotAt != 1578009600 {
		t.Errorf("snapshot range = [%d,%d]", st.OldestSnapshotAt, st.LatestSnapshotAt)
	}
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || !w.Full {
		t.Fatalf("window = %+v, want the whole history", w)
	}
	if fx := silvertest.Merged(t, conn, w).FxRates; len(fx) != 3 {
		t.Errorf("fx rows = %d, want 3", len(fx))
	}
	if rates := silvertest.Merged(t, conn, w).RateObservations; len(rates) != 0 {
		t.Errorf("rates = %+v, want none", rates)
	}
}

// TestFredCanonicalGolden holds every record the adapter yields over the
// shared seedFred fixture to fred/testdata/canonical.jsonl.
func TestFredCanonicalGolden(t *testing.T) {
	path, db := newFredSilver(t)
	seedFred(t, db)
	seedFredRates(t, db)
	silvertest.Golden(t, openFred(t, path), "../fred/testdata/canonical.jsonl")
}

// mustRead is a fixture file's text.
func mustRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The FX rates come a page at a time, in (day, base, quote) order, and
// the keyset cursor neither skips nor repeats a row: the pages end
// inside a day.
func TestFredFxPage(t *testing.T) {
	lowered := silver.FxBatchSize
	silver.FxBatchSize = 2
	t.Cleanup(func() { silver.FxBatchSize = lowered })
	path, db := newFredSilver(t)
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (1700000000, 1, '/x/1')`)
	for d := int64(0); d < 3; d++ {
		for _, pair := range [][2]string{{"USD", "EUR"}, {"CHF", "USD"}, {"CHF", "EUR"}} {
			silvertest.Exec(t, db, `INSERT INTO fx_rates VALUES (?, ?, ?, '1', '{}')`, d2020+d*86400, pair[0], pair[1])
		}
	}
	stream, err := openFred(t, path).Snapshots(context.Background(),
		canonical.Window{Start: d2020, End: d2020 + 2*86400, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var got []string
	pages := 0
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(b.FxRates) > 0 {
			pages++
		}
		for _, r := range b.FxRates {
			got = append(got, fmt.Sprintf("%d|%s|%s", r.SnapshotAt-d2020, r.BaseCurrency, r.QuoteCurrency))
		}
		if !more {
			break
		}
	}
	want := []string{"0|CHF|EUR", "0|CHF|USD", "0|USD|EUR", "86400|CHF|EUR", "86400|CHF|USD",
		"86400|USD|EUR", "172800|CHF|EUR", "172800|CHF|USD", "172800|USD|EUR"}
	if strings.Join(got, " ") != strings.Join(want, " ") || pages != 5 {
		t.Errorf("fx = %v in %d pages, want %v in 5", got, pages, want)
	}
}
