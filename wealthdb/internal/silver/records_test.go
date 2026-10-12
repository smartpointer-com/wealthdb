package silver

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func openScratch(t *testing.T, ddl string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSnapshotTimesMergesExtraInsideTheWindow pins the batch times: the
// query's times and every extra time inside the window, once each, ascending.
func TestSnapshotTimesMergesExtraInsideTheWindow(t *testing.T) {
	db := openScratch(t, `CREATE TABLE p (t INTEGER);
INSERT INTO p VALUES (30), (10), (30), (50);`)
	w := canonical.Window{Start: 10, End: 40, HasChanges: true}
	got, err := SnapshotTimes(context.Background(), db, w, []int64{5, 20, 30, 45},
		`SELECT t FROM p WHERE t BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{10, 20, 30}; !slices.Equal(got, want) {
		t.Errorf("SnapshotTimes = %v, want %v", got, want)
	}
}

// TestAccountCurrenciesReadTheLatestRow pins the roster currency: the
// account's latest row, and the fallback where that row names none or the
// account has no row.
func TestAccountCurrenciesReadTheLatestRow(t *testing.T) {
	db := openScratch(t, `CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, currency TEXT);
INSERT INTO accounts VALUES (1, 'a', 'EUR'), (2, 'a', 'CHF'),
                            (1, 'b', 'GBP'), (2, 'b', NULL);`)
	ccy, err := ReadAccountCurrencies(context.Background(), db, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"a": "CHF", "b": "USD", "c": "USD"} {
		if got := ccy.Of(id); got != want {
			t.Errorf("Of(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestContentStatusReadsTheExtrema pins the content Status: each value the
// query yields lands in its field, and a NULL leaves the field unknown.
func TestContentStatusReadsTheExtrema(t *testing.T) {
	db := openScratch(t, `CREATE TABLE x (a INTEGER);`)
	s, err := ContentStatus(context.Background(), db, "test", `SELECT 1, 2, NULL, 4, 5`)
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.UnknownStatus()
	want.OldestSnapshotAt, want.LatestSnapshotAt = 1, 2
	want.LatestTransactionAt, want.LatestChangeNumber = 4, 5
	if s != want {
		t.Errorf("ContentStatus = %+v, want %+v", s, want)
	}
}
