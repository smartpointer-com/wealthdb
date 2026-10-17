package ledgercsv

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// TestOpenEmptyInputIsAnEmptyLedger pins that a file with no header at
// all reads as no records rather than as a missing-column error.
func TestOpenEmptyInputIsAnEmptyLedger(t *testing.T) {
	lr, err := Open(strings.NewReader(""), []string{"a"})
	if err != nil || lr != nil {
		t.Fatalf("Open(empty) = (%v, %v), want (nil, nil)", lr, err)
	}
}

// TestOpenReportsTheFirstMissingColumn pins the required-column check:
// headers match trimmed and lower-cased, and the first column missing
// in the order given is the one named.
func TestOpenReportsTheFirstMissingColumn(t *testing.T) {
	if _, err := Open(strings.NewReader(" A ,B\n"), []string{"a", "b"}); err != nil {
		t.Fatalf("Open with every column: %v", err)
	}
	_, err := Open(strings.NewReader("a\n"), []string{"a", "c", "b"})
	if err == nil || err.Error() != `missing required column "c"` {
		t.Fatalf("Open = %v, want missing required column \"c\"", err)
	}
}

// TestOpenWrapsAHeaderReadError pins the header error's text and that
// the CSV error stays reachable.
func TestOpenWrapsAHeaderReadError(t *testing.T) {
	_, err := Open(strings.NewReader("a,\"b\n"), []string{"a"})
	if err == nil || !strings.HasPrefix(err.Error(), "read header: ") {
		t.Fatalf("Open = %v, want a read header error", err)
	}
}

// TestColumnsAreTheNormalisedHeader pins what a ledger's unknown-column
// check reads: every header cell, trimmed and lower-cased, in order,
// with an empty trailing cell as an empty name.
func TestColumnsAreTheNormalisedHeader(t *testing.T) {
	lr, err := Open(strings.NewReader("Silver_Source_ID, Amount ,note,\n"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []string{"silver_source_id", "amount", "note", ""}
	if got := lr.Columns(); !slices.Equal(got, want) {
		t.Errorf("Columns() = %q, want %q", got, want)
	}
}

// TestNextSkipsBlankRecordsAndCountsLines pins the record walk: a record
// of empty fields is skipped but counted, so the line a ledger reports
// for the record after it stays the record's own; a field is read by
// header name, trimmed; and a short record reads its missing fields
// and an unnamed column as "".
func TestNextSkipsBlankRecordsAndCountsLines(t *testing.T) {
	in := "b,a,c\n" +
		"2, 1 ,3\n" +
		" , ,\n" +
		"5\n"
	lr, err := Open(strings.NewReader(in), []string{"a"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	row, line, err := lr.Next()
	if err != nil || line != 2 {
		t.Fatalf("first Next = (line %d, %v), want line 2", line, err)
	}
	if row.Get("a") != "1" || row.Get("b") != "2" || row.Get("c") != "3" {
		t.Errorf("first row = (%q, %q, %q), want (1, 2, 3)", row.Get("a"), row.Get("b"), row.Get("c"))
	}

	row, line, err = lr.Next()
	if err != nil || line != 4 {
		t.Fatalf("second Next = (line %d, %v), want line 4: the blank record is line 3", line, err)
	}
	if row.Get("b") != "5" || row.Get("a") != "" || row.Get("note") != "" {
		t.Errorf("short row = (b %q, a %q, note %q), want (5, \"\", \"\")", row.Get("b"), row.Get("a"), row.Get("note"))
	}

	if _, _, err := lr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after the last record = %v, want io.EOF", err)
	}
}

// TestNextNamesTheLineOfAParseError pins the record error: the line it
// failed on leads the text.
func TestNextNamesTheLineOfAParseError(t *testing.T) {
	lr, err := Open(strings.NewReader("a\n1\n\"x\n"), []string{"a"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := lr.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	_, line, err := lr.Next()
	if err == nil || line != 3 || !strings.HasPrefix(err.Error(), "line 3: ") {
		t.Fatalf("Next = (line %d, %v), want a line 3 error", line, err)
	}
}
