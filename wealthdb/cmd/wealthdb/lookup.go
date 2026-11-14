package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

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
	if twin := l.twinOf(i); twin >= 0 {
		return twin, l.note(name, twin, true), nil
	}
	return i, note, nil
}

// twinOf is the index of column i's output-currency twin (Twin), -1 for
// none.
func (l columnLookup) twinOf(i int) int {
	for j, c := range l.cols {
		if c.twin == l.cols[i].name {
			return j
		}
	}
	return -1
}

// currencySuffixed splits a name that ends in a currency code.
var currencySuffixed = regexp.MustCompile(`^(.+)_([a-z]{3})$`)

// find is resolve without the twin preference.
func (l columnLookup) find(name string) (int, string, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	ccy := strings.ToLower(l.currency)
	if i := l.exact(name); i >= 0 {
		return i, l.note(name, i, false), nil
	}
	// A name with a currency suffix means a converted column, even when
	// it is misspelt or names another currency: total_valu_usd is
	// total_value_<CCY>, never the base-currency total_value; _pct is
	// the percentage suffix, not a currency. The result is in one
	// currency, and the note says which, and how to get another
	// reporting currency the suffix names: start_val is start_<CCY>, but
	// val is no currency to pass.
	if m := currencySuffixed.FindStringSubmatch(want); m != nil && m[2] != "pct" && ccy != "" {
		var converted []string
		for _, c := range l.cols {
			if c.converted() {
				converted = append(converted, l.stem(c))
			}
		}
		stem, ok := m[1], slices.Contains(converted, m[1])
		if !ok {
			stem, ok = nearMiss(m[1], converted)
		}
		if ok {
			for i, c := range l.cols {
				if c.converted() && l.stem(c) == stem {
					note := l.note(name, i, true)
					if m[2] != ccy && slices.Contains(gold.MaterializedCurrencies(), strings.ToUpper(m[2])) {
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
	// A typo is matched only to a column of its own kind: a name ending
	// in _pct never lands on a money column.
	pct := strings.HasSuffix(want, "_pct")
	candidates := make([]string, 0, 2*len(l.cols))
	for _, c := range l.cols {
		if !pct || c.kind == PercentColumn {
			candidates = append(candidates, c.name, l.stem(c))
		}
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

// exact is the column name names as its registry name or its header,
// any case; -1 for none. The CLI's -C reads names this way.
func (l columnLookup) exact(name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, c := range l.cols {
		if strings.ToLower(c.header) == want || c.name == want {
			return i
		}
	}
	return -1
}

// exactOrStem is the first column name names as its registry name, its
// header or its stem, any case; -1 for none. A value that names a
// column, and a column printed elsewhere, read names this way.
func (l columnLookup) exactOrStem(name string) int {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, c := range l.cols {
		if strings.ToLower(c.header) == want || c.name == want || l.stem(c) == want {
			return i
		}
	}
	return -1
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
	h := strings.ToLower(c.header)
	if !c.converted() {
		return h
	}
	return strings.TrimSuffix(h, "_"+strings.ToLower(l.currency))
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
	e := parseColumnsExpr(expr, true)
	switch {
	case e.defaults:
		return defIdx, nil, nil
	case e.all:
		all := make([]int, len(l.cols))
		for i := range all {
			all[i] = i
		}
		return all, nil, nil
	case e.delta:
		addIdx, err := resolve(e.adds)
		if err != nil {
			return nil, nil, err
		}
		remIdx, err := resolve(e.removes)
		if err != nil {
			return nil, nil, err
		}
		out := applyColumnsDelta(defIdx, addIdx, remIdx)
		if len(out) == 0 {
			return nil, nil, fmt.Errorf("columns %q leaves no column", expr)
		}
		return out, notes, nil
	}
	out, err := resolve(e.names)
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("columns %q names no column", expr)
	}
	return out, notes, nil
}
