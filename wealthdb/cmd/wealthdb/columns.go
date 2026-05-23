package main

import (
	"fmt"
	"strings"

	"github.com/ptu/wealthdb/internal/output"
)

// columnSpec describes one named output column. T is the row
// type the Extract function takes — gold.AccountRow,
// gold.PortfolioRow, renderedPosition, or renderedTx in this
// codebase.
type columnSpec[T any] struct {
	Name    string
	Header  string // empty ⇒ same as Name (use for dynamic headers like value_<CCY>)
	Align   output.Alignment
	Extract func(T) string
}

// header returns the column's display label — Header if set,
// else Name. Used so registries can carry a stable lookup name
// while rendering a -x/--currency-suffixed header.
func (c columnSpec[T]) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

// resolveColumns implements the standard -C / --columns parsing
// shared by positions / accounts / portfolios / transactions:
//
//   - empty / "default" → the per-command defaults list
//   - "all"             → every registered column
//   - "+adds,...-removes,..." → delta against the defaults
//   - otherwise         → a comma-separated absolute list
//
// Unknown names produce a helpful error listing every column the
// command knows about.
func resolveColumns[T any](flagValue string, defaults []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	flagValue = strings.TrimSpace(flagValue)
	if adds, removes, isDelta := parseColumnsDelta(flagValue); isDelta {
		return columnsByName(applyColumnsDelta(defaults, adds, removes), registry)
	}
	switch flagValue {
	case "", "default":
		return columnsByName(defaults, registry)
	case "all":
		out := make([]columnSpec[T], len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return columnsByName(names, registry)
}

func columnsByName[T any](names []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	index := make(map[string]columnSpec[T], len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]columnSpec[T], 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinColumnNames[T any](registry []columnSpec[T]) string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// rowsToTable assembles an output.Table from a typed row slice
// and a column set. All four readout subcommands funnel their
// data through this on the way to the formatter.
func rowsToTable[T any](rows []T, cols []columnSpec[T]) output.Table {
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Align
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.Extract(r)
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}
