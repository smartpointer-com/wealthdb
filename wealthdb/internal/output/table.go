package output

import (
	"fmt"
	"io"
	"strings"
)

// WriteTable renders the table in postgres-psql style — column
// names on top, separator line, rows below, all aligned to the
// widest cell per column.
//
// Example output:
//
//	 silver_source_id | snapshot_at | account_external_id | qty
//	------------------+-------------+---------------------+-----
//	 schwab-retail    |  1736899200 | ACC1                | 100
//	(1 row)
func WriteTable(w io.Writer, t Table) error {
	if len(t.Columns) == 0 {
		return nil
	}

	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = len(c)
	}
	for _, row := range t.Rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if l := len(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}

	// Header
	if err := writeRow(w, t.Columns, widths); err != nil {
		return err
	}

	// Separator line — `---+---+---` with widths matching the data
	sep := make([]string, len(widths))
	for i, w := range widths {
		sep[i] = strings.Repeat("-", w+2)
	}
	if _, err := fmt.Fprintln(w, strings.Join(sep, "+")); err != nil {
		return err
	}

	// Data rows
	for _, row := range t.Rows {
		if err := writeRow(w, row, widths); err != nil {
			return err
		}
	}

	// Row count footer, also psql-style
	suffix := "rows"
	if len(t.Rows) == 1 {
		suffix = "row"
	}
	if _, err := fmt.Fprintf(w, "(%d %s)\n", len(t.Rows), suffix); err != nil {
		return err
	}

	return nil
}

// writeRow emits a single padded row. Cells beyond len(widths)
// are ignored (defensive; producers should match column count).
func writeRow(w io.Writer, cells []string, widths []int) error {
	parts := make([]string, len(widths))
	for i, width := range widths {
		var cell string
		if i < len(cells) {
			cell = cells[i]
		}
		parts[i] = " " + cell + strings.Repeat(" ", width-len(cell)) + " "
	}
	_, err := fmt.Fprintln(w, strings.Join(parts, "|"))
	return err
}
