package output

import (
	"encoding/csv"
	"io"
)

// WriteCSV renders the table as RFC-4180 CSV with a header row.
// Alignment is ignored (CSV consumers don't see whitespace).
func WriteCSV(w io.Writer, t Table) error {
	return writeCSVRows(w, t, true)
}

// WriteCSVPlain renders the table without a header row. For
// piping into tools that don't expect column names.
func WriteCSVPlain(w io.Writer, t Table) error {
	return writeCSVRows(w, t, false)
}

func writeCSVRows(w io.Writer, t Table, withHeader bool) error {
	cw := csv.NewWriter(w)
	if withHeader {
		if err := cw.Write(t.Columns); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
