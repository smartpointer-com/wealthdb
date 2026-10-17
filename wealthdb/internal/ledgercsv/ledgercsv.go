// Package ledgercsv reads the config-sourced CSV ledgers: the
// equity-transfer ledger, the pins ledgers and the transfer-override
// ledger. Each is a header row naming its columns, then one record per
// line, read by column name so the columns may come in any order.
//
// The package owns the reading only. Which columns a ledger refuses,
// how it parses an amount and what a record means stay with the
// ledger, and so does the prefix its errors carry.
package ledgercsv

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// Reader walks one ledger's records after its header.
type Reader struct {
	cr      *csv.Reader
	columns []string
	col     map[string]int
	line    int
}

// Open reads r's header and checks that it names every required
// column, in the order given, so the first one missing is the one
// reported. A header cell is matched trimmed and lower-cased. Records
// may be short or long: a field is found by its header position.
//
// An input with no header at all is an empty ledger: Open returns a
// nil Reader and no error.
func Open(r io.Reader, required []string) (*Reader, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1 // tolerate trailing/blank columns; fields are found by header
	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	lr := &Reader{cr: cr, columns: make([]string, len(header)), col: map[string]int{}, line: 1}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		lr.columns[i] = name
		lr.col[name] = i
	}
	for _, c := range required {
		if _, ok := lr.col[c]; !ok {
			return nil, fmt.Errorf("missing required column %q", c)
		}
	}
	return lr, nil
}

// Columns returns the header's column names as Open matches them,
// trimmed and lower-cased, in header order. A wholly empty header cell
// is an empty name.
func (r *Reader) Columns() []string { return r.columns }

// Next returns the next record that is not blank, with its line: the
// header is line 1 and each record read counts one more, a blank one
// included. It returns io.EOF after the last record, and a record the
// CSV reader cannot parse as an error naming its line.
func (r *Reader) Next() (Row, int, error) {
	for {
		rec, err := r.cr.Read()
		if err == io.EOF {
			return Row{}, 0, io.EOF
		}
		r.line++
		if err != nil {
			return Row{}, r.line, fmt.Errorf("line %d: %w", r.line, err)
		}
		if blank(rec) {
			continue
		}
		return Row{rec: rec, col: r.col}, r.line, nil
	}
}

// Row is one ledger record, read by column name.
type Row struct {
	rec []string
	col map[string]int
}

// Get returns the named column's field, trimmed. A column the header
// does not name, or a field past the record's end, reads as "".
func (r Row) Get(name string) string {
	i, ok := r.col[name]
	if !ok || i >= len(r.rec) {
		return ""
	}
	return strings.TrimSpace(r.rec[i])
}

// blank reports whether every field of rec is empty or whitespace.
func blank(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}
