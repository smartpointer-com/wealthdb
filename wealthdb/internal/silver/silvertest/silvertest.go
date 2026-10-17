// Package silvertest holds the fixture helpers the silver adapter tests
// share: a silver SQLite built from an adapter's embedded test schema, a
// connection on it, and the window and batches a test reads back. Only
// _test.go files import it.
package silvertest

import (
	"context"
	"database/sql"
	"path/filepath"
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

// Open opens a on the single silver at path. The connection closes when the
// test ends.
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

// Day is the unix time of UTC midnight on the given date.
func Day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
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
