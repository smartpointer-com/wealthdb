package gold

import "testing"

func TestTrimTrailingZeros(t *testing.T) {
	cases := []struct{ in, want string }{
		{"100.00000000", "100"},
		{"100.50000000", "100.5"},
		{"100.5000", "100.5"},
		{"0.00100000", "0.001"},
		{"-100.5000", "-100.5"},
		{"100", "100"},        // no decimal point, no-op
		{"0", "0"},            // exact zero
		{"0.0000", "0"},       // trims to "0", not "" or "."
		{"1500.0000", "1500"}, // market_value case
		{"1.5", "1.5"},        // already trimmed
	}
	for _, c := range cases {
		if got := trimTrailingZeros(c.in); got != c.want {
			t.Errorf("trimTrailingZeros(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
