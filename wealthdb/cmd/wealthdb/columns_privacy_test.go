package main

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

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

// TestScrubSensitiveIDs covers the content-based defence-in-depth
// pass that masks structured bank identifiers wherever they appear,
// independent of a column's PrivacyClass. Synthetic ids only.
func TestScrubSensitiveIDs(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// UBS relationship_id "<4-digit branch> <8-digit base>":
		// masked, last 4 digits kept. Synthetic.
		{"1234 00000001", "**** ****0001"},
		// UBS mortgage account number embeds the relationship prefix;
		// the ".MMM <n>" product suffix is not sensitive and stays.
		{"1234 00000001.MMM 0000", "**** ****0001.MMM 0000"},
		// Portfolio form "<rel> R001": the trailing portfolio marker
		// is preserved, only the relationship core is masked.
		{"1234 00000001 R001", "**** ****0001 R001"},
		// Embedded inside a longer free-text cell.
		{"Mortgage 1234 00000001 (fixed)", "Mortgage **** ****0001 (fixed)"},
		// Non-matches pass through untouched:
		//   ISIN-shaped (no internal space). Synthetic placeholder.
		{"CH0000000009", "CH0000000009"},
		//   a 5-then-8 digit run is not the 4+8 shape
		{"12345 00000001", "12345 00000001"},
		//   4+7 and 4+9 digit runs don't match the exact 4+8 shape
		{"1234 0000001", "1234 0000001"},
		{"1234 000000012", "1234 000000012"},
		//   display labels / taxonomy
		{"Brokerage (other)", "Brokerage (other)"},
		{"iShares Core MSCI World", "iShares Core MSCI World"},
		{"", ""},
	}
	for _, c := range cases {
		if got := scrubSensitiveIDs(c.in); got != c.want {
			t.Errorf("scrubSensitiveIDs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestScrubReachesPrivacyNoneColumn proves the scrub fires on a
// PrivacyNone column (e.g. position_key) under privacy mode — the
// case the per-column applyPrivacy pass alone misses.
func TestScrubReachesPrivacyNoneColumn(t *testing.T) {
	type row struct{ key string }
	cols := []columnSpec[row]{
		// PrivacyNone, mirroring position_key / symbol.
		{Name: "position_key", Extract: func(r row) string { return r.key }},
	}
	rows := []row{{key: "1234 00000001.MMM 0000"}}

	// privacy off: legible.
	off := rowsToTable(rows, cols, false, output.FormatTable)
	if off.Rows[0][0] != "1234 00000001.MMM 0000" {
		t.Errorf("privacy off = %q, want it legible", off.Rows[0][0])
	}
	// privacy on: scrubbed even though the column is PrivacyNone.
	on := rowsToTable(rows, cols, true, output.FormatTable)
	if on.Rows[0][0] != "**** ****0001.MMM 0000" {
		t.Errorf("privacy on = %q, want the account number masked", on.Rows[0][0])
	}
}
