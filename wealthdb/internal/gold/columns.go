package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// colInsert is one INSERT that takes its rows column by column: each
// column binds as one list, and the lists unnest side by side. A
// statement then carries a whole chunk of rows for the price of one,
// where a multi-row VALUES list pays per bound value (InsertChunked).
// A nullable value travels as text, "" for NULL, or as a float64, NaN
// for NULL, and is cast in SQL. A value every row shares is a literal.
type colInsert struct {
	names, exprs, types []string
	cols                []any
	n                   int
	sized               bool
}

// colChunkRows bounds one statement's rows, so a list stays a size the
// driver binds without strain.
const colChunkRows = 100_000

func (c *colInsert) add(name, typ, expr string, col any, n int) {
	if c.sized && n != c.n {
		panic(fmt.Sprintf("colInsert: column %s has %d rows, want %d", name, n, c.n))
	}
	c.n, c.sized = n, true
	c.names = append(c.names, name)
	c.types = append(c.types, typ)
	c.exprs = append(c.exprs, expr)
	c.cols = append(c.cols, col)
}

// text adds a text column; "" is NULL.
func (c *colInsert) text(name string, v []string) {
	c.add(name, "VARCHAR", "NULLIF(c%d, '')", v, len(v))
}

// typed adds a column of typ given as text, "" for NULL: a decimal, a
// date, a nullable boolean, JSON.
func (c *colInsert) typed(name, typ string, v []string) {
	c.add(name, "VARCHAR", "CAST(NULLIF(c%d, '') AS "+typ+")", v, len(v))
}

// floats adds a column of typ given as float64, NaN for NULL; the cast
// rounds to typ's scale. Binding floats costs a fraction of formatting
// each one as text.
func (c *colInsert) floats(name, typ string, v []float64) {
	c.add(name, "DOUBLE", "CAST(CASE WHEN isnan(c%[1]d) THEN NULL ELSE c%[1]d END AS "+typ+")", v, len(v))
}

func (c *colInsert) int64s(name string, v []int64) { c.add(name, "BIGINT", "c%d", v, len(v)) }

// constant adds a column every row holds v in.
func (c *colInsert) constant(name string, v int64) {
	c.names = append(c.names, name)
	c.types = append(c.types, "")
	c.exprs = append(c.exprs, strconv.FormatInt(v, 10))
	c.cols = append(c.cols, nil)
}

func (c *colInsert) bools(name string, v []bool) { c.add(name, "BOOLEAN", "c%d", v, len(v)) }

// exec inserts the rows into table, a chunk per statement.
func (c *colInsert) exec(ctx context.Context, tx *sql.Tx, table string) error {
	if c.n == 0 {
		return nil
	}
	sel := make([]string, len(c.cols))
	var src []string
	for i, col := range c.cols {
		if col == nil {
			sel[i] = c.exprs[i]
			continue
		}
		sel[i] = fmt.Sprintf(c.exprs[i], i)
		src = append(src, fmt.Sprintf("unnest(?::%s[]) AS c%d", c.types[i], i))
	}
	q := "INSERT INTO " + table + " (" + strings.Join(c.names, ", ") + ") SELECT " + strings.Join(sel, ", ") +
		" FROM (SELECT " + strings.Join(src, ", ") + ")"
	for off := 0; off < c.n; off += colChunkRows {
		end := min(off+colChunkRows, c.n)
		var args []any
		for _, col := range c.cols {
			switch v := col.(type) {
			case []string:
				args = append(args, v[off:end])
			case []float64:
				args = append(args, v[off:end])
			case []int64:
				args = append(args, v[off:end])
			case []bool:
				args = append(args, v[off:end])
			}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("insert %s rows %d..%d: %w", table, off, end-1, err)
		}
	}
	return nil
}
