package fred

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/fred.db"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func collectFx(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.FxRateChange {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	var fx []canonical.FxRateChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		fx = append(fx, b.FxRates...)
		if !more {
			break
		}
	}
	return fx
}

func TestKindIsFred(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "fred" {
		t.Fatalf("Kind() = %q, want fred", got)
	}
}

// Two observation dates (2020-01-02/03) loaded by a fetch run at
// 1_700_000_000. Mixed quote directions, like the real series.
func seed(t *testing.T, db *sql.DB) {
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

func TestStatus(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	st, err := openAdapter(t, path).Status(context.Background())
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

func TestChangeWindowAndSnapshots(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	conn := openAdapter(t, path)

	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.NewChangeNumber != 1700000000 {
		t.Fatalf("window = %+v, want changes + new change number 1700000000", w)
	}

	fx := collectFx(t, conn, w)
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
func TestChangeWindowNoNewLoad(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	w, err := openAdapter(t, path).ChangeWindow(context.Background(), 1700000000)
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
		t.Errorf("fred emits no transactions, got %d", len(b.Transactions))
	}
}
