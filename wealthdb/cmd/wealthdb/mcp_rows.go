package main

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

// The MCP server's row layer: what it does to a report's rows after
// the report has run. The named filters, search, sort and the column
// names here are the server's alone, because a shell has jq and a model
// has nothing to pipe into; the conditions (filter_*.go) are shared
// with the CLI. Aggregates and shares are computed before any of it, as
// the CLI computes them, so a filtered categories result still shows
// each category's share of the whole bucket.

// namedFilter is one named row filter. columns are the registry columns
// it reads: the label spelling and the id spelling of the same thing, so
// "Cash withdrawal" and "cash_withdrawal" both match; a filter reads the
// listed columns its view's registry has. keys are the columns that make
// a view carry it: a view carries it exactly when it prints one of them
// (family.carries). exact filters match whole cells.
type namedFilter struct {
	name          string
	columns, keys []string
	exact         bool
}

// namedFilters are the named filters, in the order they are applied and
// reported in.
var namedFilters = []namedFilter{
	{name: "source", columns: []string{"silver_source"}, keys: []string{"silver_source"}, exact: true},
	{name: "account", columns: []string{"account", "account_id", "account_nickname", "entity", "entity_id"},
		keys: []string{"account"}},
	{name: "symbol", columns: []string{"symbol", "instrument_id", "position_key", "name", "description"},
		keys: []string{"symbol"}},
	{name: "category", columns: []string{"category", "category_id", "category_primary", "spend_detailed", "spend_primary",
		"detailed"}, keys: []string{"category", "detailed"}},
	{name: "type", columns: []string{"type", "type_id", "income_type", "income_type_id", "income_primary",
		"income_primary_id"}, keys: []string{"type", "income_type"}},
	{name: "kind", columns: []string{"kind"}, keys: []string{"kind"}},
	{name: "merchant", columns: []string{"merchant", "merchant_signature", "counterparty"}, keys: []string{"merchant"}},
	{name: "payer", columns: []string{"payer", "payer_signature", "counterparty"}, keys: []string{"payer"}},
	{name: "section", columns: []string{"section", "section_id"}, keys: []string{"section"}},
	{name: "class", columns: []string{"class", "class_id"}, keys: []string{"class"}},
	{name: "group", columns: []string{"group", "group_id"}, keys: []string{"group"}},
	{name: "asset_class", columns: []string{"asset_class"}, keys: []string{"asset_class"}},
	{name: "tax_wrapper", columns: []string{"tax_wrapper"}, keys: []string{"tax_wrapper"}},
	{name: "term", columns: []string{"term"}, keys: []string{"term"}},
}

// namedFilterOf is the named filter of that name; the zero filter, which
// reads no column, for none.
func namedFilterOf(name string) namedFilter {
	for _, f := range namedFilters {
		if f.name == name {
			return f
		}
	}
	return namedFilter{name: name}
}

// genericMoneyWords name a view's principal column when no column of
// the view carries the word itself: "-value" on spending categories is
// net_spend, on returns it is the return.
var genericMoneyWords = map[string]bool{
	"value": true, "amount": true, "spend": true, "spending": true, "net_spend": true,
	"income": true, "net_income": true, "net": true, "net_cash_flow": true, "total": true,
	"total_value": true, "balance": true, "worth": true, "return": true, "returns": true,
	"performance": true,
}

// rowFilter is one named filter of a call: its needles (a comma list
// keeps a row matching any) and the registry columns it reads. An exact
// filter (source) matches whole cells; every other filter matches
// substrings, but for a pseudonym on the privacy endpoint
// (resultSet.needle).
type rowFilter struct {
	name    string
	needles []string
	cols    []int
	exact   bool
}

// newRowFilter resolves a filter's columns against a report.
func newRowFilter(rep *report, name, value string) rowFilter {
	nf := namedFilterOf(name)
	f := rowFilter{name: name, exact: nf.exact}
	for _, n := range strings.Split(value, ",") {
		if n = normalizeCell(n); n != "" {
			f.needles = append(f.needles, n)
		}
	}
	for _, want := range nf.columns {
		for i, c := range rep.columns {
			if c.name == want {
				f.cols = append(f.cols, i)
			}
		}
	}
	return f
}

// needle is a normalized text a filter or a search looks for: a whole
// cell, or a part of one.
type needle struct {
	text  string
	whole bool
}

// needle is how the call reads s: as a whole cell where exact says so,
// or where s is one of the call's own pseudonyms on the privacy
// endpoint, since manual-1 is not part of manual-10. Any other text,
// such as a name with covid-19 in it, matches a part of a cell.
func (rs *resultSet) needle(s string, exact bool) needle {
	return needle{text: s, whole: exact || rs.privacy && rs.names.named(s)}
}

// matches reports whether any cell of row at cols, normalized, matches
// one of needles.
func matches(row []string, cols []int, needles []needle) bool {
	for _, c := range cols {
		cell := normalizeCell(row[c])
		for _, n := range needles {
			if n.whole && cell == n.text || !n.whole && strings.Contains(cell, n.text) {
				return true
			}
		}
	}
	return false
}

// filter returns the indices of the rows every filter keeps and, when
// search is set, that carry it in some cell.
func (rs *resultSet) filter(filters []rowFilter, search string) []int {
	needles := make([][]needle, len(filters))
	for i, f := range filters {
		for _, n := range f.needles {
			needles[i] = append(needles[i], rs.needle(n, f.exact))
		}
	}
	var sought []needle
	if search = normalizeCell(search); search != "" {
		sought = []needle{rs.needle(search, false)}
	}
	every := rs.rep.allColumns()
	out := make([]int, 0, len(rs.cells))
rows:
	for i, row := range rs.cells {
		for j, f := range filters {
			if !matches(row, f.cols, needles[j]) {
				continue rows
			}
		}
		if sought != nil && !matches(row, every, sought) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// distinctValues lists the values column col takes over every row, in
// first-seen order, at most limit of them; more reports whether there
// were others.
func (rs *resultSet) distinctValues(col, limit int) (vals []string, more bool) {
	seen := map[string]bool{}
	for _, row := range rs.cells {
		v := row[col]
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if len(vals) == limit {
			return vals, true
		}
		vals = append(vals, v)
	}
	return vals, false
}

// sortKey is how one cell orders: missing cells (blank, n/a, or not a
// number in a numeric column) sort last in either direction.
type sortKey struct {
	missing bool
	num     float64
	text    string
}

// sortRows orders idx by column col, stably, so ties keep the report's
// own order. A money column orders by magnitude, whatever its sign: in
// a line view money leaving is negative, and "largest first" means the
// largest amount.
func (rs *resultSet) sortRows(idx []int, col int, desc bool) {
	c := rs.rep.columns[col]
	numeric := c.numeric()
	money := c.kind == MoneyColumn
	keys := make(map[int]sortKey, len(idx))
	for _, i := range idx {
		cell := strings.TrimSpace(rs.cells[i][col])
		k := sortKey{text: strings.ToLower(cell)}
		switch {
		case cell == "" || cell == "n/a":
			k.missing = true
		case numeric:
			f, err := strconv.ParseFloat(cell, 64)
			if err != nil {
				k.missing = true
			} else if money {
				k.num = math.Abs(f)
			} else {
				k.num = f
			}
		}
		keys[i] = k
	}
	sort.SliceStable(idx, func(x, y int) bool {
		a, b := keys[idx[x]], keys[idx[y]]
		if a.missing != b.missing {
			return b.missing
		}
		if a.missing {
			return false
		}
		if numeric {
			if desc {
				return a.num > b.num
			}
			return a.num < b.num
		}
		if desc {
			return a.text > b.text
		}
		return a.text < b.text
	})
}

// carries are the named filters each view takes, by view ("" for a
// tool of one shape): those whose key column (the filter's keys, or the
// view's keyAlias) the view prints, but its excludes. Derived once from
// the registries, so a column a view gains brings its filter.
func (fam *family) carries() map[string][]string {
	fam.derive.Do(func() {
		fam.carried = map[string][]string{}
		cfg := &config.Config{DefaultCurrency: "USD"}
		for i := range fam.views {
			v := &fam.views[i]
			printed := map[string]bool{}
			for _, c := range fam.defaultLookup(cfg, v.name, false).cols {
				printed[c.name] = true
			}
			for _, f := range namedFilters {
				keys := f.keys
				if alias := v.keyAlias[f.name]; alias != "" {
					keys = append(slices.Clone(keys), alias)
				}
				if slices.ContainsFunc(keys, func(k string) bool { return printed[k] }) && !slices.Contains(v.excludes, f.name) {
					fam.carried[v.name] = append(fam.carried[v.name], f.name)
				}
			}
		}
	})
	return fam.carried
}

// buildDefault builds req's report with no call arguments: a view's
// columns as describe lists them.
func (fam *family) buildDefault(cfg *config.Config, req request) *report {
	return fam.build(cfg, req, &toolArgs{values: map[string]any{}})
}

// defaultLookup is the column lookup of view at every option's default
// (family.defaultRequest), in the configured currency. series reads the
// view as a series, which prints its period column ahead of the view's
// own.
func (fam *family) defaultLookup(cfg *config.Config, view string, series bool) columnLookup {
	req := fam.defaultRequest(view, cfg.DefaultCurrency, time.Now(), cfg.YieldHorizon())
	if series {
		req.series = []seriesPoint{{}}
	}
	return fam.lookup(fam.buildDefault(cfg, req), req)
}

// escalate picks the view a call's filters need. A small model starts
// at the coarse default view and adds a filter — spending with a
// category, holdings with a tax wrapper, a condition on a column only a
// finer view prints — and the coarse view has no such column. Instead
// of failing, the first view at or after the requested one, coarse to
// fine, that fits runs. ok is false when none fits; the requested view
// is returned and the caller says why.
func escalate(views []string, fits func(view string) bool, view string) (string, bool) {
	start := 0
	for i, v := range views {
		if v == view {
			start = i
		}
	}
	for _, v := range views[start:] {
		if fits(v) {
			return v, true
		}
	}
	return view, false
}

// carriesAll reports whether view carries every one of filters.
func carriesAll(carries map[string][]string, view string, filters []string) bool {
	for _, f := range filters {
		if !slices.Contains(carries[view], f) {
			return false
		}
	}
	return true
}
