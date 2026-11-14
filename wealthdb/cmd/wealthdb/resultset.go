package main

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/output"

// resultSet is a fetched report ready to filter, sort and page. cells
// are every registry column as the table format prints them, privacy
// applied: the privacy endpoint filters and sorts over what it shows,
// so no filter can probe a value it hides. names are the call's
// pseudonyms, numbered over cells, so the page a call returns names a
// typed value as its filters read it, whatever the page's rows and
// columns.
type resultSet struct {
	rep     *report
	rows    reportRows
	cells   [][]string
	privacy bool
	names   *pseudonyms
}

func newResultSet(rep *report, rows reportRows, privacy bool) *resultSet {
	names := newPseudonyms()
	t := rows.table(nil, rep.allColumns(), privacy, output.FormatTable, names)
	return &resultSet{rep: rep, rows: rows, cells: t.Rows, privacy: privacy, names: names}
}
