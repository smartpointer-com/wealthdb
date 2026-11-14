package main

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/output"

// resultSet is a fetched report ready to filter, sort and page. cells
// are every registry column as the table format prints them, privacy
// applied: a privacy run filters and sorts over what it shows, so no
// filter can probe a value it hides. names are the run's pseudonyms,
// numbered as numberedCells says, so the rows a run prints name a typed
// value as its filters read it, whatever its rows and columns.
type resultSet struct {
	rep     *report
	rows    reportRows
	cells   [][]string
	privacy bool
	names   *pseudonyms
}

func newResultSet(rep *report, rows reportRows, privacy bool) *resultSet {
	if !privacy {
		t := rows.table(nil, rep.allColumns(), false, output.FormatTable, nil)
		return &resultSet{rep: rep, rows: rows, cells: t.Rows, names: newPseudonyms()}
	}
	cells, names := numberedCells(rep, rows)
	return &resultSet{rep: rep, rows: rows, cells: cells, privacy: true, names: names}
}

// numberedCells are every registry column of rows as the table format
// prints them under privacy, with the pseudonyms that number them. The
// numbers count over every row: the default columns first, as the
// default output prints them, then every other column. So a CLI run and
// an MCP call name a typed value alike, whatever columns they show,
// whatever rows their conditions keep and whatever page they return.
func numberedCells(rep *report, rows reportRows) ([][]string, *pseudonyms) {
	names := newPseudonyms()
	if defaults, err := rep.pick("default"); err == nil {
		rows.table(nil, defaults, true, output.FormatTable, names)
	}
	return rows.table(nil, rep.allColumns(), true, output.FormatTable, names).Rows, names
}
