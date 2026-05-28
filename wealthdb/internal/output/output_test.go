package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want Format
	}{
		{"table", true, FormatTable},
		{"csv", true, FormatCSV},
		{"csv_plain", true, FormatCSVPlain},
		{"json", true, FormatJSON},
		{"yaml", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if (err == nil) != c.ok {
			t.Errorf("Parse(%q) err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWriteTableBasic(t *testing.T) {
	var buf bytes.Buffer
	err := WriteTable(&buf, Table{
		Columns: []string{"id", "kind", "qty"},
		Rows: [][]string{
			{"schwab-retail", "equity", "100"},
			{"ubs-main", "bond", "50"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	// Spot-check the structure rather than the whole golden string:
	// header, separator, data rows, footer.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5\noutput:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "id") || !strings.Contains(lines[0], "kind") {
		t.Errorf("header line missing column names: %q", lines[0])
	}
	if !strings.Contains(lines[1], "---") || !strings.Contains(lines[1], "+") {
		t.Errorf("separator line malformed: %q", lines[1])
	}
	if !strings.Contains(lines[2], "schwab-retail") || !strings.Contains(lines[2], "100") {
		t.Errorf("first data row malformed: %q", lines[2])
	}
	if lines[4] != "(2 rows)" {
		t.Errorf("footer = %q, want (2 rows)", lines[4])
	}
}

func TestWriteTableSingleRowFooter(t *testing.T) {
	var buf bytes.Buffer
	WriteTable(&buf, Table{
		Columns: []string{"id"},
		Rows:    [][]string{{"only"}},
	})
	if !strings.Contains(buf.String(), "(1 row)") {
		t.Errorf("expected (1 row) footer, got:\n%s", buf.String())
	}
}

func TestWriteTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	WriteTable(&buf, Table{
		Columns: []string{"id"},
		Rows:    nil,
	})
	if !strings.Contains(buf.String(), "(0 rows)") {
		t.Errorf("expected (0 rows) footer, got:\n%s", buf.String())
	}
}

func TestWriteTableRightAlign(t *testing.T) {
	var buf bytes.Buffer
	WriteTable(&buf, Table{
		Columns: []string{"name", "qty"},
		Aligns:  []Alignment{AlignLeft, AlignRight},
		Rows: [][]string{
			{"apples", "5"},
			{"oranges", "100"},
		},
	})
	lines := strings.Split(buf.String(), "\n")
	// Data row layout for the right-aligned `qty` column: cell
	// padded on the LEFT. With widest cell "100" (3 chars), "5"
	// renders as "  5" inside its column.
	if !strings.Contains(lines[2], "|   5 ") {
		t.Errorf("apples row not right-aligned in qty col:\n%s", lines[2])
	}
	if !strings.Contains(lines[3], "| 100 ") {
		t.Errorf("oranges row alignment wrong:\n%s", lines[3])
	}
}

func TestWriteTableColumnAlignment(t *testing.T) {
	var buf bytes.Buffer
	WriteTable(&buf, Table{
		Columns: []string{"short", "longer_column"},
		Rows:    [][]string{{"x", "y"}, {"longer_data_here", "z"}},
	})
	lines := strings.Split(buf.String(), "\n")
	// Every line (header + separator + 2 data + footer) should
	// have the same byte length for the rendered cells; only check
	// header vs first data row alignment.
	if len(lines[0]) != len(lines[2]) || len(lines[0]) != len(lines[3]) {
		t.Errorf("rows not aligned:\nh: %s\n1: %s\n2: %s", lines[0], lines[2], lines[3])
	}
}
