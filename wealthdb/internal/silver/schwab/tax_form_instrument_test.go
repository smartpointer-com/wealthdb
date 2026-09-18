package schwab

import "testing"

// The 1099-B states its instrument by name and nothing else. Both
// shapes it uses resolve; anything else states nothing rather than a
// guess.
func TestSecurityNameInstrument(t *testing.T) {
	for _, tc := range []struct {
		name     string
		symbol   string
		isOption bool
		ok       bool
	}{
		{"EXA", "EXA", false, true},
		{"EXAMP", "EXAMP", false, true},
		// A share class is spelled into the ticker; refusing the slash
		// would drop a real holding for punctuation.
		{"EXA/B", "EXA/B", false, true},
		// An option resolves to its UNDERLYING: the exposure the trade
		// touched is that instrument's, and the option is the wrapper.
		{"EXA 01/20/2023 400.00 C", "EXA", true, true},
		{"EXA 07/16/2021 19.00 P", "EXA", true, true},
		// Neither shape: a descriptive option line, a name with spaces,
		// an empty cell.
		{"PUT EXAMPLE HEALTH INVTS $10 EXP 07/16/21", "", false, false},
		{"EXAMPLE CORP CLASS A", "", false, false},
		{"", "", false, false},
		// An expiry without a strike is not an option line.
		{"EXA 01/20/2023", "", false, false},
	} {
		sym, isOpt, ok := securityNameInstrument(tc.name)
		if sym != tc.symbol || isOpt != tc.isOption || ok != tc.ok {
			t.Errorf("securityNameInstrument(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.name, sym, isOpt, ok, tc.symbol, tc.isOption, tc.ok)
		}
	}
}
