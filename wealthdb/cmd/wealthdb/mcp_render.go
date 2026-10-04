package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

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

// page is the slice of a filtered, sorted result one call returns.
type page struct {
	idx    []int // this page's rows, as indices into the result set
	total  int   // rows after filtering, before paging
	offset int
	limit  int  // the limit in force; 0 means every row
	capped bool // the limit asked for was lowered to the server's ceiling
	// narrow is the advice for a result too long to page through:
	// the parameters that cut it down for this tool.
	narrow string
	// empty explains a result with no rows; it is set exactly then.
	empty string
}

// cut takes the page at offset from the filtered rows. The page is
// never nil: to the report seam a nil index list means every row, and
// an offset past the end must render none.
func cut(idx []int, offset, limit int) []int {
	if offset >= len(idx) {
		return []int{}
	}
	end := len(idx)
	if limit > 0 && limit < end-offset {
		end = offset + limit
	}
	return idx[offset:end]
}

// pagingLine is the last line of a result that does not hold every
// row: which rows these are and how to get the rest. Empty when the
// page is the whole result.
func (p page) pagingLine() string {
	end := p.offset + len(p.idx)
	switch {
	case p.total == 0 || (p.offset == 0 && end >= p.total):
		return ""
	case len(p.idx) == 0:
		return fmt.Sprintf("no rows at offset=%d: the result has %s rows", p.offset, thousands(p.total))
	}
	line := fmt.Sprintf("showing rows %s–%s of %s", thousands(p.offset+1), thousands(end), thousands(p.total))
	if end < p.total {
		next := p.total - end
		if p.limit > 0 && p.limit < next {
			next = p.limit
		}
		line += fmt.Sprintf(" — pass offset=%d for the next %s, or narrow with %s", end, thousands(next), p.narrow)
	}
	if p.capped {
		line += fmt.Sprintf(" (this server returns at most %s rows per call)", thousands(p.limit))
	}
	return line
}

// trailer is what follows the rows in table and csv: the empty note or
// the paging line.
func (p page) trailer() string {
	if p.empty != "" {
		return p.empty + "\n"
	}
	if line := p.pagingLine(); line != "" {
		return line + "\n"
	}
	return ""
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
// quantity keys absent altogether, as -f json -p drops them.
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
	// Note explains an empty result: why it may be empty, and the
	// values the filters' columns take.
	Note string `json:"note,omitempty"`
}

// resultMeta is what the json object says about the call beside the
// rows; the other formats carry it in the header line.
type resultMeta struct {
	tool, view, from, to, asOf, currency string
}

// render writes one page of a result set in the requested format. The
// cells come from rowsToTable for the page's rows and columns, so a
// table or csv cell is the string the CLI prints.
func render(rs *resultSet, p page, cols []int, format string, head resultHeader, meta resultMeta) toolOutput {
	switch format {
	case formatJSON:
		t := rs.rows.table(p.idx, cols, rs.privacy, output.FormatJSON)
		res := jsonResult{
			Tool: meta.tool, View: meta.view, From: meta.from, To: meta.to, AsOf: meta.asOf,
			Currency: meta.currency, Header: head.String(), Columns: t.Columns,
			Rows: make([]map[string]string, 0, len(t.Rows)), RowCount: len(t.Rows),
			TotalRows: p.total, Offset: p.offset, Note: p.empty,
		}
		for _, row := range t.Rows {
			obj := make(map[string]string, len(t.Columns))
			for i, name := range t.Columns {
				if row[i] != "" {
					obj[name] = row[i]
				}
			}
			res.Rows = append(res.Rows, obj)
		}
		if end := p.offset + len(p.idx); end < p.total && len(p.idx) > 0 {
			res.NextOffset = &end
		}
		// Unescaped: a merchant "A&B" reads as itself, not A\u0026B.
		var text bytes.Buffer
		enc := json.NewEncoder(&text)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			return toolOutput{text: head.String() + "\n(the result could not be encoded as json: " + err.Error() + ")"}
		}
		return toolOutput{text: strings.TrimRight(text.String(), "\n"), structured: res}
	case formatCSV:
		var b bytes.Buffer
		b.WriteString(head.String() + "\n")
		_ = output.WriteCSV(&b, rs.rows.table(p.idx, cols, rs.privacy, output.FormatCSV))
		b.WriteString(p.trailer())
		return toolOutput{text: strings.TrimRight(b.String(), "\n")}
	default:
		var b strings.Builder
		b.WriteString(head.String() + "\n")
		if len(p.idx) > 0 {
			writeMarkdownTable(&b, rs.rows.table(p.idx, cols, rs.privacy, output.FormatTable))
		}
		b.WriteString(p.trailer())
		return toolOutput{text: strings.TrimRight(b.String(), "\n")}
	}
}

// writeMarkdownTable writes t as a markdown table: the CLI's headers
// and cells, numeric columns right-aligned, no padding — padding is
// tokens a model pays for and does not read.
func writeMarkdownTable(b *strings.Builder, t output.Table) {
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
	for _, row := range t.Rows {
		b.WriteString("|")
		for _, cell := range row {
			b.WriteString(" " + markdownCell(cell) + " |")
		}
		b.WriteString("\n")
	}
}

// markdownCell escapes what would break a table row: a pipe, and a
// line break inside a statement narrative.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, `|`, `\|`)
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}
