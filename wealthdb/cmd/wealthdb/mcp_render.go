package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// Result formats a tool can return.
const (
	formatTable = "table"
	formatCSV   = "csv"
	formatJSON  = "json"
)

var resultFormats = []string{formatTable, formatCSV, formatJSON}

// resultHeader is the first line of every result. It states what ran,
// resolved: the view, the window or as-of date as ISO dates (never the
// strings the model sent), the currency, the options in force, the row
// count, the sort, and every choice the server made for the model —
// the view a filter picked, the column a misspelt name resolved to.
// A small model checks its answer against this line.
type resultHeader struct {
	parts []string
}

func (h *resultHeader) add(format string, args ...any) {
	h.parts = append(h.parts, fmt.Sprintf(format, args...))
}

func (h resultHeader) String() string {
	return strings.Join(h.parts, " · ")
}

// page is a filtered, sorted result and the part of it one call
// returns: the rows from offset that fit the character budget, at most
// limit of them where a limit is given.
type page struct {
	idx    []int // every row of the result, as indices into the result set
	offset int
	limit  int // most rows to return; 0 for no row bound
	budget int // most characters the text may take (max_chars)
	// narrow is the advice for a result too long for one page: the
	// parameters that cut it down for this tool.
	narrow string
	// empty explains a result with no rows; it is set exactly then.
	empty string
	// totals are the total line's sums over every row of the result.
	totals []columnTotal
}

// totalRule says how a view's money adds up over its rows.
type totalRule int

const (
	noTotal      totalRule = iota
	flowTotal              // lines and buckets over a window: any rows add up
	balanceTotal           // values at a date: rows add up within one period
)

// columnTotal is one money column's sum over a result's rows.
type columnTotal struct {
	header, sum string
}

// totals sums each shown money column in the output currency over the
// rows at idx, under rule: a balance view adds up only within one
// period, so a series of more than one has no total.
func totals(rs *resultSet, idx, cols []int, currency string, rule totalRule) []columnTotal {
	if rule == noTotal || len(idx) == 0 || currency == "" {
		return nil
	}
	if rule == balanceTotal {
		for i, c := range rs.rep.columns {
			if c.name == "period" && len(distinctAt(rs.cells, idx, i)) > 1 {
				return nil
			}
		}
	}
	var out []columnTotal
	for _, col := range cols {
		c := rs.rep.columns[col]
		if c.privacy != PrivacyMoney || !strings.HasSuffix(c.header, "_"+currency) {
			continue
		}
		var cents int64
		for _, i := range idx {
			if f, err := strconv.ParseFloat(strings.TrimSpace(rs.cells[i][col]), 64); err == nil {
				cents += int64(math.Round(f * 100))
			}
		}
		out = append(out, columnTotal{header: c.header, sum: formatCentsInt(cents)})
	}
	return out
}

// formatCentsInt prints cents as a decimal with two places.
func formatCentsInt(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// distinctAt is the distinct cells of column col over the rows at idx.
func distinctAt(cells [][]string, idx []int, col int) map[string]bool {
	seen := map[string]bool{}
	for _, i := range idx {
		seen[cells[i][col]] = true
	}
	return seen
}

// totalLine is the total line of a page with totals: the rows it sums
// and each column's sum.
func (p page) totalLine() string {
	if len(p.totals) == 0 {
		return ""
	}
	parts := make([]string, len(p.totals))
	for i, t := range p.totals {
		parts[i] = t.header + " " + t.sum
	}
	return fmt.Sprintf("total of %s %s: %s\n", thousands(len(p.idx)), plural(len(p.idx), "row", "rows"), strings.Join(parts, ", "))
}

// cutInfo is what the paging line says of a page that does not hold the
// whole result: which rows it holds, how many characters it and the
// whole result take, and what makes the rest fit.
type cutInfo struct {
	offset, end, total int
	chars, totalChars  int
	narrow             string
	widest             []columnShare
}

// columnShare is one column's part of the characters of every cell.
type columnShare struct {
	Column   string `json:"column"`
	SharePct int    `json:"share_pct"`
}

// line is the paging line of a cut page.
func (c cutInfo) line() string {
	if c.end == c.offset {
		return fmt.Sprintf("no rows at offset=%d: the result has %s rows", c.offset, thousands(c.total))
	}
	line := fmt.Sprintf("showing rows %s–%s of %s (%s of %s chars, about %s a row)",
		thousands(c.offset+1), thousands(c.end), thousands(c.total),
		thousands(c.chars), thousands(c.totalChars), thousands(c.perRow()))
	if c.end < c.total {
		line += fmt.Sprintf("; next: offset=%d; all rows: max_chars=%d; narrow with %s", c.end, c.totalChars, c.narrow)
	}
	if len(c.widest) > 0 {
		parts := make([]string, len(c.widest))
		for i, w := range c.widest {
			parts[i] = fmt.Sprintf("%s %d%%", w.Column, w.SharePct)
		}
		line += "; widest columns: " + strings.Join(parts, ", ")
	}
	return line
}

// perRow is the whole result's average characters per row.
func (c cutInfo) perRow() int {
	if c.total == 0 {
		return 0
	}
	return (c.totalChars + c.total/2) / c.total
}

// toolOutput is a rendered result: the text every client shows the
// model, and for json the same object as structured content.
type toolOutput struct {
	text       string
	structured any
}

// jsonResult is the json format's object. rows follow the CLI's JSON
// contract: every value a string, money a decimal string, an empty
// cell an absent key — and on the privacy endpoint the money and
// quantity keys absent altogether, as -f json -p drops them. A page
// that does not hold the whole result carries the paging line's
// figures as fields.
type jsonResult struct {
	Tool       string              `json:"tool"`
	View       string              `json:"view,omitempty"`
	From       string              `json:"from,omitempty"`
	To         string              `json:"to,omitempty"`
	AsOf       string              `json:"as_of,omitempty"`
	Currency   string              `json:"currency,omitempty"`
	Header     string              `json:"header"`
	Columns    []string            `json:"columns"`
	Rows       []map[string]string `json:"rows"`
	RowCount   int                 `json:"row_count"`
	TotalRows  int                 `json:"total_rows"`
	Offset     int                 `json:"offset"`
	NextOffset *int                `json:"next_offset,omitempty"`
	// Chars is this text's length and TotalChars the whole result's,
	// which a max_chars of that size returns in one call.
	Chars         int           `json:"chars,omitempty"`
	TotalChars    int           `json:"total_chars,omitempty"`
	CharsPerRow   int           `json:"chars_per_row,omitempty"`
	Narrow        string        `json:"narrow,omitempty"`
	WidestColumns []columnShare `json:"widest_columns,omitempty"`
	// Totals are the total line's sums over every matching row.
	Totals map[string]string `json:"totals,omitempty"`
	// Note explains an empty result: why it may be empty, and the
	// values the filters' columns take.
	Note string `json:"note,omitempty"`
}

// resultMeta is what the json object says about the call beside the
// rows; the other formats carry it in the header line.
type resultMeta struct {
	tool, view, from, to, asOf, currency string
}

// layout is a result rendered in one format, ready to be cut: compose
// writes the rows [from, to) with the paging line of c (none for nil).
type layout struct {
	compose func(from, to int, c *cutInfo) string
	rowLen  []int // each row's characters, as compose writes it
	table   output.Table
	json    func(from, to int, c *cutInfo) jsonResult
}

// render writes a page of a result set in the requested format. The
// cells come from rowsToTable for the result's rows and columns, so a
// table or csv cell is the string the CLI prints. The whole result is
// rendered first, so its size is exact; the page is then the longest
// run of rows from offset whose text fits the budget, and at least one
// row.
func render(rs *resultSet, p page, cols []int, format string, head resultHeader, meta resultMeta) toolOutput {
	l := newLayout(rs, p, cols, format, head, meta)
	total := len(p.idx)
	whole := l.compose(0, total, nil)
	if p.empty != "" || p.offset == 0 && (p.limit == 0 || p.limit >= total) && runes(whole) <= p.budget {
		return l.output(whole, 0, total, nil)
	}
	c := &cutInfo{offset: p.offset, end: p.offset, total: total, totalChars: runes(whole), narrow: p.narrow,
		widest: widestColumns(l.table)}
	if p.offset >= total {
		// Past the end: no rows, and never the whole result.
		return l.output(l.compose(total, total, c), total, total, c)
	}
	c.end = l.fit(c, p)
	return l.output(settle(l, c), c.offset, c.end, c)
}

// fit is the end of the longest run of rows from c.offset, at most
// p.limit, whose text with its paging line fits p.budget: the prefix
// sum of the rows' lengths guesses it, and the composed text checks it.
// The run holds at least one row.
func (l layout) fit(c *cutInfo, p page) int {
	limitEnd := len(l.rowLen)
	if p.limit > 0 {
		limitEnd = min(limitEnd, c.offset+p.limit)
	}
	probe := *c
	probe.end = c.offset + 1
	room := p.budget - runes(l.compose(c.offset, c.offset, &probe))
	end := c.offset
	for end < limitEnd && room >= l.rowLen[end] {
		room -= l.rowLen[end]
		end++
	}
	end = max(end, c.offset+1)
	for end > c.offset+1 {
		probe.end = end
		if runes(settle(l, &probe)) <= p.budget {
			break
		}
		end--
	}
	return end
}

// settle composes c's page with c.chars set to the text's own length:
// the paging line states it, so it is recomposed until the count holds.
func settle(l layout, c *cutInfo) string {
	text := l.compose(c.offset, c.end, c)
	for range 4 {
		n := runes(text)
		if n == c.chars {
			break
		}
		c.chars = n
		text = l.compose(c.offset, c.end, c)
	}
	return text
}

// output is the tool output of the text for rows [from, to).
func (l layout) output(text string, from, to int, c *cutInfo) toolOutput {
	out := toolOutput{text: text}
	if l.json != nil {
		out.structured = l.json(from, to, c)
	}
	return out
}

func newLayout(rs *resultSet, p page, cols []int, format string, head resultHeader, meta resultMeta) layout {
	switch format {
	case formatJSON:
		return jsonLayout(rs, p, cols, head, meta)
	case formatCSV:
		t := rs.rows.table(p.idx, cols, rs.privacy, output.FormatCSV, rs.names)
		rows := make([]string, len(t.Rows))
		for i, row := range t.Rows {
			var b bytes.Buffer
			_ = output.WriteCSVPlain(&b, output.Table{Columns: t.Columns, Rows: [][]string{row}})
			rows[i] = b.String()
		}
		var cols bytes.Buffer
		_ = output.WriteCSV(&cols, output.Table{Columns: t.Columns})
		return textLayout(t, head.String()+"\n", cols.String(), true, rows, p)
	default:
		t := rs.rows.table(p.idx, cols, rs.privacy, output.FormatTable, rs.names)
		rows := make([]string, len(t.Rows))
		for i, row := range t.Rows {
			rows[i] = markdownRow(row)
		}
		return textLayout(t, head.String()+"\n", markdownHead(t), false, rows, p)
	}
}

// textLayout is table or csv: the header line, the column line (on a
// page with no rows only where always says so), the rows, then the
// empty note or the paging line.
func textLayout(t output.Table, head, cols string, always bool, rows []string, p page) layout {
	l := layout{table: t, rowLen: make([]int, len(rows))}
	for i, r := range rows {
		l.rowLen[i] = runes(r)
	}
	l.compose = func(from, to int, c *cutInfo) string {
		var b strings.Builder
		b.WriteString(head)
		if always || from < to {
			b.WriteString(cols)
		}
		for _, r := range rows[from:to] {
			b.WriteString(r)
		}
		b.WriteString(p.totalLine())
		b.WriteString(p.empty)
		if c != nil {
			b.WriteString(c.line())
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return l
}

// jsonLayout is the json object, rows as objects; a row's length is its
// object's and the comma after it.
func jsonLayout(rs *resultSet, p page, cols []int, head resultHeader, meta resultMeta) layout {
	t := rs.rows.table(p.idx, cols, rs.privacy, output.FormatJSON, rs.names)
	objs := make([]map[string]string, len(t.Rows))
	l := layout{table: t, rowLen: make([]int, len(t.Rows))}
	for i, row := range t.Rows {
		obj := make(map[string]string, len(t.Columns))
		for j, name := range t.Columns {
			if row[j] != "" {
				obj[name] = row[j]
			}
		}
		objs[i] = obj
		l.rowLen[i] = runes(encodeJSON(obj)) + 1
	}
	l.json = func(from, to int, c *cutInfo) jsonResult {
		res := jsonResult{
			Tool: meta.tool, View: meta.view, From: meta.from, To: meta.to, AsOf: meta.asOf,
			Currency: meta.currency, Header: head.String(), Columns: t.Columns,
			Rows: objs[from:to], RowCount: to - from, TotalRows: len(objs), Offset: from, Note: p.empty,
		}
		if res.Rows == nil {
			res.Rows = []map[string]string{}
		}
		if len(p.totals) > 0 {
			res.Totals = map[string]string{}
			for _, t := range p.totals {
				res.Totals[t.header] = t.sum
			}
		}
		if c != nil {
			res.Offset = c.offset
			if to < len(objs) && to > from {
				next := to
				res.NextOffset = &next
			}
			res.Chars, res.TotalChars, res.CharsPerRow = c.chars, c.totalChars, c.perRow()
			res.WidestColumns = c.widest
			if to < len(objs) {
				res.Narrow = c.narrow
			}
		}
		return res
	}
	l.compose = func(from, to int, c *cutInfo) string {
		return encodeJSON(l.json(from, to, c))
	}
	return l
}

// encodeJSON is v as one line, unescaped: a merchant "A&B" reads as
// itself, not A\u0026B.
func encodeJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "(the result could not be encoded as json: " + err.Error() + ")"
	}
	return strings.TrimRight(b.String(), "\n")
}

// widestColumns are the columns that take the largest shares of the
// cells' characters, widest first: the three widest, of those that take
// a tenth or more. They say which columns= cut shortens a page most.
func widestColumns(t output.Table) []columnShare {
	sums := make([]int, len(t.Columns))
	all := 0
	for _, row := range t.Rows {
		for i, cell := range row {
			n := runes(cell)
			sums[i] += n
			all += n
		}
	}
	if all == 0 || len(t.Columns) < 2 {
		return nil
	}
	order := make([]int, len(t.Columns))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sums[order[a]] > sums[order[b]] })
	var out []columnShare
	for _, i := range order[:min(3, len(order))] {
		if pct := sums[i] * 100 / all; pct >= 10 {
			out = append(out, columnShare{Column: t.Columns[i], SharePct: pct})
		}
	}
	return out
}

// runes is the length the budget counts: code points.
func runes(s string) int { return utf8.RuneCountInString(s) }

// markdownHead is a markdown table's column and rule lines: the CLI's
// headers, numeric columns right-aligned, no padding — padding is
// tokens a model pays for and does not read.
func markdownHead(t output.Table) string {
	var b strings.Builder
	b.WriteString("|")
	for _, c := range t.Columns {
		b.WriteString(" " + markdownCell(c) + " |")
	}
	b.WriteString("\n|")
	for i := range t.Columns {
		if i < len(t.Aligns) && t.Aligns[i] == output.AlignRight {
			b.WriteString("---:|")
		} else {
			b.WriteString("---|")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// writeMarkdownTable writes t as a markdown table.
func writeMarkdownTable(b *strings.Builder, t output.Table) {
	b.WriteString(markdownHead(t))
	for _, row := range t.Rows {
		b.WriteString(markdownRow(row))
	}
}

// markdownRow is one row of a markdown table, with its line end.
func markdownRow(row []string) string {
	var b strings.Builder
	b.WriteString("|")
	for _, cell := range row {
		b.WriteString(" " + markdownCell(cell) + " |")
	}
	b.WriteString("\n")
	return b.String()
}

// markdownCell escapes what would break a table row: a pipe, and a
// line break inside a statement narrative.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, `|`, `\|`)
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}
