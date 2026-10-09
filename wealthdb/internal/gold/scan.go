package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// scanRows runs a report query and scans each row into a T through the
// destinations dests lists for it, in column order. A destination is a
// plain pointer for a NOT NULL column, or one of the cells below for a
// nullable one, which copies the value into its field once the row is
// read.
func scanRows[T any](ctx context.Context, db *sql.DB, label, q string, args []any, dests func(*T) []any) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()

	var out []T
	for rows.Next() {
		var r T
		d := dests(&r)
		if err := rows.Scan(d...); err != nil {
			return nil, fmt.Errorf("%s scan: %w", label, err)
		}
		for _, c := range d {
			if c, ok := c.(cell); ok {
				c.apply()
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// cell is a nullable scan destination that writes its field after the
// scan.
type cell interface{ apply() }

type strCell struct {
	sql.NullString
	dst **string
}

func (c *strCell) apply() { *c.dst = nullStringToPtr(c.NullString) }

// str scans a nullable text column into a *string, nil for NULL.
func str(dst **string) *strCell { return &strCell{dst: dst} }

type decCell struct {
	sql.NullString
	dst **string
}

func (c *decCell) apply() { *c.dst = trimmedDecimalPtr(c.NullString) }

// dec scans a nullable decimal column, cast to VARCHAR by the macro,
// into a *string with its trailing zeros trimmed.
func dec(dst **string) *decCell { return &decCell{dst: dst} }

type fltCell struct {
	sql.NullFloat64
	dst **float64
}

func (c *fltCell) apply() { *c.dst = nullFloatToPtr(c.NullFloat64) }

// flt scans a nullable DOUBLE column into a *float64.
func flt(dst **float64) *fltCell { return &fltCell{dst: dst} }

type intCell struct {
	sql.NullInt64
	dst **int64
}

func (c *intCell) apply() { *c.dst = nullInt64ToPtr(c.NullInt64) }

// i64 scans a nullable BIGINT column into a *int64.
func i64(dst **int64) *intCell { return &intCell{dst: dst} }

type boolCell struct {
	sql.NullBool
	dst **bool
}

func (c *boolCell) apply() {
	if c.Valid {
		v := c.Bool
		*c.dst = &v
	}
}

// boolp scans a nullable BOOLEAN column into a *bool.
func boolp(dst **bool) *boolCell { return &boolCell{dst: dst} }

type nonNullCell struct {
	sql.NullString
	dst *string
}

func (c *nonNullCell) apply() { *c.dst = c.String }

// nonNull scans a nullable text column into a string, "" for NULL.
func nonNull(dst *string) *nonNullCell { return &nonNullCell{dst: dst} }
