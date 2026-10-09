package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// The MCP server's row layer: what it does to a report's rows after
// the report has run. The CLI has none of this because a shell has jq;
// a model has nothing to pipe into. Aggregates and shares are computed
// before any of it, as the CLI computes them, so a filtered categories
// result still shows each category's share of the whole bucket.

// filterColumns maps each named row filter to the registry columns it
// reads: the label spelling and the id spelling of the same thing, so
// "Cash withdrawal" and "cash_withdrawal" both match. A filter reads
// the listed columns its view's registry has.
var filterColumns = map[string][]string{
	"source":      {"silver_source"},
	"account":     {"account", "account_id", "account_nickname", "entity", "entity_id"},
	"symbol":      {"symbol", "instrument_id", "position_key", "name"},
	"category":    {"category", "category_id", "category_primary", "spend_detailed", "spend_primary", "detailed"},
	"type":        {"type", "type_id", "income_type", "income_type_id", "income_primary", "income_primary_id"},
	"kind":        {"kind"},
	"merchant":    {"merchant", "merchant_signature", "counterparty"},
	"payer":       {"payer", "payer_signature", "counterparty"},
	"section":     {"section", "section_id"},
	"class":       {"class", "class_id"},
	"group":       {"group", "group_id"},
	"asset_class": {"asset_class"},
	"tax_wrapper": {"tax_wrapper"},
	"term":        {"term"},
}

// filterOrder is the order filters are applied and reported in.
var filterOrder = []string{"source", "account", "symbol", "category", "type", "kind",
	"merchant", "payer", "section", "class", "group", "asset_class", "tax_wrapper", "term"}

// genericMoneyWords name a view's principal column when no column of
// the view carries the word itself: "-value" on spending categories is
// net_spend, on returns it is the return.
var genericMoneyWords = map[string]bool{
	"value": true, "amount": true, "spend": true, "spending": true, "net_spend": true,
	"income": true, "net_income": true, "net": true, "net_cash_flow": true, "total": true,
	"total_value": true, "balance": true, "worth": true, "return": true, "returns": true,
	"performance": true,
}

// resultSet is a fetched report ready to filter, sort and page. cells
// are every registry column as the table format prints them, privacy
// applied: the privacy endpoint filters and sorts over what it shows,
// so no filter can probe a value it hides.
type resultSet struct {
	rep     *report
	rows    reportRows
	cells   [][]string
	privacy bool
}

func newResultSet(rep *report, rows reportRows, privacy bool) *resultSet {
	t := rows.table(nil, rep.allColumns(), privacy, output.FormatTable)
	return &resultSet{rep: rep, rows: rows, cells: t.Rows, privacy: privacy}
}

// rowFilter is one named filter of a call: its needles (a comma list
// keeps a row matching any) and the registry columns it reads. source
// matches whole cells; every other filter matches substrings.
type rowFilter struct {
	name    string
	needles []string
	cols    []int
	exact   bool
}

// newRowFilter resolves a filter's columns against a report.
func newRowFilter(rep *report, name, value string) rowFilter {
	f := rowFilter{name: name, exact: name == "source"}
	for _, n := range strings.Split(value, ",") {
		if n = normalizeCell(n); n != "" {
			f.needles = append(f.needles, n)
		}
	}
	for _, want := range filterColumns[name] {
		for i, c := range rep.columns {
			if c.name == want {
				f.cols = append(f.cols, i)
			}
		}
	}
	return f
}

// normalizeCell is the form filters compare in: lower case, with an
// underscore read as a space, so an id spelling matches a label.
func normalizeCell(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ToLower(s), "_", " "))
}

func (f rowFilter) matches(row []string) bool {
	for _, c := range f.cols {
		cell := normalizeCell(row[c])
		for _, n := range f.needles {
			if (f.exact && cell == n) || (!f.exact && strings.Contains(cell, n)) {
				return true
			}
		}
	}
	return false
}

// filter returns the indices of the rows every filter keeps and, when
// search is set, that carry it in some cell.
func (rs *resultSet) filter(filters []rowFilter, search string) []int {
	search = normalizeCell(search)
	out := make([]int, 0, len(rs.cells))
rows:
	for i, row := range rs.cells {
		for _, f := range filters {
			if !f.matches(row) {
				continue rows
			}
		}
		if search != "" {
			found := false
			for _, cell := range row {
				if strings.Contains(normalizeCell(cell), search) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
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
	numeric := c.align == output.AlignRight
	money := c.privacy == PrivacyMoney
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

// columnLookup resolves the names a model sends for columns: a
// registry name, a header as the results print it, either without its
// currency or percent suffix, the view's principal column for a
// generic money word, and — when exactly one column is that close — a
// name one typo off.
type columnLookup struct {
	cols      []reportColumn
	currency  string
	principal string // registry name; "" when the view has none
}

// resolve returns the registry index for name, and a note when the
// name was not the column's own (the header says which column ran).
// preferOutCcy reads a base-currency aggregate as its output-currency
// twin: rows in their own currencies cannot be ordered against each
// other, so a sort by total_value means total_value_<CCY>.
func (l columnLookup) resolve(name string, preferOutCcy bool) (int, string, error) {
	i, note, err := l.find(name)
	if err != nil || !preferOutCcy {
		return i, note, err
	}
	if twin := l.byName(l.cols[i].name + "_outccy"); twin >= 0 {
		return twin, l.note(name, twin, true), nil
	}
	return i, note, nil
}

// currencySuffixed splits a name that ends in a currency code.
var currencySuffixed = regexp.MustCompile(`^(.+)_([a-z]{3})$`)

// find is resolve without the twin preference.
func (l columnLookup) find(name string) (int, string, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	ccy := strings.ToLower(l.currency)
	for i, c := range l.cols {
		if strings.ToLower(c.header) == want || c.name == want {
			return i, l.note(name, i, false), nil
		}
	}
	// A name with a currency suffix means a converted column, even when
	// it is misspelt or names another currency: total_valu_usd is
	// total_value_<CCY>, never the base-currency total_value. The
	// result is in one currency, and the note says which.
	if m := currencySuffixed.FindStringSubmatch(want); m != nil && ccy != "" {
		var converted []string
		for _, c := range l.cols {
			if strings.HasSuffix(strings.ToLower(c.header), "_"+ccy) {
				converted = append(converted, l.stem(c))
			}
		}
		stem, ok := m[1], contains(converted, m[1])
		if !ok {
			stem, ok = nearMiss(m[1], converted)
		}
		if ok {
			for i, c := range l.cols {
				if strings.ToLower(c.header) == stem+"_"+ccy {
					note := l.note(name, i, true)
					if m[2] != ccy {
						note += fmt.Sprintf(" (amounts are in %s; pass currency=%s for %s)",
							l.currency, strings.ToUpper(m[2]), strings.ToUpper(m[2]))
					}
					return i, note, nil
				}
			}
		}
	}
	for i, c := range l.cols {
		if l.stem(c) == want {
			return i, l.note(name, i, false), nil
		}
	}
	if l.principal != "" && genericMoneyWords[want] {
		if i := l.byName(l.principal); i >= 0 {
			return i, l.note(name, i, true), nil
		}
	}
	candidates := make([]string, 0, 2*len(l.cols))
	for _, c := range l.cols {
		candidates = append(candidates, c.name, l.stem(c))
	}
	if guess, ok := nearMiss(want, candidates); ok {
		for i, c := range l.cols {
			if c.name == guess || l.stem(c) == guess {
				return i, l.note(name, i, true), nil
			}
		}
	}
	return -1, "", fmt.Errorf("unknown column %q; available: %s", name, l.headers())
}

func (l columnLookup) byName(name string) int {
	for i, c := range l.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// stem is a column's header without its currency suffix, lower case:
// what a model writes when it means the column.
func (l columnLookup) stem(c reportColumn) string {
	return strings.TrimSuffix(strings.ToLower(c.header), "_"+strings.ToLower(l.currency))
}

func (l columnLookup) note(asked string, i int, always bool) string {
	h := l.cols[i].header
	if !always && (strings.EqualFold(asked, h) || strings.EqualFold(asked, l.cols[i].name)) {
		return ""
	}
	return fmt.Sprintf("%s for %q", h, asked)
}

func (l columnLookup) headers() string {
	hs := make([]string, len(l.cols))
	for i, c := range l.cols {
		hs[i] = c.header
	}
	return strings.Join(hs, ", ")
}

// pick resolves the columns parameter: "default" (or nothing), "all",
// a comma list, or a +add,-remove delta on the defaults.
func (l columnLookup) pick(expr string, defaults []string) ([]int, []string, error) {
	var notes []string
	resolve := func(names []string) ([]int, error) {
		out := make([]int, 0, len(names))
		for _, n := range names {
			i, note, err := l.resolve(n, false)
			if err != nil {
				return nil, err
			}
			if note != "" {
				notes = append(notes, note)
			}
			out = append(out, i)
		}
		return out, nil
	}
	defIdx, err := resolve(defaults)
	if err != nil {
		return nil, nil, err
	}
	expr = strings.TrimSpace(expr)
	switch strings.ToLower(expr) {
	case "", "default":
		return defIdx, nil, nil
	case "all":
		all := make([]int, len(l.cols))
		for i := range all {
			all[i] = i
		}
		return all, nil, nil
	}
	if adds, removes, isDelta := parseColumnsDelta(expr); isDelta {
		addIdx, err := resolve(adds)
		if err != nil {
			return nil, nil, err
		}
		remIdx, err := resolve(removes)
		if err != nil {
			return nil, nil, err
		}
		drop := map[int]bool{}
		for _, i := range remIdx {
			drop[i] = true
		}
		var out []int
		seen := map[int]bool{}
		for _, i := range append(defIdx, addIdx...) {
			if !seen[i] && !drop[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
		if len(out) == 0 {
			return nil, nil, fmt.Errorf("columns %q leaves no column", expr)
		}
		return out, notes, nil
	}
	var names []string
	for _, n := range strings.Split(expr, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	out, err := resolve(names)
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("columns %q names no column", expr)
	}
	return out, notes, nil
}

// escalate picks the view a call's filters need. A small model starts
// at the coarse default view and adds a filter — spending with a
// category, holdings with a tax wrapper — and the coarse view has no
// such column. Instead of failing, the first view at or after the
// requested one, coarse to fine, that carries every named filter
// runs. ok is false when no view carries them all; the requested view
// is returned and the caller says which views carry which filter.
func escalate(views []string, carries map[string][]string, view string, filters []string) (string, bool) {
	if len(filters) == 0 {
		return view, true
	}
	start := 0
	for i, v := range views {
		if v == view {
			start = i
		}
	}
	for _, v := range views[start:] {
		all := true
		for _, f := range filters {
			if !contains(carries[v], f) {
				all = false
				break
			}
		}
		if all {
			return v, true
		}
	}
	return view, false
}

// thousands renders n with comma separators, for the paging line.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
