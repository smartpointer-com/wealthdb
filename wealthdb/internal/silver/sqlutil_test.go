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
