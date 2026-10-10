package main

import (
	"context"
	"database/sql"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// report is one runnable view of a report family: its full column
// registry, its default column set, and how to fetch its rows. It is
// the seam the two front-ends share. The CLI picks columns with -C
// and writes the format it was asked for. The MCP server reads every
// column to filter and sort, then renders the page it returns. Both
// take their cells from rowsToTable, so they print the same strings
// for the same rows by construction.
type report struct {
	columns  []reportColumn
	defaults []string
	// pick resolves a -C / --columns expression to indices into
	// columns, with resolveColumns' grammar and error messages.
	pick func(expr string) ([]int, error)
	// fetch runs the report against an open gold database.
	fetch func(ctx context.Context, db *sql.DB) (reportRows, error)
}

// reportColumn is a registry column with its row type erased: what a
// front-end needs to choose, filter, sort and label it.
type reportColumn struct {
	name    string
	header  string
	align   output.Alignment
	privacy PrivacyClass
}

// reportRows are a fetched report's rows, held in their typed form so
// that any slice of rows and columns renders through rowsToTable.
type reportRows struct {
	// table renders the rows at idx (every row, in order, when idx
	// is nil) through the columns at cols, in the given privacy mode
	// and format.
	table func(idx, cols []int, privacy bool, format output.Format) output.Table
}

// request is what a report runner needs: the view, the window or
// as-of instant, the output currency, and the options of the families
// that have them. A family reads the fields it uses and ignores the
// rest. Values are already validated; period, level and the returns
// options use the CLI's vocabulary.
type request struct {
	view     string
	currency string
	// from and to bound a window, inclusive, in epoch seconds; from 0
	// is an open start. asOf is the holdings' as-of instant.
	from, to int64
	asOf     int64

	period    string // spending, income, cashflow, returns
	level     string // spending, income, cashflow
	investing string // cashflow

	method, annualize, inception string // returns
	netting                      bool   // returns

	withCash     bool              // holdings positions
	newestFirst  bool              // transactions, gains realized
	allDocuments bool              // gains realized: every copy of a sale, not the primary set
	missing      lots.MissingBasis // gains, holdings positions: how a missing cost basis counts
}

// newReport wraps a typed registry and fetch function as a report.
func newReport[T any](registry []columnSpec[T], defaults []string, fetch func(context.Context, *sql.DB) ([]T, error)) *report {
	cols := make([]reportColumn, len(registry))
	index := make(map[string]int, len(registry))
	for i, c := range registry {
		cols[i] = reportColumn{name: c.Name, header: c.header(), align: c.Align, privacy: c.Privacy}
		index[c.Name] = i
	}
	return &report{
		columns:  cols,
		defaults: defaults,
		pick: func(expr string) ([]int, error) {
			picked, err := resolveColumns(expr, defaults, registry)
			if err != nil {
				return nil, err
			}
			out := make([]int, len(picked))
			for i, c := range picked {
				out[i] = index[c.Name]
			}
			return out, nil
		},
		fetch: func(ctx context.Context, db *sql.DB) (reportRows, error) {
			rows, err := fetch(ctx, db)
			if err != nil {
				return reportRows{}, err
			}
			return reportRows{table: func(idx, colIdx []int, privacy bool, format output.Format) output.Table {
				sel := rows
				if idx != nil {
					sel = make([]T, len(idx))
					for i, j := range idx {
						sel[i] = rows[j]
					}
				}
				specs := make([]columnSpec[T], len(colIdx))
				for i, c := range colIdx {
					specs[i] = registry[c]
				}
				return rowsToTable(sel, specs, privacy, format)
			}}, nil
		},
	}
}

// allColumns is every index of a report's registry, in order.
func (r *report) allColumns() []int {
	out := make([]int, len(r.columns))
	for i := range out {
		out[i] = i
	}
	return out
}

// writeReport is the CLI's tail for a report: resolve -C before the
// database is touched, so a typo fails fast, then fetch and write.
// prefix names the command in a column error ("spending: unknown
// column …"). open is the caller's gold open, deferred until the
// columns are known to be good.
func writeReport(ctx context.Context, rep *report, colsExpr, prefix string, open func() (*sql.DB, error), privacy bool, format output.Format, w io.Writer) error {
	cols, err := rep.pick(colsExpr)
	if err != nil {
		return errs.Newf(2, "%s: %s", prefix, err.Error())
	}
	db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := rep.fetch(ctx, db)
	if err != nil {
		return err
	}
	return writeFormatted(w, format, rows.table(nil, cols, privacy, format))
}
