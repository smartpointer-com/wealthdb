package gold

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Full chunks run through one prepared statement and the tail through
// its own; every row lands with its values, and a failing chunk names
// its row range.
func TestInsertChunkedFullChunksAndTail(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE chunk_probe (i BIGINT, s VARCHAR, x DOUBLE)`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, InsertChunkRows, 2*InsertChunkRows + 7} {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := InsertChunked(ctx, tx, "probe", `INSERT INTO chunk_probe (i, s, x) VALUES `, "(?, ?, ?)", n,
			func(i int, args []any) []any { return append(args, i, strings.Repeat("s", i%5), float64(i)/4) }); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		var rows, sumI, bad int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(i), 0),
       count(*) FILTER (WHERE s <> repeat('s', CAST(i % 5 AS INTEGER)) OR x <> i / 4)
  FROM chunk_probe`).Scan(&rows, &sumI, &bad); err != nil {
			t.Fatal(err)
		}
		if rows != n || sumI != n*(n-1)/2 || bad != 0 {
			t.Fatalf("n=%d: rows=%d sum=%d mismatched=%d", n, rows, sumI, bad)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	err = InsertChunked(ctx, tx, "probe", `INSERT INTO chunk_probe (i, s, x) VALUES `, "(?, ?, CAST(? AS DOUBLE))", 2*InsertChunkRows,
		func(i int, args []any) []any {
			x := any(1.0)
			if i == InsertChunkRows+3 {
				x = "not a number"
			}
			return append(args, i, "", x)
		})
	want := fmt.Sprintf("probe rows %d..%d", InsertChunkRows, 2*InsertChunkRows-1)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}
}

// insertColumns writes the rows InsertChunked writes: NULLs, a nil and
// an empty list, a date as text, across a chunk boundary.
func TestInsertColumnsMatchesInsertChunked(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for _, table := range []string{"by_values", "by_columns"} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE `+table+` (i BIGINT, s VARCHAR, x DOUBLE, b BOOLEAN NOT NULL, d DATE, tags VARCHAR[])`); err != nil {
			t.Fatal(err)
		}
	}
	const cols = "i, s, x, b, d, tags"
	n := insertColumnRows + 7
	row := func(i int, args []any) []any {
		var s, x, d any
		if i%3 != 0 {
			s = strings.Repeat("v", i%4)
		}
		if i%5 != 0 {
			x = float64(i) / 8
		}
		if i%7 != 0 {
			d = "2024-01-0" + string(rune('1'+i%9))
		}
		var tags []string
		switch i % 4 {
		case 1:
			tags = []string{}
		case 2:
			tags = []string{"a"}
		case 3:
			tags = []string{"a", "b"}
		}
		return append(args, int64(i), s, x, i%2 == 0, d, tags)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := InsertChunked(ctx, tx, "values", `INSERT INTO by_values (`+cols+`) VALUES `, placeholders(cols), n, row); err != nil {
		t.Fatal(err)
	}
	if err := insertColumns(ctx, tx, "columns", "by_columns", cols, n, row); err != nil {
		t.Fatal(err)
	}
	var a, b, only int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM by_values), (SELECT count(*) FROM by_columns),
       (SELECT count(*) FROM (SELECT * FROM by_values EXCEPT ALL SELECT * FROM by_columns))
     + (SELECT count(*) FROM (SELECT * FROM by_columns EXCEPT ALL SELECT * FROM by_values))`).Scan(&a, &b, &only); err != nil {
		t.Fatal(err)
	}
	if a != n || b != n || only != 0 {
		t.Fatalf("rows %d and %d, %d on one side only", a, b, only)
	}
	err = insertColumns(ctx, tx, "short", "by_columns", cols, 1, func(_ int, args []any) []any { return append(args, int64(1)) })
	if err == nil || !strings.Contains(err.Error(), "1 values for 6 columns") {
		t.Fatalf("a short row: %v", err)
	}
}

// insertColumns refuses a value of the wrong kind for its column and a
// column no list carries, rather than cast either.
func TestInsertColumnsRefusesWhatItCannotCarry(t *testing.T) {
	db, err := Open(":memory:", ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE carry_probe (i BIGINT, ns INTEGER[])`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	err = insertColumns(ctx, tx, "kind", "carry_probe", "i", 1, func(_ int, args []any) []any { return append(args, "1") })
	if err == nil || !strings.Contains(err.Error(), "a string in a BIGINT list") {
		t.Fatalf("a string for a BIGINT column: %v", err)
	}
	err = insertColumns(ctx, tx, "nested", "carry_probe", "i, ns", 1, func(_ int, args []any) []any { return append(args, int64(1), nil) })
	if err == nil || !strings.Contains(err.Error(), "no list to travel in") {
		t.Fatalf("an INTEGER[] column: %v", err)
	}
	err = insertColumns(ctx, tx, "missing", "carry_probe", "i, x", 1, func(_ int, args []any) []any { return append(args, int64(1), nil) })
	if err == nil || !strings.Contains(err.Error(), "carry_probe has no column x") {
		t.Fatalf("a column the table lacks: %v", err)
	}
}
