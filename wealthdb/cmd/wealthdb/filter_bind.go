package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ---- binding ------------------------------------------------------------

// filterTarget is one view's columns, for conditions to bind to.
type filterTarget struct {
	view    string // the view's name in a message ("positions"; "" for a tool of one shape)
	lookup  columnLookup
	privacy bool
	now     time.Time
	front   filterFront
	// elsewhere names the views (and tools) that print a column this
	// view lacks, for an unknown column's message; nil for none.
	elsewhere func(col string) string
}

// condition is a bound condition: which column, how its cells compare.
type condition struct {
	col    int
	header string
	op     *opSpec
	kind   colKind
	texts  []string   // normalized values: one for = and contains, the list for in
	nums   []float64  // number and money values
	days   [][2]int64 // date values, each a period [first, last] in epoch days
	canon  string     // the canonical form, for the header
	note   string     // a name resolved to another column, for the header
}

// bind resolves raw conditions against the view. Every bad condition,
// those that did not parse (errs) and those that do not bind, is one
// line of the error, in the order sent.
func (t filterTarget) bind(raws []rawCond, errs condErrors) ([]condition, error) {
	var out []condition
	for _, r := range raws {
		c, err := t.bindOne(r)
		if err != nil {
			if _, ok := errs[r.index]; !ok {
				errs[r.index] = t.front.line(r.written, err.Error())
			}
			continue
		}
		out = append(out, c)
	}
	if err := errs.err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t filterTarget) bindOne(r rawCond) (condition, error) {
	i, note, err := t.lookup.find(r.col)
	if err != nil {
		return condition{}, t.unknownColumn(r)
	}
	col := t.lookup.cols[i]
	c := condition{col: i, header: col.header, op: r.op, kind: col.filterKind(), note: note}
	if t.privacy && redactedClass(col.privacy) {
		return c, t.redacted(col)
	}
	if !slices.Contains(r.op.kinds, c.kind) {
		return c, kindError(r, col, c.kind)
	}
	if r.missing {
		return c, condSyntaxError{fmt.Sprintf("%q needs a value after it, such as %q", r.spelled, col.header+" "+r.op.name+" "+kindWords[c.kind].example)}
	}
	values := r.list
	switch {
	case r.op.arg == argOne:
		values = []string{r.value}
	case c.kind == kindNumber || c.kind == kindMoney:
		// 1,000, 2,000 is two amounts with thousands commas; 100,200
		// may be one amount or two.
		var ambiguous bool
		if values, ambiguous = splitNumberList(r.value); ambiguous {
			return c, numberListError(r, col)
		}
	}
	var canon []string
	var ranges []string
	for _, v := range values {
		switch c.kind {
		case kindNumber, kindMoney:
			if other, ok := t.columnNamed(v); ok {
				return c, t.twoColumns(r, col, other)
			}
			n, plain, err := parseNumberValue(v, col.kind == PercentColumn)
			if err != nil {
				return c, valueError(r, col, v, err)
			}
			c.nums = append(c.nums, n)
			canon = append(canon, plain)
		case kindDate:
			if other, ok := t.columnNamed(v); ok {
				return c, t.twoColumns(r, col, other)
			}
			lo, hi, err := parseDateValue(v, t.now)
			if err != nil {
				return c, valueError(r, col, v, err)
			}
			c.days = append(c.days, [2]int64{lo, hi})
			canon = append(canon, v)
			if v != epochDayString(lo) || lo != hi {
				ranges = append(ranges, periodNote(r.op.name, lo, hi))
			}
		default:
			c.texts = append(c.texts, normalizeCell(v))
			canon = append(canon, quoteListItem(v, r.op.arg == argList))
		}
	}
	c.canon = col.header + " " + r.op.name
	if r.op.arg != argNone {
		c.canon += " " + strings.Join(canon, ", ")
	}
	if len(ranges) > 0 && r.op.arg == argOne {
		c.canon += " (" + strings.Join(ranges, ", ") + ")"
	}
	return c, nil
}

// redactedClass is a privacy class whose cells the privacy endpoint
// hides, so a condition on it could only probe the hidden value.
func redactedClass(p PrivacyClass) bool {
	return p == PrivacyMoney || p == PrivacyQuantity || p == PrivacyFreeText
}

// columnNamed reports the column a value names exactly, for a
// comparison of two columns.
func (t filterTarget) columnNamed(v string) (reportColumn, bool) {
	if i := t.lookup.exactOrStem(v); i >= 0 {
		return t.lookup.cols[i], true
	}
	return reportColumn{}, false
}
