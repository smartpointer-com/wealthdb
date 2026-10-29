// Package silvertest holds the fixture helpers the silver adapter tests
// share: a silver SQLite built from an adapter's embedded test schema, a
// connection on it, and the window and batches a test reads back. Only
// _test.go files import it.
package silvertest

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// NewDB creates a silver SQLite in a fresh temporary directory with
// schemaSQL applied, and returns its path with a handle for seeding it. The
// handle closes when the test ends.
func NewDB(t testing.TB, schemaSQL string) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silver.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

// Open opens adapter a on the single silver at path. The connection
// closes when the test ends.
func Open(t testing.TB, a silver.Adapter, path string) silver.Connection {
	t.Helper()
	conn, err := a.Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Exec runs q on db and fails the test on an error.
func Exec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// FullWindow is conn's change window since change number 0, which has to
// report changes.
func FullWindow(t testing.TB, conn silver.Connection) canonical.Window {
	t.Helper()
	w, err := conn.ChangeWindow(context.Background(), 0)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if !w.HasChanges {
		t.Fatal("ChangeWindow: expected changes")
	}
	return w
}

// Snapshots drains conn's snapshot stream over w and returns every batch it
// yields, the final one included.
func Snapshots(t testing.TB, conn silver.Connection, w canonical.Window) []canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	var batches []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, b)
		if !more {
			return batches
		}
	}
}

// Transactions drains conn's transaction stream over w and returns every
// change it yields, in stream order.
func Transactions(t testing.TB, conn silver.Connection, w canonical.Window) []canonical.TransactionChange {
	t.Helper()
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	var out []canonical.TransactionChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Transactions Next: %v", err)
		}
		out = append(out, b.Transactions...)
		if !more {
			return out
		}
	}
}

// Merged drains conn's snapshot stream over w into one batch: each of
// its slices holds the batches' records of that kind, in stream order.
func Merged(t testing.TB, conn silver.Connection, w canonical.Window) canonical.SnapshotBatch {
	t.Helper()
	var all canonical.SnapshotBatch
	v := reflect.ValueOf(&all).Elem()
	for _, b := range Snapshots(t, conn, w) {
		bv := reflect.ValueOf(b)
		for i := range v.NumField() {
			v.Field(i).Set(reflect.AppendSlice(v.Field(i), bv.Field(i)))
		}
	}
	return all
}

// CurrentsByInstant maps each account of batch to its CURRENT cash
// balances, by instant, as "amount currency".
func CurrentsByInstant(batch canonical.SnapshotBatch) map[string]map[int64]string {
	out := map[string]map[int64]string{}
	for _, cb := range batch.CashBalances {
		if cb.BalanceKind != canonical.BalanceKindCurrent {
			continue
		}
		if out[cb.AccountExternalID] == nil {
			out[cb.AccountExternalID] = map[int64]string{}
		}
		out[cb.AccountExternalID][cb.SnapshotAt] = cb.Amount.String() + " " + cb.Currency
	}
	return out
}

// SeedRuns adds three runs around the run a test's seed made at load (r2),
// into a deposit silver with run_accounts: two a week apart before it and
// one a day after it that changes no roster row. The account seed made
// (listed) is listed again at r3. The quiet account is listed by every run
// but changed only at r0, so silver holds its r0 row alone. The left-out
// account is listed at r0 and r1, its balance changed at r1, and left out
// of r2 and r3.
func SeedRuns(t testing.TB, db *sql.DB, load int64, listed, quiet, left string) (r0, r1, r2, r3 int64) {
	t.Helper()
	r0, r1, r2, r3 = load-14*86400, load-7*86400, load, load+86400
	for _, r := range []int64{r0, r1, r3} {
		Exec(t, db, `INSERT INTO dump_runs VALUES (?,2,'/run')`, r)
	}
	for _, row := range []struct {
		at      int64
		acct    string
		balance float64
	}{
		{r0, quiet, 250}, {r0, left, 75}, {r1, left, 80},
	} {
		Exec(t, db, `INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
			row.at, row.acct, "Savings", "", "…0000", nil, row.balance, "{}")
	}
	for _, l := range []struct {
		at   int64
		acct string
	}{
		{r3, listed},
		{r0, quiet}, {r1, quiet}, {r2, quiet}, {r3, quiet},
		{r0, left}, {r1, left},
	} {
		Exec(t, db, `INSERT INTO run_accounts VALUES (?,?)`, l.at, l.acct)
	}
	return r0, r1, r2, r3
}

// Day is the unix time of UTC midnight on the given date.
func Day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// ISO is the unix time of UTC midnight on s, a YYYY-MM-DD date.
func ISO(t testing.TB, s string) int64 {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d.Unix()
}

// AssertEmptyStatus checks that a reports the unknown status on a silver
// with schemaSQL applied and no rows.
func AssertEmptyStatus(t testing.TB, a silver.Adapter, schemaSQL string) {
	t.Helper()
	path, _ := NewDB(t, schemaSQL)
	s, err := Open(t, a, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := canonical.UnknownStatus(); s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
}
