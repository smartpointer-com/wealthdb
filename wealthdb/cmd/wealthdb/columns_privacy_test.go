package main

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// TestPortfolioNamePrivacyMatchesKind pins that the cointracking
// always-redact rule keys on the source's silver_kind, not on the
// free-form config id — a source named "ct" must still redact, and
// a source that merely NAMED itself "cointracking" but loads
// through another adapter must not. The orphan-account sentinel
// row carries no name at all and stays legible for every source.
func TestPortfolioNamePrivacyMatchesKind(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"ct": "cointracking", "cointracking": "ubs"}
	f := portfolioNamePrivacy(func(id string) string { return kinds[id] })

	row := func(source, extID string) gold.PortfolioRow {
		return gold.PortfolioRow{SilverSourceID: source, PortfolioExternalID: extID}
	}
	if got := f(row("ct", "cu_000001")); got != PrivacyFreeText {
		t.Errorf("kind=cointracking: privacy = %v, want PrivacyFreeText", got)
	}
	if got := f(row("cointracking", "0000001")); got != PrivacyAccountID {
		t.Errorf("id-only match: privacy = %v, want PrivacyAccountID", got)
	}
	if got := f(row("unknown", "0000001")); got != PrivacyAccountID {
		t.Errorf("unknown source: privacy = %v, want PrivacyAccountID", got)
	}
	// Sentinel row ("(no portfolio)"): legible everywhere,
	// including under the cointracking rule.
	if got := f(row("ct", "")); got != PrivacyNone {
		t.Errorf("sentinel row: privacy = %v, want PrivacyNone", got)
	}
}

func TestRedactAccountID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// IBAN-shape: two-letter country prefix kept, full length
		// preserved (so the redacted form looks like an IBAN).
		// Zero-filled placeholder IBANs (AGENTS.md §4), mod-97 invalid
		// by construction — never real account numbers.
		{"CH0000000000000002957", "CH***************2957"},
		{"DE00000000000000003000", "DE****************3000"},
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
		// Multi-word free text is NOT an account id and must keep
		// passing through here — the account-id contract is what
		// keeps display strings legible. Values shaped like this
		// belong to PrivacyFreeText instead (see
		// TestFreeTextRedactsEveryShape).
		{"SAMPLE PAYEE 0001", "SAMPLE PAYEE 0001"},
		// Purely-alphabetic tokens (taxonomy labels) → pass through
		// under the default PrivacyAccountID heuristic.
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

// TestFreeTextRedactsEveryShape is the regression pin for the hole
// PrivacyFreeText exists to close: a bank narrative carrying a
// person's name is multi-word, and every identifier-shaped rule
// waves multi-word values through. This class asks no shape
// question, so single tokens and whole sentences mask alike — in
// every output format, since a narrative is not row identity that
// a csv/json consumer needs back.
func TestFreeTextRedactsEveryShape(t *testing.T) {
	t.Parallel()
	// Synthetic narratives in the shapes a statement actually
	// produces: a P2P payee, a cheque, a wire — plus the single
	// alphanumeric tokens a shape rule does mask, and the
	// two-character one it lets through for being too short.
	values := []string{
		"SAMPLE PAYEE ZZ",
		"ZELLE PAYMENT TO SAMPLE PAYEE ZZ",
		"CHECK #0001 SAMPLE PAYEE ZZ",
		"Sample Payee",
		"SAMPLEPAYEEZZ",
		"SAMPLEPAYEE0001",
		"ab",
	}
	formats := []output.Format{
		output.FormatTable, output.FormatCSV,
		output.FormatCSVPlain, output.FormatJSON,
	}
	for _, v := range values {
		for _, f := range formats {
			if got := applyPrivacy(v, PrivacyFreeText, f); got != "***" {
				t.Errorf("applyPrivacy(%q, PrivacyFreeText, %v) = %q, want %q", v, f, got, "***")
			}
		}
	}
	// Empty cells stay empty rather than growing a placeholder.
	if got := applyPrivacy("", PrivacyFreeText, output.FormatTable); got != "" {
		t.Errorf("applyPrivacy(\"\", PrivacyFreeText) = %q, want empty", got)
	}
	// The same values under the account-id class keep their
	// existing behaviour: multi-word passes through, an
	// identifier-shaped token redacts. Moving a column onto the
	// free-text class must not have moved this contract.
	if got := applyPrivacy("SAMPLE PAYEE ZZ", PrivacyAccountID, output.FormatTable); got != "SAMPLE PAYEE ZZ" {
		t.Errorf("account-id class, multi-word = %q, want it unchanged", got)
	}
	if got := applyPrivacy("PAYEE0001", PrivacyAccountID, output.FormatTable); got != "*****0001" {
		t.Errorf("account-id class, single token = %q, want the masked id", got)
	}
}

// TestFreeTextColumnRedactsInTable drives the class through the
// rendering path a `-p` run actually takes, so the pin covers the
// column wiring and not just the dispatcher.
func TestFreeTextColumnRedactsInTable(t *testing.T) {
	t.Parallel()
	type row struct{ narrative string }
	cols := []columnSpec[row]{
		{Name: "description", Privacy: PrivacyFreeText,
			Extract: func(r row) string { return r.narrative }},
	}
	rows := []row{{narrative: "WIRE FROM SAMPLE PAYEE ZZ"}}

	off := rowsToTable(rows, cols, false, output.FormatTable)
	if off.Rows[0][0] != "WIRE FROM SAMPLE PAYEE ZZ" {
		t.Errorf("privacy off = %q, want it legible", off.Rows[0][0])
	}
	on := rowsToTable(rows, cols, true, output.FormatTable)
	if on.Rows[0][0] != "***" {
		t.Errorf("privacy on = %q, want the free-text placeholder", on.Rows[0][0])
	}
}

// TestScrubSensitiveIDs covers the content-based defence-in-depth
// pass that masks structured bank identifiers wherever they appear,
// independent of a column's PrivacyClass. Synthetic ids only.
func TestScrubSensitiveIDs(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
