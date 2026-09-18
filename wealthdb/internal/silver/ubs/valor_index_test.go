package ubs

import "testing"

// A Swiss ISIN is `CH` plus the valor padded to nine digits plus a
// check digit, so the ISIN IS the valor for every Swiss line — which is
// the only road to the instruments PSN describes without an identifier
// object at all.
func TestValorFromSwissISIN(t *testing.T) {
	for _, tc := range []struct{ isin, want string }{
		{"CH0000000017", "1"},            // leading zeros dropped
		{"CH0012345678", "1234567"},      // the ordinary shape
		{"CH1234567890", "123456789"},    // a full nine-digit valor
		{"US0000000017", ""},             // not Swiss
		{"CH00123456", ""},               // too short to be an ISIN
		{"CH00ABCDEF78", ""},             // not all digits
		{"", ""},
	} {
		if got := valorFromSwissISIN(tc.isin); got != tc.want {
			t.Errorf("valorFromSwissISIN(%q) = %q, want %q", tc.isin, got, tc.want)
		}
	}
}

// One spelling of a number, whichever road stated it, or the two roads
// would index the same instrument twice and collide with themselves.
func TestNormalizeValor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1234567", "1234567"},
		{"0001234567", "1234567"},
		{" 1234567 ", "1234567"},
		{"12A4567", ""},
		{"", ""},
		{"0000", ""},
	} {
		if got := normalizeValor(tc.in); got != tc.want {
			t.Errorf("normalizeValor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
