package main

import (
	"encoding/json"
	"testing"
)

// TestRenderTable pins the markdown: the CLI's headers, numeric columns
// right-aligned, a pipe in a cell escaped, and the paging line.
func TestRenderTable(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, false)
	merchant, value := colIndex(t, rs, "merchant"), colIndex(t, rs, "value")
	idx := rs.filter(nil, "")
	p := page{idx: cut(idx, 2, 2), total: len(idx), offset: 2, limit: 2, narrow: "from/to or a filter"}
	head := resultHeader{}
	head.add("spending transactions")
	out := render(rs, p, []int{merchant, value}, formatTable, head, resultMeta{})
	want := "spending transactions\n" +
		"| merchant | value_USD |\n" +
		"|---|---:|\n" +
		"| Corner Market | 15.00 |\n" +
		`| Paper \| Ink |  |` + "\n" +
		"showing rows 3–4 of 4"
	if out.text != want {
		t.Errorf("table =\n%s\nwant\n%s", out.text, want)
	}
}

func TestPagingLine(t *testing.T) {
	t.Parallel()
	idx := func(n int) []int { return make([]int, n) }
	cases := []struct {
		p    page
		want string
	}{
		{page{idx: idx(5), total: 5, limit: 100}, ""},
		{page{idx: idx(100), total: 1532, limit: 100, narrow: "a filter"},
			"showing rows 1–100 of 1,532 — pass offset=100 for the next 100, or narrow with a filter"},
		{page{idx: idx(32), total: 1532, offset: 1500, limit: 100, narrow: "a filter"}, "showing rows 1,501–1,532 of 1,532"},
		{page{idx: idx(50), total: 80, limit: 50, capped: true, narrow: "a filter"},
			"showing rows 1–50 of 80 — pass offset=50 for the next 30, or narrow with a filter (this server returns at most 50 rows per call)"},
		{page{total: 80, offset: 90, limit: 10}, "no rows at offset=90: the result has 80 rows"},
	}
	for _, c := range cases {
		if got := c.p.pagingLine(); got != c.want {
			t.Errorf("pagingLine(%+v) =\n%q\nwant\n%q", c.p, got, c.want)
		}
	}
}

// TestRenderJSONContract: every value a string, an empty cell an absent
// key, and under privacy the money keys gone — the CLI's -f json shape.
func TestRenderJSONContract(t *testing.T) {
	t.Parallel()
	for _, privacy := range []bool{false, true} {
		rs := testSpendLines(t, privacy)
		cols, _, err := columnLookup{cols: rs.rep.columns, currency: "USD"}.pick("merchant,category,value", nil)
		if err != nil {
			t.Fatal(err)
		}
		idx := rs.filter(nil, "")
		out := render(rs, page{idx: idx[:3], total: 4, limit: 3}, cols, formatJSON, resultHeader{}, resultMeta{tool: "spending"})
		var got jsonResult
		if err := json.Unmarshal([]byte(out.text), &got); err != nil {
			t.Fatalf("json: %v\n%s", err, out.text)
		}
		if got.RowCount != 3 || got.TotalRows != 4 || got.NextOffset == nil || *got.NextOffset != 3 {
			t.Errorf("counts = %d of %d, next %v", got.RowCount, got.TotalRows, got.NextOffset)
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
	out := render(rs, page{idx: []int{3}, total: 1}, []int{colIndex(t, rs, "value")}, formatJSON, resultHeader{}, resultMeta{})
	var blank jsonResult
	if err := json.Unmarshal([]byte(out.text), &blank); err != nil {
		t.Fatal(err)
	}
	if _, ok := blank.Rows[0]["value_USD"]; ok {
		t.Errorf("an empty cell rendered as a key: %s", out.text)
	}
}

// TestMarkdownCell keeps a statement narrative on one row.
func TestMarkdownCell(t *testing.T) {
	t.Parallel()
	if got := markdownCell("A|B\nC\r\n  D"); got != `A\|B C D` {
		t.Errorf("markdownCell = %q", got)
	}
}
