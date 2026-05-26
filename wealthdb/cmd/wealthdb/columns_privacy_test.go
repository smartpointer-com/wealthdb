package main

import "testing"

func TestRedactAccountID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// IBAN-shape: two-letter country prefix kept, full length
		// preserved (so the redacted form looks like an IBAN).
		{"CH9300762011623852957", "CH***************2957"},
		{"DE89370400440532013000", "DE****************3000"},
		// Long alphanumeric (Schwab hashValue, 64 hex chars) is
		// not IBAN-shape (length > 34); full length preserved
		// with the trailing 4 chars visible.
		{"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
			"************************************************************CDEF"},
		// Fidelity-style 9-digit account number.
		{"123456789", "*****6789"},
		// Swissquote-style 7-digit customer ID → last 3.
		{"1234567", "****567"},
		// UBS safekeeping shape: alphanumeric, contains digits.
		{"00000000000000S4", "************00S4"},
		// UBS-adapter synthetic per-portfolio overlay ID. The
		// portfolio_id prefix redacts as a normal ID; ":overlay"
		// is a structural marker preserved verbatim.
		{"1234567890123456:overlay", "************3456:overlay"},
		// Schwab web suffix (3-5 digit account-number tail).
		{"000", "***"},
		{"0012", "**12"},
		{"00045", "***45"},
		// Display strings / sentinels: any non-alphanumeric → pass
		// through unchanged.
		{"Portfolio overlay", "Portfolio overlay"},
		{"(no portfolio)", "(no portfolio)"},
		{"Brokerage (other)", "Brokerage (other)"},
		// Purely-alphabetic tokens (taxonomy labels) → pass through.
		{"Savings", "Savings"},
		{"Brokerage", "Brokerage"},
		// Empty input passes through unchanged.
		{"", ""},
	}
	for _, c := range cases {
		got := redactAccountID(c.in)
		if got != c.want {
			t.Errorf("redactAccountID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
