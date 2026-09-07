package silver

import "testing"

// TestJoinText pins the transaction-text composition rule the adapters
// share: trim each part, drop the empty ones, join the rest with "; ",
// and yield "" — never a stray separator — when nothing is left.
func TestJoinText(t *testing.T) {
	for _, c := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"no parts", nil, ""},
		{"every part blank", []string{"", "   ", "\t"}, ""},
		{"one part is trimmed", []string{"  PAYMENT ORDER  "}, "PAYMENT ORDER"},
		{"parts are joined", []string{"PAYMENT ORDER", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"blank parts drop out", []string{"", "PAYMENT ORDER", "  ", "EXAMPLE SHOP"},
			"PAYMENT ORDER; EXAMPLE SHOP"},
		{"an already-separated part is not re-split", []string{"A; B", "C"}, "A; B; C"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JoinText(c.parts...); got != c.want {
				t.Errorf("JoinText(%q) = %q, want %q", c.parts, got, c.want)
			}
		})
	}
}

// TestPayloadWith pins the payload-merge rule the card adapters share: the
// collector's own JSON survives, the adapter's annotations are laid over it,
// and a payload that is not a JSON object never costs the annotations.
func TestPayloadWith(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		extra   map[string]any
		want    string
	}{
		{"no annotations passes the payload through untouched",
			`{"a":1}`, nil, `{"a":1}`},
		{"no annotations does not even reformat",
			`not json at all`, nil, `not json at all`},
		{"annotations are merged in",
			`{"a":1}`, map[string]any{"b": "x"}, `{"a":1,"b":"x"}`},
		{"an annotation overrides the collector's key",
			`{"a":1}`, map[string]any{"a": 2}, `{"a":2}`},
		{"an undecodable payload yields the annotations alone",
			`not json at all`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a JSON non-object yields the annotations alone",
			`[1,2]`, map[string]any{"b": "x"}, `{"b":"x"}`},
		{"a null payload yields the annotations alone",
			`null`, map[string]any{"b": "x"}, `{"b":"x"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(PayloadWith(c.payload, c.extra)); got != c.want {
				t.Errorf("PayloadWith(%q, %v) = %s, want %s",
					c.payload, c.extra, got, c.want)
			}
		})
	}
}
