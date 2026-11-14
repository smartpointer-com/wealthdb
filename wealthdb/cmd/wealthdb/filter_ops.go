package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Row conditions: the rows a call or a command keeps, by the printed
// cells of its view. The MCP tools take them as filter, the CLI's
// report commands as -F, and both bind, keep and explain them here, so
// the two front-ends agree on every row and every message.
//
// A condition is "column op value": the column is the leading name, the
// operator the earliest match of the operator table, and the value the
// rest, unquoted. A row is kept when every condition holds. Conditions
// read a report's printed cells, privacy applied, after the report has
// run, so a share stays a share of the whole and the privacy endpoint
// cannot be probed by a filter.

// colKind is what a column's cells are, for the operators and the
// values a condition on it takes.
type colKind int

const (
	kindText colKind = iota
	kindNumber
	kindMoney
	kindDate
)

// kindWords name a kind in prose: one cell's kind ("a number"), the
// kind of several ("numbers"), the bare word for the help ("number"),
// and a value of it for an example.
var kindWords = map[colKind]struct{ one, many, bare, example string }{
	kindText:   {"text", "text", "text", "x"},
	kindNumber: {"a number", "numbers", "number", "10"},
	kindMoney:  {"money", "money", "money", "1000"},
	kindDate:   {"a date", "dates", "date", "2027"},
}

// filterKind is the kind of value a condition on the column reads, by
// the column's kind: text, a date, money, or a number for every other
// figure.
func (c reportColumn) filterKind() colKind {
	switch c.kind {
	case TextColumn:
		return kindText
	case DateColumn:
		return kindDate
	case MoneyColumn:
		return kindMoney
	}
	return kindNumber
}

// numeric reports whether the column's cells are figures.
func (c reportColumn) numeric() bool { return c.kind.figure() }

// converted reports whether the column holds money in the output
// currency, its header ending in _<CCY>.
func (c reportColumn) converted() bool { return c.kind == MoneyColumn && c.unit == OutputCurrency }

// argShape is what follows an operator.
type argShape int

const (
	argOne  argShape = iota // one value
	argList                 // a comma list
	argNone                 // nothing
)

// opSpec is one operator: its canonical spelling, the kinds it
// compares, what a row needs to be kept, and the spellings read as it.
// The parser, the schema doc, the help and the messages all read this
// table.
type opSpec struct {
	name  string
	kinds []colKind
	keeps string
	arg   argShape
	// alias are spellings read as this operator and echoed in the
	// canonical one.
	alias []string
}

var (
	allKinds     = []colKind{kindText, kindNumber, kindMoney, kindDate}
	orderedKinds = []colKind{kindNumber, kindMoney, kindDate}
)

var operators = []opSpec{
	{name: "=", kinds: allKinds, keeps: "equals the value: the whole cell, any case, an underscore read as a space; a date or a period is any day in it", alias: []string{"=="}},
	{name: "!=", kinds: allKinds, keeps: "does not equal it", alias: []string{"<>", "≠"}},
	{name: "<", kinds: orderedKinds, keeps: "is less, or before"},
	{name: "<=", kinds: orderedKinds, keeps: "is at most, or not after", alias: []string{"≤"}},
	{name: ">", kinds: orderedKinds, keeps: "is greater, or after"},
	{name: ">=", kinds: orderedKinds, keeps: "is at least, or not before", alias: []string{"≥"}},
	{name: "in", kinds: allKinds, keeps: "equals one of a comma list", arg: argList},
	{name: "not in", kinds: allKinds, keeps: "equals none of a comma list", arg: argList},
	{name: "contains", kinds: []colKind{kindText}, keeps: "holds the text", alias: []string{"like"}},
	{name: "not contains", kinds: []colKind{kindText}, keeps: "does not hold the text", alias: []string{"not like"}},
	{name: "is empty", kinds: allKinds, keeps: "is blank or n/a", arg: argNone, alias: []string{"is null"}},
	{name: "is not empty", kinds: allKinds, keeps: "is not blank", arg: argNone, alias: []string{"is not null"}},
}

// nearOperators are spellings that are no operator but name one: a
// condition that uses one is refused with the operator it meant.
var nearOperators = map[string]string{
	"=>": ">=", "=<": "<=", "!contains": "not contains", "!in": "not in", "not_in": "not in", "notin": "not in",
	"not_contains": "not contains", "notcontains": "not contains", "is not": "!=", "is": "=",
	"!like": "not contains", "includes": "contains", "has": "contains",
}

func opByName(name string) *opSpec {
	for i := range operators {
		if operators[i].name == name {
			return &operators[i]
		}
	}
	return nil
}

// operatorNames lists the operators for a message: "= != < <= > >= in,
// not in, contains, not contains, is empty, is not empty".
func operatorNames() string {
	var sym, words []string
	for _, op := range operators {
		if unicode.IsLetter(rune(op.name[0])) {
			words = append(words, op.name)
		} else {
			sym = append(sym, op.name)
		}
	}
	return strings.Join(sym, " ") + " " + strings.Join(words, ", ")
}

// opsForKind lists the operators a kind takes.
func opsForKind(k colKind) string {
	var out []string
	for _, op := range operators {
		if slices.Contains(op.kinds, k) {
			out = append(out, op.name)
		}
	}
	return strings.Join(out, ", ")
}

// kindsOf names the kinds an operator compares: "numbers, money and
// dates".
func kindsOf(op *opSpec) string {
	var out []string
	for _, k := range op.kinds {
		out = append(out, kindWords[k].many)
	}
	return andList(out)
}

// spelling is one way an operator is written in a condition, and what
// it reads as: an operator of the table, or a near miss.
type spelling struct {
	text string
	op   *opSpec // nil for a near miss
	near string  // the operator a near miss meant
	word bool    // a word: it must stand between spaces
}

// spellings are every operator spelling, longest first, so "<=" wins
// over "<" and "not in" over "in" at the same position.
var spellings = func() []spelling {
	var out []spelling
	add := func(text string, op *opSpec, near string) {
		first, _ := utf8.DecodeRuneInString(text)
		out = append(out, spelling{text: text, op: op, near: near, word: unicode.IsLetter(first) || text[0] == '!' && len(text) > 2})
	}
	for i := range operators {
		op := &operators[i]
		add(op.name, op, "")
		for _, a := range op.alias {
			add(a, op, "")
		}
	}
	for text, near := range nearOperators {
		add(text, nil, near)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if len(out[a].text) != len(out[b].text) {
			return len(out[a].text) > len(out[b].text)
		}
		return out[a].text < out[b].text
	})
	return out
}()

// ---- help ---------------------------------------------------------------

// filterHelp is `wealthdb help filter` and describe(topic=filter): one
// text, generated from the operator table.
func filterHelp(cli bool) string {
	param, example := "filter", `filter: ["asset_class = fixed_income", "maturity_date = 2027"]`
	if cli {
		param, example = "-F", `wealthdb yield positions -F "asset_class = fixed_income" -F "maturity_date = 2027"`
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Conditions on a result's columns: %s takes one condition per item, and a row is kept when every condition holds.\n", param)
	b.WriteString("A condition is column op value. The column is any column the view prints, by its header (value_USD) or without its currency (value). The value is the rest of the condition and needs no quotes.\n\nOperators (a row is kept when the cell ...):\n")
	for _, op := range operators {
		var ks []string
		for _, k := range op.kinds {
			ks = append(ks, kindWords[k].bare)
		}
		fmt.Fprintf(&b, "  %-13s %s (%s)\n", op.name, op.keeps, strings.Join(ks, ", "))
	}
	b.WriteString("\nValues:\n")
	b.WriteString("- Text compares in any case, an underscore read as a space. A list item that holds a comma goes in quotes.\n")
	b.WriteString("- Numbers and money are plain numbers, and money compares signed: spending lines are negative, so \"over 500\" is value <= -500.\n")
	b.WriteString("- Dates are " + valueDateForms + ". A year, a quarter or a month is the whole period: maturity_date = 2027 keeps 2027, < 2027 what comes before it, >= 2027-07 from 1 July 2027 on.\n")
	b.WriteString("- A blank cell meets only is empty and the negations (!=, not in, not contains).\n")
	b.WriteString("\nRead as meant and echoed in the form above: == <> ≠ ≥ ≤, is null, like '%x%', between a and b, in (a, b), a leading WHERE, quotes around a value, several conditions joined by and.\n")
	b.WriteString("Refused: or (make one call per alternative, or use in for one column), two columns compared (a view's gain or _pct column holds the comparison), functions, ORDER BY, GROUP BY, LIMIT.\n")
	if cli {
		b.WriteString("\nOn the command line each condition goes in double quotes. $ expands in a shell, so amounts are plain numbers; the negations are words, since ! before a letter is history expansion.\n")
	}
	b.WriteString("\nExample: " + example)
	return b.String()
}

// filterDoc is the filter parameter's doc in each tool's schema; the
// grammar is in the instructions (filterInstruction).
func filterDoc() string {
	return `Conditions, all of which hold: ["value > 1000", "date >= 2025-07", "category not in Rent, Groceries"].`
}

// filterInstruction is the instructions' line on filter: the grammar,
// said once for every tool.
func filterInstruction() string {
	return "- filter: a list of conditions, each \"column op value\" on a column the view prints, op one of " + operatorNames() +
		"; a row is kept when all hold. The value needs no quotes; a year or a month is the whole period; money leaving is negative."
}
