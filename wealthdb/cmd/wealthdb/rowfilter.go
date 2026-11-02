package main

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
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

var kindNames = map[colKind]string{kindText: "text", kindNumber: "a number", kindMoney: "money", kindDate: "a date"}

// kind is the column's kind: a date by the registry's mark, money by
// its privacy class, a number by its alignment, else text.
func (c reportColumn) kind() colKind {
	switch {
	case c.date:
		return kindDate
	case c.privacy == PrivacyMoney:
		return kindMoney
	case c.align == output.AlignRight:
		return kindNumber
	}
	return kindText
}

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
	plural := map[colKind]string{kindText: "text", kindNumber: "numbers", kindMoney: "money", kindDate: "dates"}
	var out []string
	for _, k := range op.kinds {
		out = append(out, plural[k])
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
		out = append(out, spelling{text: text, op: op, near: near, word: unicode.IsLetter(rune(text[0])) || text[0] == '!' && len(text) > 2})
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

// filterFront is how one front-end names things in a message: its
// prefix ("yield:", "wealthdb yield positions:"), the parameter that
// carries conditions, and the spelling of the parameters a hint names.
type filterFront struct {
	prefix string
	cli    bool
}

func (f filterFront) param() string {
	if f.cli {
		return "-F"
	}
	return "filter"
}

// list spells conditions as this front-end takes them.
func (f filterFront) list(conds ...string) string {
	if f.cli {
		parts := make([]string, len(conds))
		for i, c := range conds {
			parts[i] = "-F " + strconv.Quote(c)
		}
		return strings.Join(parts, " ")
	}
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = strconv.Quote(c)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// alternatives is the advice for an "or": one call or run per branch,
// each with the conditions of every branch.
func (f filterFront) alternatives(branches [][]string) string {
	spelled := make([]string, len(branches))
	for i, b := range branches {
		spelled[i] = f.list(b...)
		if !f.cli {
			spelled[i] = "filter " + spelled[i]
		}
	}
	once, each := "make one call with %s and one with %s", "make one call with each of %s"
	if f.cli {
		once, each = "run the command once with %s and once with %s", "run the command once with each of %s"
	}
	if len(spelled) == 2 {
		return fmt.Sprintf(once, spelled[0], spelled[1])
	}
	return fmt.Sprintf(each, andList(spelled))
}

// line is one message line: the prefix, the condition as written, the
// message.
func (f filterFront) line(written, msg string) string {
	return fmt.Sprintf("%s %s %s: %s", f.prefix, f.param(), strconv.Quote(written), msg)
}

// filterError is every bad condition of a call, one line each.
type filterError struct{ lines []string }

func (e *filterError) Error() string { return strings.Join(e.lines, "\n") }

// rawCond is one condition split, before it meets a view: its column
// name, operator and value as written.
type rawCond struct {
	index   int    // the item's place among those sent
	written string // the item as sent, for messages
	col     string
	op      *opSpec
	value   string
	list    []string // argList: the items, unquoted
	// missing marks an operator written with no value after it, which
	// the binding names an example for in the column's kind.
	missing bool
	spelled string // the operator as written
	// lenient notes a reading that is not the canonical form: the
	// header's canonical echo shows it.
	lenient bool
}

// condSyntaxError is a condition that cannot be split, with its message.
type condSyntaxError struct{ msg string }

func (e condSyntaxError) Error() string { return e.msg }

var (
	identRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	sqlWordRe = regexp.MustCompile(`(?i)^\s*(select|order\s+by|group\s+by|limit|having|offset)\b`)
	// A clause that ends a condition: a value may hold the words
	// ("credit limit fee"), never this shape.
	sqlTailRe = regexp.MustCompile(`(?i)\s((?:order|group)\s+by)\s+[A-Za-z_][A-Za-z0-9_]*(?:\s+(?:asc|desc))?\s*$|\s(limit|offset)\s+\d+\s*$`)
	funcRe    = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)\s*(.*)$`)
)

// condErrors are the messages of a call's bad conditions, by the place
// of each among those sent.
type condErrors map[int]string

// err is every message in the order the conditions were sent, or nil.
func (e condErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	idx := make([]int, 0, len(e))
	for i := range e {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	lines := make([]string, len(idx))
	for j, i := range idx {
		lines[j] = e[i]
	}
	return &filterError{lines: lines}
}

// parseConditions splits each written condition into one or more raw
// conditions: a string that joins several with "and", or a between,
// yields several. A bad condition is one message; front names the
// parameters in a hint.
func parseConditions(written []string, front filterFront) ([]rawCond, condErrors) {
	var out []rawCond
	errs := condErrors{}
	for i, w := range written {
		conds, err := splitCondition(w, front)
		if err != nil {
			errs[i] = front.line(w, err.Error())
			continue
		}
		for j := range conds {
			conds[j].index = i
		}
		out = append(out, conds...)
	}
	return out, errs
}

// splitCondition reads one written condition: its conjuncts (several
// joined by "and" read as meant), each a condition or a between.
func splitCondition(written string, front filterFront) ([]rawCond, error) {
	text := strings.TrimSpace(written)
	lenient := false
	if text == "" {
		return nil, condSyntaxError{"an empty condition; write column op value, such as \"value > 1000\""}
	}
	if pos, q := unclosedQuote(text); pos >= 0 {
		return nil, unclosedQuoteError(text, pos, q)
	}
	if strings.HasPrefix(asciiLower(text), "where ") {
		text, lenient = strings.TrimSpace(text[len("where "):]), true
	}
	if trimmed := strings.TrimRight(text, "; "); trimmed != text {
		text, lenient = trimmed, true // a statement's closing semicolon
	}
	if err := sqlClause(text, front); err != nil {
		return nil, err
	}
	if inner, ok := outerParens(text); ok {
		text, lenient = inner, true
	}
	parts := conjuncts(text)
	for i, part := range parts {
		inner, _ := outerParens(part)
		if branches := orBranches(inner); len(branches) > 1 {
			return nil, orError(parts, i, branches, front)
		}
	}
	for _, part := range parts {
		if len(wordPositions(" "+part+" ", "between")) > 0 && betweenRe.FindStringSubmatch(part) == nil {
			return nil, betweenError(part)
		}
	}
	if last := parts[len(parts)-1]; strings.HasSuffix(asciiLower(last), " and") || asciiLower(last) == "and" {
		head := strings.TrimSpace(last[:len(last)-len("and")])
		return nil, condSyntaxError{fmt.Sprintf("nothing follows the \"and\"; write %q, and each other condition in full as its own item", head)}
	}
	var out []rawCond
	for _, part := range parts {
		var conds []rawCond
		if m := betweenRe.FindStringSubmatch(part); m != nil {
			for _, half := range []string{m[1] + " >= " + m[2], m[1] + " <= " + m[3]} {
				c, err := splitOne(written, half, front)
				if err != nil {
					return nil, err
				}
				conds = append(conds, c...)
			}
		} else {
			c, err := splitOne(written, part, front)
			if err != nil {
				return nil, err
			}
			conds = c
		}
		for i := range conds {
			conds[i].lenient = conds[i].lenient || lenient || len(parts) > 1
		}
		out = append(out, conds...)
	}
	return out, nil
}

// betweenError refuses a between that lacks a bound, giving the whole
// form and the two conditions it reads as.
func betweenError(part string) error {
	col := identRe.FindString(strings.TrimSpace(part))
	lo := "5"
	if i := strings.Index(asciiLower(part), " between "); i >= 0 {
		rest := strings.TrimSpace(part[i+len(" between "):])
		if f := strings.Fields(rest); len(f) > 0 && !strings.EqualFold(f[0], "and") {
			lo = f[0]
		}
	}
	hi := "10"
	if n, err := strconv.ParseFloat(lo, 64); err == nil && n >= 10 {
		hi = strconv.FormatFloat(n*2, 'f', -1, 64)
	}
	return condSyntaxError{fmt.Sprintf("between takes two values joined by and; write %q, which reads as %q and %q",
		col+" between "+lo+" and "+hi, col+" >= "+lo, col+" <= "+hi)}
}

// orBranches splits text at each "or" or "||" outside quotes and
// parentheses; one branch when it holds none.
func orBranches(text string) []string {
	var out []string
	for _, p := range splitWord(text, "or") {
		for _, q := range strings.Split(p, "||") {
			out = append(out, strings.TrimSpace(q))
		}
	}
	return out
}

// splitOne reads a single condition: column, operator, value.
func splitOne(written, text string, front filterFront) ([]rawCond, error) {
	text = strings.TrimSpace(text)
	lenient := false
	if inner, ok := outerParens(text); ok {
		text, lenient = inner, true
	}
	if m := funcRe.FindStringSubmatch(text); m != nil {
		return nil, functionError(m[1], m[2], m[3])
	}
	if strings.ContainsAny(beforeOperator(text), "()") {
		return nil, condSyntaxError{fmt.Sprintf("parentheses group nothing: every condition must hold, so send each as its own item: %s",
			front.list("value > 1000", "date >= 2025"))}
	}
	pos, sp, ok := findOperator(text)
	if !ok {
		return nil, noOperatorError(text)
	}
	col := strings.TrimSpace(text[:pos])
	value := strings.TrimSpace(text[pos+len(sp.text):])
	if col == "" {
		return nil, condSyntaxError{fmt.Sprintf("no column before %q; a condition starts with the column it reads, such as \"value %s 1000\"",
			sp.text, orDefault(sp.near, sp.text))}
	}
	if !identRe.MatchString(col) || identRe.FindString(col) != col {
		// "market value > 5": a name with spaces means the column with
		// underscores.
		joined := strings.Join(strings.Fields(strings.ReplaceAll(col, "-", "_")), "_")
		if identRe.FindString(joined) != joined {
			return nil, condSyntaxError{fmt.Sprintf("%q is not a column name; a column is one word, such as value_USD or maturity_date", col)}
		}
		col, lenient = joined, true
	}
	if sp.op == nil {
		fixed := col + " " + sp.near
		if value != "" {
			fixed += " " + value
		}
		msg := fmt.Sprintf("%q is not an operator; write %q", sp.text, fixed)
		if value == "" && (sp.text == "is" || sp.text == "is not") {
			// "name is" most likely asks for the empty cell.
			msg = fmt.Sprintf("%q needs a word after it; write %q, or %q for a value", sp.text, col+" "+sp.text+" empty", fixed+" x")
		}
		if strings.HasPrefix(sp.text, "!") {
			msg += " (a \"!\" before a word is history expansion in a shell, so every negation but != is a word)"
		}
		return nil, condSyntaxError{msg}
	}
	op := sp.op
	lenient = lenient || sp.text != op.name
	if sp.text == "like" || sp.text == "not like" {
		value = strings.Trim(unquote(value), "%")
	}
	if op.arg == argOne && value != "" && unquote(value) == "" {
		value, lenient = "", true // a quoted empty value: the empty cell
	}
	switch {
	case value != "" || op.arg != argOne:
	case op.name == "=":
		// "col =" with nothing after it: the empty cell.
		return []rawCond{{written: written, col: col, op: opByName("is empty"), lenient: true}}, nil
	case op.name == "!=":
		return []rawCond{{written: written, col: col, op: opByName("is not empty"), lenient: true}}, nil
	default:
		return []rawCond{{written: written, col: col, op: op, missing: true, spelled: sp.text}}, nil
	}
	switch op.arg {
	case argNone:
		if value != "" {
			return nil, condSyntaxError{fmt.Sprintf("%q takes no value; write %q", op.name, col+" "+op.name)}
		}
		return []rawCond{{written: written, col: col, op: op, lenient: lenient}}, nil
	case argList:
		if value == "" {
			return nil, condSyntaxError{fmt.Sprintf("%q needs a comma list after it, such as %q", op.name, col+" "+op.name+" a, b")}
		}
		if inner, ok := outerParens(value); ok {
			value, lenient = inner, true
		} else if strings.HasPrefix(value, "(") != strings.HasSuffix(value, ")") {
			return nil, condSyntaxError{fmt.Sprintf("a parenthesis around the list never closes; write %q",
				col+" "+op.name+" "+strings.Trim(value, "() "))}
		}
		items := splitList(value)
		if len(items) == 0 {
			return nil, condSyntaxError{fmt.Sprintf("%q needs a comma list after it, such as %q", op.name, col+" "+op.name+" a, b")}
		}
		return []rawCond{{written: written, col: col, op: op, value: value, list: items, lenient: lenient}}, nil
	}
	if unq := unquote(value); unq != value {
		value, lenient = unq, true
	}
	return []rawCond{{written: written, col: col, op: op, value: value, lenient: lenient}}, nil
}

// betweenRe reads "col between a and b".
var betweenRe = regexp.MustCompile(`(?i)^\s*([A-Za-z_][A-Za-z0-9_]*)\s+between\s+(.+?)\s+and\s+(.+?)\s*$`)

// conjuncts splits text at each "and" outside quotes and parentheses
// whose right side starts a condition of its own (a column, then an
// operator or between), but the "and" a between owns: a value that
// holds "and" stays whole.
func conjuncts(text string) []string {
	var parts []string
	start := 0
	owned := false // the current part's between has its "and"
	for _, i := range wordPositions(text, "and") {
		part := text[start:i]
		if len(wordPositions(part, "between")) > 0 && !owned {
			owned = true
			continue
		}
		if startsCondition(text[i+3:]) {
			parts = append(parts, strings.TrimSpace(part))
			start, owned = i+3, false
		}
	}
	return append(parts, strings.TrimSpace(text[start:]))
}

// startsCondition reports whether text opens with a column and then an
// operator of the table or between.
func startsCondition(text string) bool {
	text = strings.TrimLeft(strings.TrimSpace(text), "( ")
	col := identRe.FindString(text)
	if col == "" {
		return false
	}
	rest := " " + strings.TrimSpace(text[len(col):])
	if strings.HasPrefix(asciiLower(rest), " between ") {
		return true
	}
	pos, sp, ok := findOperator(rest)
	return ok && sp.op != nil && strings.TrimSpace(rest[:pos]) == ""
}

// asciiLower lower-cases the ASCII letters of s and keeps every other
// byte, so an offset into it is an offset into s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// findOperator finds the earliest operator spelling outside quotes, the
// longest where several start at one position. A word spelling stands
// between spaces.
func findOperator(text string) (int, spelling, bool) {
	lower := asciiLower(text)
	quote := rune(0)
	for i, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			continue
		case (r == '"' || r == '\'') && (i == 0 || text[i-1] == ' ' || text[i-1] == ','):
			quote = r
			continue
		}
		for _, sp := range spellings {
			if !strings.HasPrefix(lower[i:], sp.text) {
				continue
			}
			if sp.word {
				end := i + len(sp.text)
				if i == 0 || text[i-1] != ' ' || end < len(text) && text[end] != ' ' {
					continue
				}
			}
			return i, sp, true
		}
	}
	return 0, spelling{}, false
}

// beforeOperator is the text before the first operator, or all of it.
func beforeOperator(text string) string {
	if pos, _, ok := findOperator(text); ok {
		return text[:pos]
	}
	return text
}

// wordPositions are the byte offsets of word, any case, standing
// between spaces outside quotes and parentheses.
func wordPositions(text, word string) []int {
	var out []int
	lower := asciiLower(text)
	quote := byte(0)
	d := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			continue
		case (c == '"' || c == '\'') && (i == 0 || text[i-1] == ' ' || text[i-1] == ','):
			quote = c
			continue
		case c == '(':
			d++
		case c == ')':
			d--
		}
		if d == 0 && i > 0 && text[i-1] == ' ' && strings.HasPrefix(lower[i:], word) &&
			i+len(word) < len(text) && text[i+len(word)] == ' ' {
			out = append(out, i)
		}
	}
	return out
}

// splitWord splits text at each standalone word outside quotes.
func splitWord(text, word string) []string {
	var parts []string
	last := 0
	for _, i := range wordPositions(text, word) {
		parts = append(parts, strings.TrimSpace(text[last:i]))
		last = i + len(word)
	}
	return append(parts, strings.TrimSpace(text[last:]))
}

// outerParens strips one pair of parentheses that wraps the whole text.
func outerParens(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "(") || !strings.HasSuffix(text, ")") {
		return text, false
	}
	depth := 0
	for i, r := range text {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(text)-1 {
				return text, false
			}
		}
	}
	return strings.TrimSpace(text[1 : len(text)-1]), true
}

// unclosedQuote is the position and the quote character of a quote that
// opens a value or a list item and never closes, or -1. A quote opens
// where a value or an item starts, and closes before a space, a comma,
// a parenthesis or the end; an apostrophe inside a word (Joe's) does
// neither.
func unclosedQuote(text string) (int, byte) {
	open, q := -1, byte(0)
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '"' && c != '\'' {
			continue
		}
		switch {
		case q == 0 && (i == 0 || strings.ContainsRune(" ,(", rune(text[i-1]))):
			q, open = c, i
		case c == q && (i+1 == len(text) || strings.ContainsRune(" ,)", rune(text[i+1]))):
			q, open = 0, -1
		}
	}
	return open, q
}

func unclosedQuoteError(text string, pos int, q byte) error {
	after := strings.Fields(text[pos+1:])
	where := "at the end"
	if len(after) > 0 {
		where = "before " + after[0]
	}
	fixed := strings.TrimSpace(text[:pos] + text[pos+1:])
	if strings.Contains(text[pos+1:], ",") {
		// The quote holds a comma: it meant one item, so close it.
		return condSyntaxError{fmt.Sprintf("the %c %s never closes; close it: %q", q, where, strings.TrimSpace(text)+string(q))}
	}
	if _, _, ok := findOperator(fixed); !ok {
		return condSyntaxError{fmt.Sprintf("the %c %s never closes; write column op value, such as \"name = Miller's Pantry\": a value needs no quotes", q, where)}
	}
	return condSyntaxError{fmt.Sprintf("the %c %s never closes; a value needs no quotes: %q", q, where, fixed)}
}

// unquote strips one pair of matching quotes around s.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// splitList splits a comma list outside quotes; each item is trimmed
// and unquoted.
func splitList(s string) []string {
	var out []string
	q := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case (c == '"' || c == '\'') && strings.TrimSpace(s[start:i]) == "":
			q = c
		case c == ',':
			out = append(out, unquote(strings.TrimSpace(s[start:i])))
			start = i + 1
		}
	}
	out = append(out, unquote(strings.TrimSpace(s[start:])))
	kept := out[:0]
	for _, item := range out {
		if item != "" {
			kept = append(kept, item)
		}
	}
	return kept
}

// noOperatorError suggests the condition with = between the column and
// the value.
func noOperatorError(text string) error {
	fields := strings.Fields(text)
	msg := "no operator; the operators are " + operatorNames()
	if len(fields) >= 2 && identRe.FindString(fields[0]) == fields[0] {
		msg += fmt.Sprintf(". Write %q", fields[0]+" = "+strings.Join(fields[1:], " "))
	} else {
		msg += `. Write column op value, such as "value > 1000"`
	}
	return condSyntaxError{msg}
}

// orError explains that conditions all hold. parts are the condition's
// conjuncts and branches the "or" of the one at i. An "or" of
// equalities on one column is an in list; an "or" whose later branches
// are bare words joins values, so the fix is that list or the value in
// quotes; any other "or" is one call per branch, each with the other
// conjuncts.
func orError(parts []string, i int, branches []string, front filterFront) error {
	var others []string
	for j, p := range parts {
		if j != i {
			inner, _ := outerParens(p)
			others = append(others, inner)
		}
	}
	with := func(c string) []string { return append([]string{c}, others...) }
	// One condition is written as itself, several as the parameter
	// takes them.
	spell := func(c string) string {
		if len(others) == 0 {
			return strconv.Quote(c)
		}
		return front.list(with(c)...)
	}
	const lead = "every condition must hold, so \"or\" cannot join them; "
	var col string
	var vals []string
	same, bare := true, len(branches) > 1
	for k, b := range branches {
		pos, sp, ok := findOperator(b)
		if k > 0 && ok {
			bare = false
		}
		if k > 0 && !ok {
			vals = append(vals, unquote(b))
			continue
		}
		c := strings.TrimSpace(b[:max(pos, 0)])
		if !ok || sp.op == nil || sp.op.name != "=" || col != "" && !strings.EqualFold(c, col) {
			same, bare = false, false
			break
		}
		col = c
		vals = append(vals, unquote(strings.TrimSpace(b[pos+len(sp.text):])))
	}
	switch {
	case bare && col != "":
		in := col + " in " + strings.Join(vals, ", ")
		quoted := col + " = \"" + strings.Join(vals, " or ") + "\""
		return condSyntaxError{fmt.Sprintf("%sfor any of the values write %s; a value that holds \"or\" goes in quotes: %s",
			lead, spell(in), spell(quoted))}
	case same && col != "":
		return condSyntaxError{fmt.Sprintf("%sfor one column write %s", lead, spell(col+" in "+strings.Join(vals, ", ")))}
	}
	alts := make([][]string, len(branches))
	for k, b := range branches {
		inner, _ := outerParens(b)
		alts[k] = with(inner)
	}
	return condSyntaxError{lead + front.alternatives(alts)}
}

// sqlClause refuses the SQL that is no condition, naming what does the
// job.
func sqlClause(text string, front filterFront) error {
	var word string
	if m := sqlWordRe.FindStringSubmatch(text); m != nil {
		word = m[1]
	} else if m := sqlTailRe.FindStringSubmatch(maskQuotes(text)); m != nil {
		word = m[1] + m[2]
	} else {
		return nil
	}
	word = strings.ToUpper(strings.Join(strings.Fields(word), " "))
	var fix string
	switch word {
	case "ORDER BY":
		fix = "sort orders the rows: sort=\"-value\""
		if front.cli {
			fix = "the CLI prints the report's own order; pipe -f csv into sort"
		}
	case "LIMIT", "OFFSET":
		n := "10"
		if m := regexp.MustCompile(`(?i)\b(?:limit|offset)\s+(\d+)`).FindStringSubmatch(text); m != nil {
			n = m[1]
		}
		fix = "limit and offset page the rows: " + strings.ToLower(word) + "=" + n
		if front.cli {
			fix = "pipe the output into head"
		}
	case "GROUP BY", "HAVING":
		fix = "a view groups: spending categories, income types, a period of monthly, quarterly or annual"
	case "SELECT":
		fix = "columns picks the columns: columns=\"+coupon,maturity_date\""
		if front.cli {
			fix = "-C picks the columns: -C +coupon,maturity_date"
		}
	}
	return condSyntaxError{fmt.Sprintf("%s is not a condition; %s", word, fix)}
}

// maskQuotes blanks what quotes enclose, so a pattern over text cannot
// match inside a quoted value.
func maskQuotes(text string) string {
	b := []byte(text)
	q := byte(0)
	for i, c := range b {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				b[i] = 'x'
			}
		case (c == '"' || c == '\'') && (i == 0 || b[i-1] == ' ' || b[i-1] == ','):
			q = c
		}
	}
	return string(b)
}

// functionError refuses a function over a column, naming the condition
// that does its job.
func functionError(fn, col, rest string) error {
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(fn) {
	case "lower", "upper", "trim":
		return condSyntaxError{fmt.Sprintf("= ignores case already; write %q", col+" "+orDefault(rest, "= x"))}
	case "month":
		return condSyntaxError{fmt.Sprintf("a date compares by period, and a month needs its year; write %q with the year", col+" = YYYY-MM")}
	case "year", "date", "quarter":
		pos, sp, ok := findOperator(rest)
		if ok && sp.op != nil {
			return condSyntaxError{fmt.Sprintf("a date compares by period; write %q", col+" "+sp.op.name+" "+strings.TrimSpace(rest[pos+len(sp.text):]))}
		}
		return condSyntaxError{fmt.Sprintf("a date compares by period; write %q", col+" = 2025")}
	case "abs":
		return condSyntaxError{"money compares signed: spending lines are negative, so \"over 500\" is \"value <= -500\"; a size either way takes one call per sign"}
	}
	return condSyntaxError{fmt.Sprintf("functions are not read; compare the column itself: %q", col+" "+rest)}
}

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
	written string
	col     int
	header  string
	op      *opSpec
	kind    colKind
	texts   []string   // normalized values: one for = and contains, the list for in
	nums    []float64  // number and money values
	days    [][2]int64 // date values, each a period [first, last] in epoch days
	canon   string     // the canonical form, for the header
	note    string     // a name resolved to another column, for the header
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
	c := condition{written: r.written, col: i, header: col.header, op: r.op, kind: col.kind(), note: note}
	if t.privacy && redactedClass(col.privacy) {
		return c, t.redacted(col)
	}
	if !slices.Contains(r.op.kinds, c.kind) {
		return c, kindError(r, col, c.kind)
	}
	if r.missing {
		example := map[colKind]string{kindText: "x", kindNumber: "10", kindMoney: "1000", kindDate: "2027"}[c.kind]
		return c, condSyntaxError{fmt.Sprintf("%q needs a value after it, such as %q", r.spelled, col.header+" "+r.op.name+" "+example)}
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
			n, plain, err := parseNumberValue(v, strings.HasSuffix(col.header, "_pct"))
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

// thousandsComma is a comma between digits that groups thousands: three
// digits follow, then no digit.
var thousandsComma = regexp.MustCompile(`\d,\d{3}(?:[^\d]|$)`)

// splitNumberList splits a comma list of numbers, keeping a comma that
// groups thousands inside its number. Such a comma is one where the list
// separates its items with ", ", or where a 0 follows it (1,000 is no
// list of 1 and 000); else it is ambiguous, and 100,200 may be 100 and
// 200 or 100200.
func splitNumberList(s string) (items []string, ambiguous bool) {
	spaced := strings.Contains(s, ", ")
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ',' && i > 0 && thousandsComma.MatchString(s[i-1:min(len(s), i+5)]) && !spaced && s[i+1] != '0' {
			ambiguous = true
		}
	}
	return splitNumbers(s, true), ambiguous
}

// splitNumbers splits s at its commas, but for those that group
// thousands where thousands says so.
func splitNumbers(s string, thousands bool) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ',' || thousands && i > 0 && thousandsComma.MatchString(s[i-1:min(len(s), i+5)]) {
			continue
		}
		out = append(out, unquote(strings.TrimSpace(s[start:i])))
		start = i + 1
	}
	out = append(out, unquote(strings.TrimSpace(s[start:])))
	kept := out[:0]
	for _, item := range out {
		if item != "" {
			kept = append(kept, strings.ReplaceAll(item, ",", ""))
		}
	}
	return kept
}

// numberListError gives both readings of an ambiguous number list: every
// comma a separator, or the thousands commas inside their numbers.
func numberListError(r rawCond, col reportColumn) error {
	split, whole := splitNumbers(r.value, false), splitNumbers(r.value, true)
	other := col.header + " in " + strings.Join(whole, ", ")
	if len(whole) == 1 {
		other = col.header + " = " + whole[0]
	}
	return condSyntaxError{fmt.Sprintf("%q may be %d numbers or a number with thousands commas; write %q or %q",
		r.value, len(split), col.header+" in "+strings.Join(split, ", "), other)}
}

// quoteListItem quotes a list item that holds a comma.
func quoteListItem(v string, list bool) string {
	if list && strings.Contains(v, ",") {
		return strconv.Quote(v)
	}
	return v
}

// redactedClass is a privacy class whose cells the privacy endpoint
// hides, so a condition on it could only probe the hidden value.
func redactedClass(p PrivacyClass) bool {
	return p == PrivacyMoney || p == PrivacyQuantity || p == PrivacyFreeText
}

// columnNamed reports the column a value names exactly, for a
// comparison of two columns.
func (t filterTarget) columnNamed(v string) (reportColumn, bool) {
	want := strings.ToLower(strings.TrimSpace(v))
	for _, c := range t.lookup.cols {
		if strings.ToLower(c.header) == want || c.name == want || t.lookup.stem(c) == want {
			return c, true
		}
	}
	return reportColumn{}, false
}

// unknownColumn names the closest columns, the view's columns of the
// kind the condition reads, and where the column is printed instead.
func (t filterTarget) unknownColumn(r rawCond) error {
	want := strings.ToLower(r.col)
	fit := t.fittingColumns(r)
	// The nearest names, those of the kind the condition reads first.
	var fitting, others []string
	for _, c := range t.lookup.cols {
		if len(fit) > 0 && slices.Contains(fit[1:], c.header) {
			fitting = append(fitting, c.header)
		} else {
			others = append(others, c.header)
		}
	}
	near := t.nearest(want, fitting, 2)
	if len(near) == 0 {
		near = t.nearest(want, others, 2)
	}
	msg := fmt.Sprintf("%s has no column %q", orDefault(t.view, "this result"), r.col)
	if t.view != "" {
		msg = fmt.Sprintf("the %s view has no column %q", t.view, r.col)
	}
	if t.elsewhere != nil {
		if where := t.elsewhere(want); where != "" {
			msg += "; " + where
		}
	}
	if len(near) > 0 {
		msg += fmt.Sprintf(". Nearest: %s", strings.Join(near, ", "))
		fixed := near[0] + " " + r.op.name
		if r.op.arg != argNone {
			fixed += " " + r.value
		}
		msg += fmt.Sprintf(" (%q)", fixed)
	}
	if len(fit) > 0 {
		msg += fmt.Sprintf(". Its %s columns: %s", fit[0], strings.Join(fit[1:], ", "))
	}
	return condSyntaxError{msg}
}

// fittingColumns is the kind word and the view's columns of the kind the
// condition's operator and value read: dates for a date value, numbers
// and money for a number or an ordering, else text.
func (t filterTarget) fittingColumns(r rawCond) []string {
	value := r.value
	if len(r.list) > 0 {
		value = r.list[0]
	}
	want, word := []colKind{kindText}, "text"
	n, _, numErr := parseNumberValue(value, true)
	_, _, dateErr := parseDateValue(value, t.now)
	switch {
	case r.op.arg == argNone || r.missing:
		return nil
	case dateErr == nil && numErr == nil && n >= 1900 && n <= 2100:
		// 2025 is a year or an amount.
		want, word = []colKind{kindDate, kindNumber, kindMoney}, "date, number and money"
	case dateErr == nil && numErr != nil:
		want, word = []colKind{kindDate}, "date"
	case numErr == nil || !slices.Contains(r.op.kinds, kindText):
		want, word = []colKind{kindNumber, kindMoney}, "number and money"
	}
	out := []string{word}
	for _, c := range t.lookup.cols {
		if slices.Contains(want, c.kind()) && !(t.privacy && redactedClass(c.privacy)) {
			out = append(out, c.header)
		}
	}
	if len(out) == 1 {
		return nil
	}
	return out
}

// nearest are up to n of the headers closest to s, by the header or by
// its stem, closest first, of those at least half alike.
func (t filterTarget) nearest(s string, headers []string, n int) []string {
	type scored struct {
		name  string
		score float64
	}
	var all []scored
	for _, h := range headers {
		score := 0.0
		for _, c := range []string{strings.ToLower(h), strings.TrimSuffix(strings.ToLower(h), "_"+strings.ToLower(t.lookup.currency))} {
			score = max(score, similarity(s, c))
			if strings.Contains(c, s) || strings.Contains(s, c) {
				score = max(score, 0.75)
			}
		}
		c := h
		if score >= 0.6 {
			all = append(all, scored{c, score})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
	var out []string
	for _, s := range all[:min(n, len(all))] {
		out = append(out, s.name)
	}
	return out
}

func kindError(r rawCond, col reportColumn, k colKind) error {
	switch {
	case r.op.name == "contains" || r.op.name == "not contains":
		eq := "="
		if r.op.name == "not contains" {
			eq = "!="
		}
		return condSyntaxError{fmt.Sprintf("%s reads text, and %s is %s; write %q", r.op.name, col.header, kindNames[k],
			col.header+" "+eq+" "+r.value)}
	}
	return condSyntaxError{fmt.Sprintf("%q compares %s; %s is text, which takes %s", r.op.name, kindsOf(r.op), col.header, opsForKind(k))}
}

func valueError(r rawCond, col reportColumn, v string, err error) error {
	var fe fixableValue
	if e, ok := err.(fixableValue); ok {
		fe = e
	}
	msg := fmt.Sprintf("%q is not %s, which %s takes", v, kindNames[col.kind()], col.header)
	if fe.why != "" {
		msg = fmt.Sprintf("%q is not %s: %s", v, kindNames[col.kind()], fe.why)
	}
	switch len(fe.fixes) {
	case 0:
		if col.kind() == kindDate {
			msg += "; dates are " + valueDateForms
		} else {
			msg += "; write a plain number, such as 1000 or -42.5"
		}
	case 1:
		msg += fmt.Sprintf("; write %q", col.header+" "+r.op.name+" "+fe.fixes[0])
	default:
		var opts []string
		for _, f := range fe.fixes {
			opts = append(opts, strconv.Quote(col.header+" "+r.op.name+" "+f))
		}
		msg += "; write " + strings.Join(opts, " or ")
	}
	return condSyntaxError{msg}
}

// twoColumns refuses a comparison of two columns, naming the view's
// column that holds the comparison: a difference, else a share, signed
// as the comparison asked. A date compares with a date.
func (t filterTarget) twoColumns(r rawCond, col, other reportColumn) error {
	msg := fmt.Sprintf("a condition compares a column with a constant, and %s is a column", other.header)
	if col.kind() == kindDate {
		return condSyntaxError{fmt.Sprintf("%s; compare with a date, such as %q", msg, col.header+" "+r.op.name+" 2027")}
	}
	var diffs, shares []string
	for _, c := range t.lookup.cols {
		h := strings.ToLower(c.header)
		switch {
		case c.header == col.header || c.header == other.header || c.kind() == kindText || c.kind() == kindDate:
		case strings.Contains(h, "gain") || strings.Contains(h, "change") || strings.Contains(h, "gap") || strings.Contains(h, "delta"):
			diffs = append(diffs, c.header)
		case strings.HasSuffix(h, "_pct"):
			shares = append(shares, c.header)
		}
	}
	if len(diffs) == 0 {
		diffs = shares
	}
	sign := map[string]string{"<": "<", "<=": "<=", ">": ">", ">=": ">=", "=": "=", "!=": "!="}[r.op.name]
	if holders := diffs[:min(2, len(diffs))]; len(holders) > 0 && sign != "" {
		msg += fmt.Sprintf("; %s %s the comparison: %q", orList(holders), plural(len(holders), "holds", "hold"), holders[0]+" "+sign+" 0")
	}
	return condSyntaxError{msg}
}

// redacted refuses a condition on a hidden column, naming a share that
// selects by size instead.
func (t filterTarget) redacted(col reportColumn) error {
	where := "on this endpoint"
	if t.front.cli {
		where = "under -p"
	}
	msg := fmt.Sprintf("%s is redacted %s, so it cannot select rows", col.header, where)
	var shares []string
	for _, c := range t.lookup.cols {
		if strings.HasSuffix(c.header, "_pct") && !redactedClass(c.privacy) {
			shares = append(shares, c.header)
		}
	}
	sort.SliceStable(shares, func(a, b int) bool {
		return strings.Contains(shares[a], "share") && !strings.Contains(shares[b], "share")
	})
	if len(shares) > 0 {
		what := "a percentage"
		if strings.Contains(shares[0], "share") {
			what = "a share"
		}
		msg += fmt.Sprintf("; %s can: %q", what, shares[0]+" > 5")
	}
	return condSyntaxError{msg}
}

// ---- values -------------------------------------------------------------

// fixableValue is a value that does not read, with the plain forms that
// would.
type fixableValue struct {
	why   string
	fixes []string
}

func (e fixableValue) Error() string { return e.why }

var (
	plainNumberRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)$`)
	groupedRe     = regexp.MustCompile(`^[+-]?\d{1,3}(,\d{3})+(\.\d+)?$`)
	scaledRe      = regexp.MustCompile(`(?i)^([+-]?\d+(\.\d+)?)\s*(k|thousand|m|mm|mn|million|bn|b|billion)$`)
	unitRe        = regexp.MustCompile(`(?i)^(?:([a-z]{3})\s+)?([+-]?[\d,]+(\.\d+)?)\s*(%|percent|bps?|[a-z]{3}|dollars?|francs?|euros?)?$`)
)

var numberWords = map[string]int{"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "twenty": 20, "fifty": 50, "hundred": 100, "a hundred": 100, "one hundred": 100,
	"thousand": 1000, "a thousand": 1000, "one thousand": 1000, "million": 1000000, "a million": 1000000, "one million": 1000000}

// parseNumberValue reads a number or money value: a plain number, or
// one with a currency sign or thousands commas, or a percent sign on a
// percentage column, all echoed plain. A number in words, a scale such
// as 1k, or a unit is refused with its plain form.
func parseNumberValue(v string, pct bool) (float64, string, error) {
	s := strings.TrimSpace(strings.ReplaceAll(v, "−", "-"))
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	body = strings.TrimLeft(body, "$€£")
	if neg {
		body = "-" + body
	}
	if strings.HasPrefix(s, "-") && len(body) > 1 && strings.ContainsAny(body[1:2], "$€£") {
		body = "-" + strings.TrimLeft(body[1:], "$€£")
	}
	if pct {
		for _, sign := range []string{"%", "percent", "pct"} {
			if strings.HasSuffix(asciiLower(body), sign) {
				body = strings.TrimSpace(body[:len(body)-len(sign)])
			}
		}
	}
	if groupedRe.MatchString(body) {
		body = strings.ReplaceAll(body, ",", "")
	}
	if plainNumberRe.MatchString(body) {
		f, err := strconv.ParseFloat(body, 64)
		if err == nil {
			return f, strconv.FormatFloat(f, 'f', -1, 64), nil
		}
	}
	plain := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	if m := scaledRe.FindStringSubmatch(s); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		mult := map[string]float64{"k": 1e3, "thousand": 1e3, "m": 1e6, "mm": 1e6, "mn": 1e6, "million": 1e6,
			"b": 1e9, "bn": 1e9, "billion": 1e9}[strings.ToLower(m[3])]
		return 0, "", fixableValue{why: "a scale is not read", fixes: []string{plain(math.Round(f*mult*100) / 100)}}
	}
	if n, ok := numberWords[strings.ToLower(s)]; ok {
		return 0, "", fixableValue{why: "a number in words is not read", fixes: []string{strconv.Itoa(n)}}
	}
	if m := unitRe.FindStringSubmatch(s); m != nil && (m[1] != "" || m[4] != "") {
		num := strings.ReplaceAll(m[2], ",", "")
		if unit := strings.ToLower(m[4]); pct && (unit == "bp" || unit == "bps") {
			f, _ := strconv.ParseFloat(num, 64)
			return 0, "", fixableValue{why: "basis points are not read; the column is in percent", fixes: []string{plain(f / 100)}}
		}
		why := "a unit is not read; amounts are plain numbers in the result's currency"
		if m[4] == "%" || strings.EqualFold(m[4], "percent") {
			why = "a percent sign is read on a _pct column only, whose cells are percentages"
		}
		return 0, "", fixableValue{why: why, fixes: []string{num}}
	}
	return 0, "", fixableValue{}
}

// valueDateForms are the forms a condition's date value takes.
const valueDateForms = "YYYY, YYYY-Qn, YYYY-MM, YYYY-MM-DD, today, yesterday, this month, last month, this year, last year"

var (
	isoDayRe     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[ T]\d{2}:\d{2}(?::\d{2})?Z?)?$`)
	isoMonthRe   = regexp.MustCompile(`^(\d{4})-(\d{2})$`)
	isoYearRe    = regexp.MustCompile(`^(\d{4})$`)
	isoQuarterRe = regexp.MustCompile(`(?i)^(\d{4})-q([1-4])$`)
	relativeRe   = regexp.MustCompile(`(?i)^(today|yesterday|(this|last) (month|year|week)|\d+ (day|week|month|year)s? ago)$`)
	slashDayRe   = regexp.MustCompile(`^(\d{1,2})[/.](\d{1,2})[/.](\d{4})$`)
	ymdSlashRe   = regexp.MustCompile(`^(\d{4})[/.](\d{1,2})(?:[/.](\d{1,2}))?$`)
	quarterRe    = regexp.MustCompile(`(?i)^(?:q([1-4])[\s/-]*(\d{4})|(\d{4})[\s/-]*q([1-4]))$`)
)

// parseDateValue reads a date value as the period it names, in epoch
// days: a year, a quarter, a month or a day, or a relative word. Every
// form a date or a period cell prints reads. A near form (03/15/2027,
// March 2027, Q1 2026) is refused with its ISO form, both readings
// where day and month are ambiguous.
func parseDateValue(v string, now time.Time) (int64, int64, error) {
	s := strings.TrimSpace(v)
	day := func(y, m, d int) int64 { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Unix() / 86400 }
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case isoDayRe.MatchString(s):
		m := isoDayRe.FindStringSubmatch(s)
		y, mo, d := atoi(m[1]), atoi(m[2]), atoi(m[3])
		if t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC); int(t.Month()) != mo || t.Day() != d {
			return 0, 0, fixableValue{why: "no such day"}
		}
		return day(y, mo, d), day(y, mo, d), nil
	case isoMonthRe.MatchString(s):
		m := isoMonthRe.FindStringSubmatch(s)
		y, mo := atoi(m[1]), atoi(m[2])
		if mo < 1 || mo > 12 {
			return 0, 0, fixableValue{why: "no such month"}
		}
		return day(y, mo, 1), day(y, mo+1, 1) - 1, nil
	case isoYearRe.MatchString(s):
		y := atoi(s)
		return day(y, 1, 1), day(y+1, 1, 1) - 1, nil
	case isoQuarterRe.MatchString(s):
		m := isoQuarterRe.FindStringSubmatch(s)
		y, q := atoi(m[1]), atoi(m[2])
		return day(y, 3*q-2, 1), day(y, 3*q+1, 1) - 1, nil
	case relativeRe.MatchString(s):
		lo, err := parseDate(s, now, false)
		if err != nil {
			return 0, 0, fixableValue{}
		}
		hi, err := parseDate(s, now, true)
		if err != nil {
			return 0, 0, fixableValue{}
		}
		return lo.Unix() / 86400, hi.Unix() / 86400, nil
	}
	pad := func(n int) string { return fmt.Sprintf("%02d", n) }
	if m := slashDayRe.FindStringSubmatch(s); m != nil {
		a, b, y := atoi(m[1]), atoi(m[2]), m[3]
		valid := func(mo, d int) bool {
			t := time.Date(atoi(y), time.Month(mo), d, 0, 0, 0, 0, time.UTC)
			return mo >= 1 && mo <= 12 && int(t.Month()) == mo && t.Day() == d
		}
		switch {
		case !valid(a, b) && !valid(b, a):
			return 0, 0, fixableValue{why: "no such day"}
		case !valid(b, a):
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(a) + "-" + pad(b)}}
		case !valid(a, b):
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(b) + "-" + pad(a)}}
		case a > 12 && b <= 12:
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(b) + "-" + pad(a)}}
		case b > 12 && a <= 12:
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(a) + "-" + pad(b)}}
		case a <= 12 && b <= 12 && a != b:
			return 0, 0, fixableValue{why: "dates are ISO, and day and month are ambiguous here",
				fixes: []string{y + "-" + pad(a) + "-" + pad(b), y + "-" + pad(b) + "-" + pad(a)}}
		case a == b:
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(a) + "-" + pad(b)}}
		}
	}
	if m := ymdSlashRe.FindStringSubmatch(s); m != nil {
		fix := m[1] + "-" + pad(atoi(m[2]))
		if m[3] != "" {
			fix += "-" + pad(atoi(m[3]))
		}
		return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{fix}}
	}
	if m := quarterRe.FindStringSubmatch(s); m != nil {
		y, q := m[2], m[1]
		if y == "" {
			y, q = m[3], m[4]
		}
		return 0, 0, fixableValue{why: "a quarter is written YYYY-Qn", fixes: []string{y + "-Q" + q}}
	}
	// A date in words ("March 2027", "15 March 2027"): its ISO form.
	if strings.IndexFunc(s, unicode.IsLetter) >= 0 && strings.IndexFunc(s, unicode.IsDigit) >= 0 {
		if dt, err := getDateParser().Parse(newParseConfig(now), s); err == nil && !dt.IsZero() {
			t := dt.Time.UTC()
			fix := t.Format("2006-01-02")
			if !strings.ContainsAny(strings.TrimSpace(strings.TrimRight(strings.Fields(s)[0], ",")), "0123456789") &&
				len(strings.Fields(s)) == 2 {
				fix = t.Format("2006-01")
			}
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{fix}}
		}
	}
	return 0, 0, fixableValue{}
}

func epochDayString(d int64) string { return formatDate(d * 86400) }

// periodNote is the header's reading of a date value under op: the
// days it keeps.
func periodNote(op string, lo, hi int64) string {
	switch op {
	case "<":
		return "before " + epochDayString(lo)
	case "<=":
		return "to " + epochDayString(hi)
	case ">":
		return "after " + epochDayString(hi)
	case ">=":
		return "from " + epochDayString(lo)
	}
	if lo == hi {
		return epochDayString(lo)
	}
	return epochDayString(lo) + " → " + epochDayString(hi)
}

// cellDay is the first day a date or period cell names, in epoch days;
// false for a blank or a cell that is no date (total).
func cellDay(cell string) (int64, bool) {
	cell = strings.TrimSpace(cell)
	if len(cell) > 10 && isoDayRe.MatchString(cell) {
		cell = cell[:10]
	}
	switch {
	case isoDayRe.MatchString(cell), isoMonthRe.MatchString(cell), isoYearRe.MatchString(cell), isoQuarterRe.MatchString(cell):
		lo, _, err := parseDateValue(cell, time.Time{})
		return lo, err == nil
	}
	return 0, false
}

// missingCell is a cell with no value.
func missingCell(cell string) bool {
	cell = strings.TrimSpace(cell)
	return cell == "" || cell == "n/a"
}

// ---- keeping rows -------------------------------------------------------

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
			ks = append(ks, strings.TrimPrefix(kindNames[k], "a "))
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

// reportFamilies are the report tools and commands by name, in describe's
// order: where a column a view lacks is printed instead.
var reportFamilies = []struct {
	name string
	fam  *family
}{
	{"holdings", holdingsFamily}, {"returns", returnsFamily}, {"gains", gainsFamily}, {"exposure", exposureFamily},
	{"yield", yieldFamily}, {"transactions", transactionsFamily}, {"spending", spendingFamily}, {"income", incomeFamily},
	{"cashflow", cashflowFamily},
}

// columnElsewhere names where col is printed when view of tool lacks it:
// the tool's other views, else the first views of other tools. cli
// spells a view as the command that prints it. "" when nothing prints
// it.
func columnElsewhere(cfg *config.Config, tool, view, col string, cli bool) string {
	if col == "period" && (tool == "exposure" || tool == "yield") {
		if cli {
			return "period is a series' column: give --period quarterly FROM TO"
		}
		return "period is a series' column: give period=quarterly with from and to"
	}
	prints := func(fam *family, v string) bool {
		req := defaultRequest(v, cfg.DefaultCurrency)
		rep := fam.build(cfg, req, &toolArgs{values: map[string]any{}})
		l := fam.lookup(rep, req)
		for _, c := range rep.columns {
			if c.name == col || strings.ToLower(c.header) == col || l.stem(c) == col {
				return true
			}
		}
		return false
	}
	spell := func(t, v string) string {
		switch {
		case cli:
			return strings.TrimSpace("wealthdb " + t + " " + v)
		case t == tool:
			return "view=" + v
		case v == "":
			return "the " + t + " tool"
		}
		return "the " + t + " tool's " + v + " view"
	}
	var same, other []string
	for _, rf := range reportFamilies {
		for _, v := range rf.fam.viewsOrOne() {
			if rf.name == tool && v == view || !prints(rf.fam, v) {
				continue
			}
			if rf.name == tool {
				same = append(same, spell(rf.name, v))
			} else if len(other) < 2 {
				other = append(other, spell(rf.name, v))
			}
		}
	}
	switch {
	case len(same) > 0:
		return col + " is printed by " + orList(same)
	case len(other) > 0:
		return col + " is printed by " + orList(other)
	}
	return ""
}
