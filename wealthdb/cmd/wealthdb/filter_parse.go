package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

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
	limitRe   = regexp.MustCompile(`(?i)\b(?:limit|offset)\s+(\d+)`)
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
	if text == "" {
		return nil, condSyntaxError{"an empty condition; write column op value, such as \"value > 1000\""}
	}
	if pos, q := unclosedQuote(text); pos >= 0 {
		return nil, unclosedQuoteError(text, pos, q)
	}
	if strings.HasPrefix(asciiLower(text), "where ") {
		text = strings.TrimSpace(text[len("where "):])
	}
	if trimmed := strings.TrimRight(text, "; "); trimmed != text {
		text = trimmed // a statement's closing semicolon
	}
	if err := sqlClause(text, front); err != nil {
		return nil, err
	}
	if inner, ok := outerParens(text); ok {
		text = inner
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
		out = append(out, conds...)
	}
	return out, nil
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
	if inner, ok := outerParens(text); ok {
		text = inner
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
	if identRe.FindString(col) != col {
		// "market value > 5": a name with spaces means the column with
		// underscores.
		joined := strings.Join(strings.Fields(strings.ReplaceAll(col, "-", "_")), "_")
		if identRe.FindString(joined) != joined {
			return nil, condSyntaxError{fmt.Sprintf("%q is not a column name; a column is one word, such as value_USD or maturity_date", col)}
		}
		col = joined
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
	if sp.text == "like" || sp.text == "not like" {
		value = strings.Trim(unquote(value), "%")
	}
	if op.arg == argOne && value != "" && unquote(value) == "" {
		value = "" // a quoted empty value: the empty cell
	}
	switch {
	case value != "" || op.arg != argOne:
	case op.name == "=":
		// "col =" with nothing after it: the empty cell.
		return []rawCond{{written: written, col: col, op: opByName("is empty")}}, nil
	case op.name == "!=":
		return []rawCond{{written: written, col: col, op: opByName("is not empty")}}, nil
	default:
		return []rawCond{{written: written, col: col, op: op, missing: true, spelled: sp.text}}, nil
	}
	switch op.arg {
	case argNone:
		if value != "" {
			return nil, condSyntaxError{fmt.Sprintf("%q takes no value; write %q", op.name, col+" "+op.name)}
		}
		return []rawCond{{written: written, col: col, op: op}}, nil
	case argList:
		if value == "" {
			return nil, condSyntaxError{fmt.Sprintf("%q needs a comma list after it, such as %q", op.name, col+" "+op.name+" a, b")}
		}
		if inner, ok := outerParens(value); ok {
			value = inner
		} else if strings.HasPrefix(value, "(") != strings.HasSuffix(value, ")") {
			return nil, condSyntaxError{fmt.Sprintf("a parenthesis around the list never closes; write %q",
				col+" "+op.name+" "+strings.Trim(value, "() "))}
		}
		items := splitList(value)
		if len(items) == 0 {
			return nil, condSyntaxError{fmt.Sprintf("%q needs a comma list after it, such as %q", op.name, col+" "+op.name+" a, b")}
		}
		return []rawCond{{written: written, col: col, op: op, value: value, list: items}}, nil
	}
	return []rawCond{{written: written, col: col, op: op, value: unquote(value)}}, nil
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
