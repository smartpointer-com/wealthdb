package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// renderLines renders the merchant and value columns of the spend lines
// in format, with a page of p and the header "spending transactions".
func renderLines(t *testing.T, rs *resultSet, p page, format string) toolOutput {
	t.Helper()
	head := resultHeader{}
	head.add("spending transactions")
	if p.idx == nil {
		p.idx = rs.filter(nil, "")
	}
	if p.budget == 0 {
		p.budget = 16000
	}
	return render(rs, p, []int{colIndex(t, rs, "merchant"), colIndex(t, rs, "value")}, format, head, resultMeta{tool: "spending"})
}

// TestRenderTable pins the markdown: the CLI's headers, numeric columns
// right-aligned, a pipe in a cell escaped, and no paging line when the
// page holds the whole result.
func TestRenderTable(t *testing.T) {
	t.Parallel()
	out := renderLines(t, testSpendLines(t, false), page{}, formatTable)
	want := "spending transactions\n" +
		"| merchant | value_USD |\n" +
		"|---|---:|\n" +
		"| Corner Market | -42.10 |\n" +
		"| Heron Air | -900.00 |\n" +
		"| Corner Market | 15.00 |\n" +
		`| Paper \| Ink |  |`
	if out.text != want {
		t.Errorf("table =\n%s\nwant\n%s", out.text, want)
	}
}

// TestBudgetCutsAtARowBoundary: a page is the longest run of rows from
// offset that fits max_chars, counted in code points over the whole
// text, the paging line included; it holds at least one row, a limit
// within the budget does not move it, and every row is on exactly one
// page.
func TestBudgetCutsAtARowBoundary(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, false)
	whole := utf8.RuneCountInString(renderLines(t, rs, page{}, formatJSON).text)
	for _, format := range []string{formatTable, formatCSV, formatJSON} {
		for budget := 1; budget <= whole+10; budget += 7 {
			seen := 0
			for offset := 0; offset < 4; {
				out := renderLines(t, rs, page{offset: offset, budget: budget}, format)
				rows := pageRows(t, out.text, format)
				if rows == 0 {
					t.Fatalf("%s budget %d offset %d: an empty page\n%s", format, budget, offset, out.text)
				}
				if n := utf8.RuneCountInString(out.text); n > budget && rows > 1 {
					t.Errorf("%s budget %d offset %d: %d characters over %d rows\n%s", format, budget, offset, n, rows, out.text)
				}
				if offset+rows < 4 {
					if bigger := renderLines(t, rs, page{offset: offset, limit: rows + 1, budget: budget}, format); pageRows(t, bigger.text, format) != rows {
						t.Errorf("%s budget %d offset %d: a limit above the page moved it", format, budget, offset)
					}
					// One more row would not have fit.
					if longer := renderLines(t, rs, page{offset: offset, limit: rows + 1, budget: maxIntegerParam}, format); rows > 0 &&
						utf8.RuneCountInString(longer.text) <= budget && pageRows(t, longer.text, format) == rows+1 {
						t.Errorf("%s budget %d offset %d: %d rows fit, %d returned", format, budget, offset, rows+1, rows)
					}
				}
				seen += rows
				offset += rows
			}
			if seen != 4 {
				t.Errorf("%s budget %d: pages held %d rows of 4", format, budget, seen)
			}
		}
	}
}

// pageRows counts a rendered page's rows.
func pageRows(t *testing.T, text, format string) int {
	t.Helper()
	switch format {
	case formatJSON:
		var r jsonResult
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("json: %v\n%s", err, text)
		}
		return r.RowCount
	case formatCSV:
		return strings.Count(text+"\n", "\n") - 2 - strings.Count(text, "\nshowing rows")
	}
	return strings.Count(text, "\n| ") - 1
}

// TestPagingLine states the rows shown and their total, the characters
// shown (the text's own length) and the whole result's, the average
// row, the next offset, the max_chars that holds every row, the
// narrowing advice and the widest columns.
func TestPagingLine(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, false)
	whole := utf8.RuneCountInString(renderLines(t, rs, page{}, formatTable).text)
	out := renderLines(t, rs, page{limit: 2, narrow: "from/to or a filter"}, formatTable)
	last := out.text[strings.LastIndex(out.text, "\n")+1:]
	want := "showing rows 1–2 of 4 (" + thousands(utf8.RuneCountInString(out.text)) + " of " + thousands(whole) +
		" chars, about " + thousands((whole+2)/4) + " a row); next: offset=2; all rows: max_chars=" +
		plainInt(whole) + "; narrow with from/to or a filter; widest columns: merchant 71%, value_USD 28%"
	if last != want {
		t.Errorf("paging line =\n%s\nwant\n%s", last, want)
	}
	end := renderLines(t, rs, page{offset: 2, limit: 2}, formatTable).text
	if !strings.Contains(end, "showing rows 3–4 of 4 (") || strings.Contains(end, "offset=4") {
		t.Errorf("the last page:\n%s", end)
	}
	past := renderLines(t, rs, page{offset: 9}, formatTable).text
	if past != "spending transactions\nno rows at offset=9: the result has 4 rows" {
		t.Errorf("past the end:\n%s", past)
	}
}

func plainInt(n int) string { return strings.ReplaceAll(thousands(n), ",", "") }

// TestRenderJSONContract: every value a string, an empty cell an absent
// key, and under privacy the money keys gone — the CLI's -f json shape;
// a cut page carries the paging line's figures as fields.
func TestRenderJSONContract(t *testing.T) {
	t.Parallel()
	for _, privacy := range []bool{false, true} {
		rs := testSpendLines(t, privacy)
		cols, _, err := columnLookup{cols: rs.rep.columns, currency: "USD"}.pick("merchant,category,value", nil)
		if err != nil {
			t.Fatal(err)
		}
		out := render(rs, page{idx: rs.filter(nil, ""), limit: 3, budget: 16000, narrow: "a filter"}, cols, formatJSON,
			resultHeader{}, resultMeta{tool: "spending"})
		var got jsonResult
		if err := json.Unmarshal([]byte(out.text), &got); err != nil {
			t.Fatalf("json: %v\n%s", err, out.text)
		}
		if got.RowCount != 3 || got.TotalRows != 4 || got.NextOffset == nil || *got.NextOffset != 3 {
			t.Errorf("counts = %d of %d, next %v", got.RowCount, got.TotalRows, got.NextOffset)
		}
		if got.Chars != utf8.RuneCountInString(out.text) || got.TotalChars == 0 || got.CharsPerRow == 0 ||
			got.Narrow != "a filter" || len(got.WidestColumns) == 0 {
			t.Errorf("paging fields = %+v", got)
		}
		_, hasValue := got.Rows[0]["value_USD"]
		if hasValue == privacy {
			t.Errorf("privacy %v: value_USD present = %v", privacy, hasValue)
		}
		if out.structured == nil {
			t.Error("json carries no structured content")
		}
	}
	rs := testSpendLines(t, false)
	out := render(rs, page{idx: []int{3}, budget: 16000}, []int{colIndex(t, rs, "value")}, formatJSON, resultHeader{}, resultMeta{})
	var blank jsonResult
	if err := json.Unmarshal([]byte(out.text), &blank); err != nil {
		t.Fatal(err)
	}
	if _, ok := blank.Rows[0]["value_USD"]; ok {
		t.Errorf("an empty cell rendered as a key: %s", out.text)
	}
	if blank.Chars != 0 || blank.NextOffset != nil {
		t.Errorf("a whole result carries paging fields: %s", out.text)
	}
}

// TestMarkdownCell keeps a statement narrative on one row.
func TestMarkdownCell(t *testing.T) {
	t.Parallel()
	if got := markdownCell("A|B\nC\r\n  D"); got != `A\|B C D` {
		t.Errorf("markdownCell = %q", got)
	}
}
