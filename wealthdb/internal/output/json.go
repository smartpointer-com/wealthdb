package output

import (
	"encoding/json"
	"io"
)

// WriteJSON renders the table as a JSON array of objects, one
// per row, with column names as keys. Cell values are emitted as
// strings (no attempt at numeric inference) so consumers get
// deterministic typing — easier to parse with `jq` and friends
// than a mixed-types JSON would be.
//
// Output ends with a trailing newline so it composes cleanly
// with shell pipes.
func WriteJSON(w io.Writer, t Table) error {
	out := make([]map[string]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		obj := make(map[string]string, len(t.Columns))
		for i, name := range t.Columns {
			if i < len(row) {
				obj[name] = row[i]
			} else {
				obj[name] = ""
			}
		}
		out = append(out, obj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
