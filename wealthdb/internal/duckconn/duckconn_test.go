package duckconn

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/duckconn/duckconntest"
)

// A session reads an instant as its UTC day on a machine in any time
// zone, on every connection of the pool.
func TestEverySessionRunsInUTC(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for i := range 3 {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// One second before midnight UTC, as a day and beside a plain
		// timestamp.
		var day string
		var between bool
		if err := conn.QueryRowContext(ctx, `
            SELECT CAST(CAST(to_timestamp(86400 * 20000 + 86399) AS DATE) AS VARCHAR),
                   TIMESTAMP '2024-10-04 23:59:59'
                       BETWEEN to_timestamp(86400 * 20000 + 86398) AND to_timestamp(86400 * 20000 + 86400)`,
		).Scan(&day, &between); err != nil {
			t.Fatal(err)
		}
		if day != "2024-10-04" {
			t.Errorf("connection %d: the instant's day is %s, want its UTC day 2024-10-04", i, day)
		}
		if !between {
			t.Errorf("connection %d: a plain timestamp is not read as UTC beside an instant", i)
		}
	}

	duckconntest.AlsoInZone(t, "Pacific/Auckland")
}

// Open is the engine's one way to a DuckDB database: a database opened
// around it takes the machine's time zone.
func TestNoOtherCodeOpensDuckDB(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "duckconn" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, call := range []string{`sql.Open("duckdb"`, "duckdb.NewConnector("} {
			if strings.Contains(string(src), call) {
				t.Errorf("%s calls %s; open the database with duckconn.Open", path, call)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
