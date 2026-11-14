package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

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
	prefix := ""
	if f.cli {
		prefix = "-F "
	}
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = prefix + strconv.Quote(c)
	}
	if f.cli {
		return strings.Join(parts, " ")
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
	same, bare := true, true
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
		if m := limitRe.FindStringSubmatch(text); m != nil {
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
	msg := fmt.Sprintf("this result has no column %q", r.col)
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
		if slices.Contains(want, c.filterKind()) && !(t.privacy && redactedClass(c.privacy)) {
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
		if score >= 0.6 {
			all = append(all, scored{h, score})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
	var out []string
	for _, a := range all[:min(n, len(all))] {
		out = append(out, a.name)
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
		return condSyntaxError{fmt.Sprintf("%s reads text, and %s is %s; write %q", r.op.name, col.header, kindWords[k].one,
			col.header+" "+eq+" "+r.value)}
	}
	return condSyntaxError{fmt.Sprintf("%q compares %s; %s is text, which takes %s", r.op.name, kindsOf(r.op), col.header, opsForKind(k))}
}

func valueError(r rawCond, col reportColumn, v string, err error) error {
	var fe fixableValue
	if e, ok := err.(fixableValue); ok {
		fe = e
	}
	msg := fmt.Sprintf("%q is not %s, which %s takes", v, kindWords[col.filterKind()].one, col.header)
	if fe.why != "" {
		msg = fmt.Sprintf("%q is not %s: %s", v, kindWords[col.filterKind()].one, fe.why)
	}
	switch len(fe.fixes) {
	case 0:
		if col.filterKind() == kindDate {
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
	if col.filterKind() == kindDate {
		return condSyntaxError{fmt.Sprintf("%s; compare with a date, such as %q", msg, col.header+" "+r.op.name+" 2027")}
	}
	var diffs, shares []string
	for _, c := range t.lookup.cols {
		h := strings.ToLower(c.header)
		switch {
		case c.header == col.header || c.header == other.header || !c.numeric():
		case strings.Contains(h, "gain") || strings.Contains(h, "change") || strings.Contains(h, "gap") || strings.Contains(h, "delta"):
			diffs = append(diffs, c.header)
		case c.kind == PercentColumn:
			shares = append(shares, c.header)
		}
	}
	if len(diffs) == 0 {
		diffs = shares
	}
	compares := slices.Contains([]string{"<", "<=", ">", ">=", "=", "!="}, r.op.name)
	if holders := diffs[:min(2, len(diffs))]; len(holders) > 0 && compares {
		msg += fmt.Sprintf("; %s %s the comparison: %q", orList(holders), plural(len(holders), "holds", "hold"), holders[0]+" "+r.op.name+" 0")
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
		if c.kind == PercentColumn && !redactedClass(c.privacy) {
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
	prints := func(fam *family, v string) bool { return fam.defaultLookup(cfg, v, false).exactOrStem(col) >= 0 }
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
