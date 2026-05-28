package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleTable() Table {
	return Table{
		Columns: []string{"silver", "symbol", "qty"},
		Aligns:  []Alignment{AlignLeft, AlignLeft, AlignRight},
		Rows: [][]string{
			{"schwab", "AAPL", "100"},
			{"ubs", "XS1234,567890", "50"}, // comma in cell — CSV must quote
			{"swissquote", "", "0"},
		},
	}
}

func TestWriteCSVWithHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, sampleTable()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "silver,symbol,qty\n") {
		t.Errorf("missing header row: %q", out)
	}
	if !strings.Contains(out, `"XS1234,567890"`) {
		t.Errorf("CSV should quote commas inside cells: %q", out)
	}
	if !strings.Contains(out, "schwab,AAPL,100\n") {
		t.Errorf("simple row missing/malformed: %q", out)
	}
}

func TestWriteCSVPlain(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSVPlain(&buf, sampleTable()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.HasPrefix(out, "silver,symbol,qty") {
		t.Errorf("csv_plain should NOT emit a header row: %q", out)
	}
	if !strings.HasPrefix(out, "schwab,AAPL,100\n") {
		t.Errorf("csv_plain first row malformed: %q", out)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleTable()); err != nil {
		t.Fatal(err)
	}
	var parsed []map[string]string
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, buf.String())
	}
	if len(parsed) != 3 {
		t.Fatalf("rows = %d, want 3", len(parsed))
	}
	if parsed[0]["silver"] != "schwab" || parsed[0]["symbol"] != "AAPL" || parsed[0]["qty"] != "100" {
		t.Errorf("first row decoded wrong: %+v", parsed[0])
	}
	// Cells with commas should not have leaked CSV escaping.
	if parsed[1]["symbol"] != "XS1234,567890" {
		t.Errorf("comma cell lost: %+v", parsed[1])
	}
	// Empty cell remains an empty string, not null.
	if parsed[2]["symbol"] != "" {
		t.Errorf("empty cell = %q, want empty string", parsed[2]["symbol"])
	}
}
