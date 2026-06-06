package main

import "testing"

func TestRedactAccountID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// IBAN-shape: two-letter country prefix kept, full length
		// preserved (so the redacted form looks like an IBAN).
		// Synthetic example IBANs only — never real account numbers.
		{"CH9300762011623852957", "CH***************2957"},
		{"DE89370400440532013000", "DE****************3000"},
		// Long alphanumeric (Schwab hashValue, 64 hex chars) is
		// not IBAN-shape (length > 34); full length preserved with
		// the trailing 4 chars visible. Synthetic hex pattern.
		{"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF",
			"************************************************************CDEF"},
		// Fidelity-style 9-digit account number (synthetic).
		{"123456789", "*****6789"},
		// Swissquote-style 7-digit customer ID → last 3 (synthetic).
		{"1234567", "****567"},
		// UBS safekeeping shape: alphanumeric, contains digits (synthetic).
		{"00000000000000S4", "************00S4"},
		// UBS-adapter synthetic per-portfolio overlay ID. The
		// portfolio_id prefix redacts as a normal ID; ":overlay"
		// is a structural marker preserved verbatim.
		{"1234567890123456:overlay", "************3456:overlay"},
		// Schwab web suffix (3-5 digit account-number tail, synthetic).
		{"000", "***"},
		{"0012", "**12"},
		{"00045", "***45"},
		// Display strings / sentinels: any non-alphanumeric → pass
		// through unchanged.
		{"Portfolio overlay", "Portfolio overlay"},
		{"(no portfolio)", "(no portfolio)"},
		{"Brokerage (other)", "Brokerage (other)"},
		// Purely-alphabetic tokens (taxonomy labels) → pass through
		// under the default PrivacyAccountID heuristic.
		{"Savings", "Savings"},
		{"Brokerage", "Brokerage"},
		// Empty input passes through unchanged.
		{"", ""},
	}
	for _, c := range cases {
		got := redactAccountID(c.in, false)
		if got != c.want {
			t.Errorf("redactAccountID(%q, false) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRedactAccountIDForceAlpha covers the PrivacyCustomerLabel
// path — where purely-alphabetic strings ARE customer-identifying
// (cointracking portfolio names) and must redact too. Same length-
// sliding suffix rules; the only behavioural change vs the default
// path is that the "has digit" exemption is dropped.
func TestRedactAccountIDForceAlpha(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Purely-alphabetic CT-portfolio-shaped tokens. Synthetic.
		{"abc", "***"},
		{"abcd", "**cd"},
		{"abcde", "***de"},
		{"abcdef", "***def"},
		{"abcdefg", "****efg"},
		{"abcdefgh", "****efgh"},
		{"abcdefghij", "******ghij"},
		// Mixed-case + digit, same length-sliding redaction.
		{"Abc2", "**c2"},
		// Non-alphanumeric still passes through (cash sentinels,
		// "(no portfolio)" etc., even under PrivacyCustomerLabel).
		{"(no portfolio)", "(no portfolio)"},
		// Empty input passes through unchanged.
		{"", ""},
	}
	for _, c := range cases {
		got := redactAccountID(c.in, true)
		if got != c.want {
			t.Errorf("redactAccountID(%q, true) = %q, want %q", c.in, got, c.want)
		}
	}
}
