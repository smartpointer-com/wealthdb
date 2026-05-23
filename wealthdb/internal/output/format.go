// Package output renders tabular query results to a writer.
// Each format (table, csv, csv_plain, json) is a separate
// function over the same Table value type.
package output

import "fmt"

// Format names the chosen output format for `wealthdb positions`
// and friends.
type Format string

const (
	FormatTable    Format = "table"
	FormatCSV      Format = "csv"
	FormatCSVPlain Format = "csv_plain"
	FormatJSON     Format = "json"
)

// Parse turns a CLI -f / --format value into a Format. Returns
// an error for unknown values.
func Parse(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatCSV, FormatCSVPlain, FormatJSON:
		return Format(s), nil
	}
	return "", fmt.Errorf("unknown output format %q (want one of: table, csv, csv_plain, json)", s)
}

// Alignment controls how a single column's cells are padded
// within their width — left-aligned (default) or right-aligned.
// Right alignment is used for numeric columns; it applies to both
// the data cells and the header.
type Alignment int

const (
	AlignLeft  Alignment = 0
	AlignRight Alignment = 1
)

// Table is the in-memory representation of one rendered result.
// Columns is the header; Rows are the data rows. Aligns is
// optional — when nil or shorter than Columns, missing entries
// default to AlignLeft. Cell values are already string-formatted
// by the caller (any decimal/date/null formatting decisions
// belong in the producing layer, not here).
type Table struct {
	Columns []string
	Aligns  []Alignment
	Rows    [][]string
}

// IsEmpty reports whether the table has no data rows.
func (t Table) IsEmpty() bool { return len(t.Rows) == 0 }

// alignAt returns the alignment for column i, defaulting to
// AlignLeft when Aligns is too short or absent.
func (t Table) alignAt(i int) Alignment {
	if i < len(t.Aligns) {
		return t.Aligns[i]
	}
	return AlignLeft
}
