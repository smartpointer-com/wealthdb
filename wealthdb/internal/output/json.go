package output

import (
	"encoding/json"
	"io"
)

// WriteJSON renders the table as a JSON array of objects, one
// per row, with column names as keys. A value is always a STRING, and
// a column the row has no value for is OMITTED.
//
// A string, because there is no numeric inference: money is a decimal
// string all the way through this product, and handing it to a JSON
// number would round it through a float. A consumer that wants
// arithmetic parses the string with a decimal type, which is the only
// safe way to do it anyway.
//
// Omitted, rather than emitted as an empty string, because "" reads as
// a value rather than an absence — and it is a trap in exactly the
// tool people reach for: in `jq` only `false` and `null` are falsy, so
// `""` is TRUTHY and `select(.merchant)` quietly matches every row. An
// absent key reads back as `null` there, so the obvious filter is
// correct. Nothing is lost by dropping it: the distinction between
// "empty" and "absent" was already gone before the renderer saw the
// cell, since a column's accessor returns a plain string and maps a
// nil pointer to "".
//
// Omitted rather than set to null, because absence is unambiguous
// where null is not — a null can be read as "known to be nothing",
// which is a claim this renderer is in no position to make. It also
// keeps the format to the two states it already had: a key is present
// with a value, or it is not there. The privacy path established that
// shape by dropping a redacted money column from the object entirely
// rather than blanking it.
//
// Output ends with a trailing newline so it composes cleanly
// with shell pipes.
func WriteJSON(w io.Writer, t Table) error {
	out := make([]map[string]any, 0, len(t.Rows))
	for _, row := range t.Rows {
		obj := make(map[string]any, len(t.Columns))
		for i, name := range t.Columns {
			if i < len(row) && row[i] != "" {
				obj[name] = row[i]
			}
		}
		out = append(out, obj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
