// Package output renders tabular query results to a writer.
// Each format (table, csv, csv_plain, json) is a separate
// function over the same Table value type.
//
// Milestone 6 ships table only; CSV/JSON land in milestone 10.
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

// Table is the in-memory representation of one rendered result.
// Columns is the header; Rows are the data rows. Cell values are
// already string-formatted by the caller (any decimal/date/null
// formatting decisions belong in the producing layer, not here).
type Table struct {
	Columns []string
	Rows    [][]string
}

// IsEmpty reports whether the table has no data rows.
func (t Table) IsEmpty() bool { return len(t.Rows) == 0 }
