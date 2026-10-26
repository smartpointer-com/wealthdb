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

// insertColumnRows bounds one statement of insertColumns, as
// colChunkRows does colInsert's. Its lists are []any, which the driver
// converts a value at a time.
const insertColumnRows = 10000

// insertColumns is the row-wise form of colInsert: it writes n rows
// into the comma-separated columns cols of table, each value from
// appendRow as InsertChunked's are, NULL as nil, and cast to its
// column's catalog type. A list travels as text, a double, a bigint, a
// boolean or a text list (transportType). Each value is the column's
// value as such, never an SQL expression.
func insertColumns(ctx context.Context, tx *sql.Tx, op, table, cols string, n int, appendRow func(i int, args []any) []any) error {
	names := strings.Split(cols, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	catalog, err := tableColumns(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("%s: columns of %s: %w", op, table, err)
	}
	typeOf := make(map[string]string, len(catalog))
	for _, c := range catalog {
		typeOf[c.name] = c.typ
	}
	carry := make([]string, len(names))
	sel := make([]string, len(names))
	for i, name := range names {
		t, ok := typeOf[name]
		if !ok {
			return fmt.Errorf("%s: %s has no column %s", op, table, name)
		}
		if carry[i] = transportType(t); carry[i] == "" {
			return fmt.Errorf("%s: column %s of type %s has no list to travel in", op, name, t)
		}
		sel[i] = "CAST(unnest(CAST(? AS " + carry[i] + "[])) AS " + t + ")"
	}
	q := `INSERT INTO ` + table + ` (` + strings.Join(names, ", ") + `) SELECT ` + strings.Join(sel, ", ")
	row := make([]any, 0, len(names))
	for off := 0; off < n; off += insertColumnRows {
		end := min(off+insertColumnRows, n)
		lists := make([][]any, len(names))
		for c := range lists {
			lists[c] = make([]any, 0, end-off)
		}
		for i := off; i < end; i++ {
			row = appendRow(i, row[:0])
			if len(row) != len(names) {
				return fmt.Errorf("%s row %d: %d values for %d columns", op, i, len(row), len(names))
			}
			for c, v := range row {
				v, err := transportValue(v, carry[c])
				if err != nil {
					return fmt.Errorf("%s row %d column %s: %w", op, i, names[c], err)
				}
				lists[c] = append(lists[c], v)
			}
		}
		args := make([]any, len(lists))
		for c := range lists {
			args[c] = lists[c]
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s rows %d..%d: %w", op, off, end-1, err)
		}
	}
	return nil
}

// transportType is the list element type a column of DuckDB type t is
// bound in. It covers the types the yield and cashflow-plan tables
// hold. A BIGINT, DOUBLE, BOOLEAN or VARCHAR[] column travels as
// itself and an INTEGER as a BIGINT. Any other scalar, such as a DATE,
// travels as text. A nested type other than a text list has none
// (empty).
func transportType(t string) string {
	switch {
	case t == "VARCHAR[]":
		return t
	case strings.HasSuffix(t, "]") || strings.HasPrefix(t, "STRUCT") || strings.HasPrefix(t, "MAP") || strings.HasPrefix(t, "UNION"):
		return ""
	case t == "BOOLEAN" || t == "DOUBLE" || t == "BIGINT":
		return t
	case t == "INTEGER":
		return "BIGINT"
	default:
		return "VARCHAR"
	}
}

// transportValue is v as an element of a carry list: a pointer is its
// value or NULL, an integer widens, and a value of another kind is an
// error rather than a conversion.
func transportValue(v any, carry string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case *float64:
		if x == nil {
			return nil, nil
		}
		v = *x
	case *string:
		if x == nil {
			return nil, nil
		}
		v = *x
	case *int64:
		if x == nil {
			return nil, nil
		}
		v = *x
	case *bool:
		if x == nil {
			return nil, nil
		}
		v = *x
	case int:
		v = int64(x)
	case int32:
		v = int64(x)
	}
	switch x := v.(type) {
	case string:
		if carry == "VARCHAR" {
			return x, nil
		}
	case float64:
		if carry == "DOUBLE" {
			return x, nil
		}
	case int64:
		switch carry {
		case "BIGINT":
			return x, nil
		case "DOUBLE":
			return float64(x), nil
		}
	case bool:
		if carry == "BOOLEAN" {
			return x, nil
		}
	case []string:
		if carry == "VARCHAR[]" {
			if x == nil {
				return nil, nil
			}
			return x, nil
		}
	}
	return nil, fmt.Errorf("a %T in a %s list", v, carry)
}

// tableColumn is one column of a table as the catalog lists it.
type tableColumn struct{ name, typ string }

// tableColumns is table's columns in the catalog's order, none for a
// table that does not exist.
func tableColumns(ctx context.Context, q querier, table string) ([]tableColumn, error) {
	var cols []tableColumn
	err := scanAll(ctx, q, `
SELECT column_name, data_type FROM duckdb_columns()
 WHERE table_name = ? AND schema_name = 'main' AND database_name = current_database()
 ORDER BY column_index`, func(rows *sql.Rows) error {
		var c tableColumn
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			return err
		}
		cols = append(cols, c)
		return nil
	}, table)
	return cols, err
}
