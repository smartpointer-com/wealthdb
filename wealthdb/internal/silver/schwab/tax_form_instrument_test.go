package schwab

import "testing"

// The 1099-B states its instrument by name and nothing else. Both
// shapes it uses are keys the other feeds store; anything else states
// nothing rather than a guess.
func TestSecurityNameKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		ok   bool
	}{
		{"EXA", "EXA", true},
		{"EXAMP", "EXAMP", true},
		// A share class is spelled into the ticker; refusing the slash
		// would drop a real holding for punctuation.
		{"EXA/B", "EXA/B", true},
		// An option keeps its contract: the lot is the contract's, not
		// the underlying's.
		{"EXA 01/20/2023 400.00 C", "EXA 01/20/2023 400.00 C", true},
		{" EXA  07/16/2021 19.00 P ", "EXA 07/16/2021 19.00 P", true},
		// Neither shape: a descriptive option line, a name with spaces,
		// an empty cell.
		{"PUT EXAMPLE HEALTH INVTS $10 EXP 07/16/21", "", false},
		{"EXAMPLE CORP CLASS A", "", false},
		{"", "", false},
		// An expiry without a strike is not an option line.
		{"EXA 01/20/2023", "", false},
	} {
		key, ok := securityNameKey(tc.name)
		if key != tc.key || ok != tc.ok {
			t.Errorf("securityNameKey(%q) = (%q, %v), want (%q, %v)", tc.name, key, ok, tc.key, tc.ok)
		}
	}
}
