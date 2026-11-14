package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// keeps reports whether a printed cell meets the condition. A missing
// cell meets only is empty and the negations.
func (c condition) keeps(cell string) bool {
	switch c.op.name {
	case "is empty":
		return missingCell(cell)
	case "is not empty":
		return !missingCell(cell)
	case "!=", "not in", "not contains":
		if missingCell(cell) {
			return true
		}
	}
	if missingCell(cell) {
		return false
	}
	switch c.kind {
	case kindNumber, kindMoney:
		f, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		if err != nil {
			return c.negated()
		}
		return c.compare(func(i int) int { return cmpFloat(f, c.nums[i]) }, len(c.nums))
	case kindDate:
		d, ok := cellDay(cell)
		if !ok {
			return c.negated()
		}
		return c.compare(func(i int) int {
			switch {
			case d < c.days[i][0]:
				return -1
			case d > c.days[i][1]:
				return 1
			}
			return 0
		}, len(c.days))
	}
	norm := normalizeCell(cell)
	switch c.op.name {
	case "contains":
		return strings.Contains(norm, c.texts[0])
	case "not contains":
		return !strings.Contains(norm, c.texts[0])
	}
	return c.compare(func(i int) int { return strings.Compare(norm, c.texts[i]) }, len(c.texts))
}

// negated is what a cell that does not read as the column's kind
// meets: the negations only.
func (c condition) negated() bool {
	return c.op.name == "!=" || c.op.name == "not in"
}

// compare applies the operator to cmp(i), the cell against value i:
// in and not in over every value, the others over the one.
func (c condition) compare(cmp func(int) int, n int) bool {
	switch c.op.name {
	case "=":
		return cmp(0) == 0
	case "!=":
		return cmp(0) != 0
	case "<":
		return cmp(0) < 0
	case "<=":
		return cmp(0) <= 0
	case ">":
		return cmp(0) > 0
	case ">=":
		return cmp(0) >= 0
	}
	for i := range n {
		if cmp(i) == 0 {
			return c.op.name == "in"
		}
	}
	return c.op.name == "not in"
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// keepRows returns the rows of idx every condition keeps, and how many
// were left after each condition, in order.
func keepRows(cells [][]string, idx []int, conds []condition) ([]int, []int) {
	left := make([]int, len(conds))
	out := make([]int, 0, len(idx))
rows:
	for _, i := range idx {
		for j, c := range conds {
			if !c.keeps(cells[i][c.col]) {
				continue rows
			}
			left[j]++
		}
		out = append(out, i)
	}
	return out, left
}

// canonicalForms is the header's echo of the conditions, in canonical form.
func canonicalForms(conds []condition) []string {
	out := make([]string, len(conds))
	for i, c := range conds {
		out[i] = c.canon
	}
	return out
}

// emptiedNote says which condition emptied a result: the rows each
// kept, in order, and for the first that kept none the values its
// column takes, nearest first, or the mirrored money bound where that
// would match.
func emptiedNote(cells [][]string, idx []int, conds []condition) string {
	_, left := keepRows(cells, idx, conds)
	var b strings.Builder
	fmt.Fprintf(&b, "of %s %s, ", thousands(len(idx)), plural(len(idx), "row", "rows"))
	before := idx
	for j, c := range conds {
		if j > 0 {
			b.WriteString(", then ")
		}
		fmt.Fprintf(&b, "%s kept %s", c.canon, thousands(left[j]))
		if left[j] == 0 {
			b.WriteString(c.whyNone(cells, before))
			break
		}
		before, _ = keepRows(cells, before, []condition{c})
	}
	return b.String()
}

// whyNone explains a condition that kept none of rows.
func (c condition) whyNone(cells [][]string, rows []int) string {
	switch {
	case c.kind == kindText && (c.op.name == "=" || c.op.name == "in"):
		seen := map[string]bool{}
		var vals []string
		for _, i := range rows {
			v := strings.TrimSpace(cells[i][c.col])
			if v != "" && !seen[v] {
				seen[v] = true
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			return ""
		}
		sortNearest(vals, c.texts[0])
		more := ""
		if len(vals) > 12 {
			vals, more = vals[:12], ", …"
		}
		return fmt.Sprintf(" (%s values: %s%s)", c.header, strings.Join(vals, ", "), more)
	case c.kind == kindMoney && len(c.nums) == 1:
		// "Over 500" written for money leaving: the bound past zero on
		// the other side.
		mirror := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}[c.op.name]
		if mirror == "" || (c.op.name == ">" || c.op.name == ">=") != (c.nums[0] > 0) || c.nums[0] == 0 {
			return ""
		}
		m := c
		m.op, m.nums = opByName(mirror), []float64{-c.nums[0]}
		if kept, _ := keepRows(cells, rows, []condition{m}); len(kept) > 0 {
			return fmt.Sprintf(" (money is signed; %s keeps %s)", c.header+" "+mirror+" "+strconv.FormatFloat(-c.nums[0], 'f', -1, 64),
				thousands(len(kept)))
		}
	}
	return ""
}

// sortNearest orders vals by how close each is to want, closest first:
// the initials of a name ("United States" for US) count as close.
func sortNearest(vals []string, want string) {
	score := func(v string) float64 {
		n := normalizeCell(v)
		s := similarity(want, n)
		if initials(want) == n || initials(n) == want {
			s = 1
		}
		if strings.Contains(n, want) || strings.Contains(want, n) {
			s = max(s, 0.9)
		}
		return s
	}
	sort.SliceStable(vals, func(a, b int) bool { return score(vals[a]) > score(vals[b]) })
}

// initials are the first letters of a text's words: "united states" is
// "us".
func initials(s string) string {
	var b strings.Builder
	for _, w := range strings.Fields(s) {
		b.WriteString(w[:1])
	}
	return b.String()
}
