package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/enrichtest"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// noKinds is a kindOf that knows no source, for a registry a test renders
// with no gold behind it.
func noKinds(string) string { return "" }

// TestPortfolioNamePrivacyMatchesKind pins that the always-redact rule
// for a typed source (cointracking, manual, the declared accounts) keys
// on the source's silver_kind, not on the free-form config id — a
// source named "ct" must still redact, and a source that merely NAMED
// itself "cointracking" but loads through another adapter must not. The
// orphan-account sentinel row carries no name at all and stays legible
// for every source.
func TestPortfolioNamePrivacyMatchesKind(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"ct": "cointracking", "hand": "manual", "cointracking": "ubs"}
	f := func(source, id string) PrivacyClass {
		return portfolioNamePrivacy(func(id string) string { return kinds[id] }, source, id)
	}
	if got := f("ct", "cu_000001"); got != PrivacyPseudonym {
		t.Errorf("kind=cointracking: privacy = %v, want PrivacyPseudonym", got)
	}
	if got := f("hand", "example-house"); got != PrivacyPseudonym {
		t.Errorf("kind=manual: privacy = %v, want PrivacyPseudonym", got)
	}
	if got := f(canonical.DeclaredSourceID, "house"); got != PrivacyPseudonym {
		t.Errorf("declared source: privacy = %v, want PrivacyPseudonym", got)
	}
	if got := f("cointracking", "0000001"); got != PrivacyAccountID {
		t.Errorf("id-only match: privacy = %v, want PrivacyAccountID", got)
	}
	if got := f("unknown", "0000001"); got != PrivacyAccountID {
		t.Errorf("unknown source: privacy = %v, want PrivacyAccountID", got)
	}
	// Sentinel row ("(no portfolio)"): legible everywhere,
	// including under the cointracking rule.
	if got := f("ct", ""); got != PrivacyNone {
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
		// Letters, a dash, digits: one id, letters and all.
		{"ABC-123456", "***-**3456"},
		// Spaced and dotted digits: one id, separators kept.
		{"1234 5678 9012", "**** **** 9012"},
		{"1.234.567.890.12", "*.***.***.*90.12"},
		{"1234.567890.1", "****.***890.1"},
		// A spaced IBAN keeps its country code like a plain one.
		{"CH00 0000 0000 0000 0295 7", "CH** **** **** **** *295 7"},
		// A long number takes its product code and its tail along.
		{"1234 00000001.MMM 0000", "**** ********.*** 0000"},
		// Other separators join digits too.
		{"12345678/90", "******78/90"},
		{"1234 - 5678", "**** - 5678"},
		// A short run masks like a short plain id.
		{"12-345", "**-*45"},
		// Each id of a composite keeps its own tail.
		{"1234567890:CH0000000000000002957:0987654321",
			"******7890:CH***************2957:******4321"},
		{"1234567890@CH0000000000000002957", "******7890@CH***************2957"},
		{"1234567890@CH0000000000000002957:WIR", "******7890@CH***************2957:WIR"},
		{"BUY:1234567890@0987654321", "BUY:******7890@******4321"},
		// A lot id: digits, a hash sign, a long hash.
		{"123456789012#0123456789abcdef0123456789abcdef",
			"********9012#****************************cdef"},
		// A short id in a composite masks as it does alone.
		{"1234:5678", "**34:**78"},
		// Non-ASCII letters count as letters, and length is in characters.
		{"Küche-123456", "*****-**3456"},
		// A word stays beside an id: a letter group never joins
		// through a space.
		{"Mortgage 12345678", "Mortgage ****5678"},
		{"Savings-Plus 12345678", "Savings-Plus ****5678"},
		{"12345678 Joint 123456", "****5678 Joint ***456"},
		// A long token of letters and digits is an id, whatever its
		// digits number and whatever pads it.
		{"AbCdEfGhIjKl3MnOpQrS==:WIR", "Ab**************pQrS==:WIR"},
		// A mixed group is an id from 8 characters, and shorter it
		// stays beside a word.
		{"Plan ABCD12E", "Plan ABCD12E"},
		{"Plan ABCD12EF", "Plan ****12EF"},
		// A later group's being mixed makes the whole run an id.
		{"AB-CDEFG1HIJ", "**-*****1HIJ"},
		// A code keeps its country only from 15 to 34 characters, and
		// only when both of its first two are letters.
		{"AB345678901234", "**********1234"},
		{"AB345678901234567890123456789012345", "*******************************2345"},
		{"A12345678901234", "***********1234"},
		// A semicolon and a space end a phrase too.
		{"Tier 12; 3456", "Tier 12; 3456"},
		// A comma and a space end a phrase, so numbers listed that way
		// are no run. An address is the scrub's to mask.
		{"Example Plan, Tier 12, 3456", "Example Plan, Tier 12, 3456"},
		// Up to four digits stay, alone or in a run: masking them
		// would hide nothing -p shows of an id.
		{"Portfolio 2024", "Portfolio 2024"},
		{"401(k)", "401(k)"},
		{"Savings 3", "Savings 3"},
		{"Rewards card ****1234", "Rewards card ****1234"},
		{"Pillar 3a", "Pillar 3a"},
		{"Series A1", "Series A1"},
		{"ABC-1234", "ABC-1234"},
		// Display strings and sentinels with no id in them stay.
		{"Portfolio overlay", "Portfolio overlay"},
		{"(no portfolio)", "(no portfolio)"},
		{"Brokerage (other)", "Brokerage (other)"},
		{"Example-Name.Part", "Example-Name.Part"},
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
		if a, b := utf8.RuneCountInString(c.in), utf8.RuneCountInString(c.want); a != b {
			t.Errorf("case %q: length %d becomes %d", c.in, a, b)
		}
	}
}

// TestRedactedIDShowsItsTailAtMost: whatever separates an id's digits,
// the redacted value shows no more of them than the plain id shows.
func TestRedactedIDShowsItsTailAtMost(t *testing.T) {
	t.Parallel()
	const id = "987654321098"
	for _, sep := range []string{" ", "  ", "-", ".", "/", "_", "+", ",", "'", " - ", ") ("} {
		for _, split := range [][]int{{4}, {4, 8}, {2, 6, 10}, {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}} {
			var b strings.Builder
			from := 0
			for _, at := range split {
				b.WriteString(id[from:at] + sep)
				from = at
			}
			b.WriteString(id[from:])
			got := redactAccountID(b.String())
			var digits strings.Builder
			for _, r := range got {
				if r >= '0' && r <= '9' {
					digits.WriteRune(r)
				}
			}
			if digits.String() != id[len(id)-4:] {
				t.Errorf("redactAccountID(%q) = %q shows %q", b.String(), got, digits.String())
			}
		}
	}
}

// TestIdentifierPrivacyEveryFormat: an account's spaced id and a label
// around one redact alike in every output format, a filter under -p
// reads the masked text, and the privacy endpoint serves the same.
func TestIdentifierPrivacyEveryFormat(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('bank', 'schwab', '/tmp/bank.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, nickname, base_currency,
		 first_seen_at, last_seen_at)
		 VALUES ('bank', '1357 2468 9753', 'cash', 'Example 2468 9753', NULL, 'USD', 0, 0),
		        ('bank', 'ABC-864297', 'cash', 'Premier Checking', NULL, 'USD', 0, 0)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		 VALUES ('bank', ?, '1357 2468 9753', 'USD', 'current', 100), ('bank', ?, 'ABC-864297', 'USD', 'current', 200)`)
	const cols = "account,account_id"
	accounts := func(flags ...string) string {
		t.Helper()
		args := append([]string{"-c", cfg, "holdings", "accounts", "-d", "2026-06-30", "-C", cols}, flags...)
		so, se, code := run(t, args...)
		if code != 0 {
			t.Fatalf("holdings accounts %v: exit %d: %s", flags, code, se)
		}
		return so
	}
	for _, format := range []string{"table", "csv", "csv_plain", "json"} {
		out := accounts("-p", "-f", format)
		for _, want := range []string{"Example **** 9753", "**** **** 9753", "Premier Checking", "***-**4297"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: -p output lacks %q:\n%s", format, want, out)
			}
		}
		for _, leak := range []string{"1357", "2468", "8642", "ABC"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: -p output shows %q:\n%s", format, leak, out)
			}
		}
	}
	if off := accounts("-f", "csv"); !strings.Contains(off, "Example 2468 9753,1357 2468 9753") {
		t.Errorf("output without -p lost an id:\n%s", off)
	}

	// A filter reads the printed cell: the masked text matches, the
	// real id does not, so a filter cannot probe what -p hides.
	for filter, want := range map[string]bool{
		"account_id = **** **** 9753": true,
		"account_id contains 9753":    true,
		"account_id = 1357 2468 9753": false,
		"account_id contains 2468":    false,
	} {
		if got := strings.Contains(accounts("-p", "-f", "csv", "-F", filter), "9753"); got != want {
			t.Errorf("-p -F %q matched the row: %v, want %v", filter, got, want)
		}
	}

	srv := httptest.NewServer(newHTTPFront(newTestMCP(cfg, false), newTestMCP(cfg, true), testToken, nil))
	defer srv.Close()
	code, body := post(t, srv, "/mcp/privacy", "Bearer "+testToken, nil,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"holdings",
		"arguments":{"view":"accounts","as_of":"2026-06-30","columns":"all"}}}`)
	if code != http.StatusOK {
		t.Fatalf("/mcp/privacy: status %d: %s", code, body)
	}
	if !strings.Contains(body, "**** **** 9753") || !strings.Contains(body, "***-**4297") {
		t.Errorf("/mcp/privacy lacks the masked ids:\n%s", body)
	}
	for _, leak := range []string{"1357", "2468", "8642", "ABC-"} {
		if strings.Contains(body, leak) {
			t.Errorf("/mcp/privacy shows %q:\n%s", leak, body)
		}
	}
}

// TestLikelyAccountIDStaysNarrow pins what lineClass reads: a value
// redactAccountID masks by its runs is still no id of a typed source.
func TestLikelyAccountIDStaysNarrow(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"123456789": true, "ABC123": true, "1234 5678 9012": false, "ABC-123456": false,
		"user:wallet1": false, "Savings": false, "12": false,
	} {
		if got := isLikelyAccountID(in); got != want {
			t.Errorf("isLikelyAccountID(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestFreeTextRedactsEveryShape is the regression pin for the hole
// PrivacyFreeText exists to close: a bank narrative carrying a
// person's name is multi-word, and every identifier-shaped rule
// waves multi-word values through. This class asks no shape
// question, so single tokens and whole sentences mask alike — in
// every output format, since a narrative is not row identity that
// a csv/json consumer needs back. A PrivacyPseudonym cell that reaches
// applyPrivacy, with no pseudonyms to name it, masks alike: it fails
// closed.
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
	for _, class := range []PrivacyClass{PrivacyFreeText, PrivacyPseudonym} {
		for _, v := range values {
			for _, f := range formats {
				if got := applyPrivacy(v, class, f); got != "***" {
					t.Errorf("applyPrivacy(%q, class %d, %v) = %q, want %q", v, class, f, got, "***")
				}
			}
		}
		// Empty cells stay empty rather than growing a placeholder.
		for _, f := range formats {
			if got := applyPrivacy("", class, f); got != "" {
				t.Errorf("applyPrivacy(\"\", class %d, %v) = %q, want empty", class, f, got)
			}
		}
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

	off := rowsToTable(rows, cols, false, output.FormatTable, nil)
	if off.Rows[0][0] != "WIRE FROM SAMPLE PAYEE ZZ" {
		t.Errorf("privacy off = %q, want it legible", off.Rows[0][0])
	}
	on := rowsToTable(rows, cols, true, output.FormatTable, nil)
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
		// the match takes it whole, the ".MMM <n>" tail included.
		{"1234 00000001.MMM 0987", "**** ********.*** 0987"},
		{"ubs:1234 00000001.MMM 0987:x", "ubs:**** ********.*** 0987:x"},
		{"1234 00000001.M1M 0987", "**** ********.*** 0987"},
		// Portfolio form "<rel> R001": the trailing portfolio marker
		// is preserved, only the relationship core is masked.
		{"1234 00000001 R001", "**** ****0001 R001"},
		// Embedded inside a longer free-text cell.
		{"Mortgage 1234 00000001 (fixed)", "Mortgage **** ****0001 (fixed)"},
		// An address closing a label masks to the end of the value, its
		// separators kept. The example is web/test_provision.py's too.
		{"Sample Loan, Invented Way 7b, 12345 Nowhere", "Sample Loan, ******** *** **, ***** *******"},
		{"Sample Loan, Invented Way 7, 1234 Nowhere XX", "Sample Loan, ******** *** *, **** ******* **"},
		// A label with commas before the address keeps them.
		{"Sample, Fixed Loan, Invented Way 7, 1234 Nowhere", "Sample, Fixed Loan, ******** *** *, **** *******"},
		// No street, no address: a list, a date, a security's name.
		{"Sample Fund, 1234 Nowhere", "Sample Fund, 1234 Nowhere"},
		{"Sample Note 2.5%, 01/02/2030", "Sample Note 2.5%, 01/02/2030"},
		{"Sample Loan, Invented Way 7, 123 Nowhere", "Sample Loan, Invented Way 7, 123 Nowhere"},
		{"Sample Loan, Invented Way 7, 1234", "Sample Loan, Invented Way 7, 1234"},
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
	off := rowsToTable(rows, cols, false, output.FormatTable, nil)
	if off.Rows[0][0] != "1234 00000001.MMM 0000" {
		t.Errorf("privacy off = %q, want it legible", off.Rows[0][0])
	}
	// privacy on: scrubbed even though the column is PrivacyNone.
	on := rowsToTable(rows, cols, true, output.FormatTable, nil)
	if on.Rows[0][0] != "**** ********.*** 0000" {
		t.Errorf("privacy on = %q, want the account number masked", on.Rows[0][0])
	}
}

// TestTypedHoldingNameColumns: a manual holding's transaction line
// prints its instrument's name as it is, and its statement narrative,
// the fallback where the instrument has no name, as free text. A cash
// flow line of a manual holding prints its name as it is, and any other
// line's as free text, since it can name a payer.
func TestTypedHoldingNameColumns(t *testing.T) {
	t.Parallel()
	kinds := func(s string) string { return map[string]string{"hand": "manual", "bank": "schwab"}[s] }
	cell := func(cols []string, rows [][]string, name string) []string {
		i := slices.Index(cols, name)
		out := make([]string, len(rows))
		for j, r := range rows {
			out[j] = r[i]
		}
		return out
	}
	txCols, err := resolveColumns("name", defaultTransactionColumns, buildTransactionColumnRegistry("USD", kinds, nil))
	if err != nil {
		t.Fatal(err)
	}
	tx := []gold.TransactionRow{
		{TxnLine: gold.TxnLine{SilverSourceID: "hand"}, InstrumentExternalID: strp("example-flat"), Name: strp("Example Flat")},
		{TxnLine: gold.TxnLine{SilverSourceID: "hand"}, InstrumentExternalID: strp("example-flat"),
			TxnValue: gold.TxnValue{Description: strp("SAMPLE PAYEE REF")}},
		{TxnLine: gold.TxnLine{SilverSourceID: "bank"}, TxnValue: gold.TxnValue{Description: strp("SAMPLE PAYEE REF")}},
	}
	table := rowsToTable(tx, txCols, true, output.FormatCSV, nil)
	if got, want := cell(table.Columns, table.Rows, "name"), []string{"Example Flat", "***", "***"}; !slices.Equal(got, want) {
		t.Errorf("transactions name under -p = %q, want %q", got, want)
	}
	cfCols, err := resolveColumns("name", defaultCashflowTransactionColumns, buildCashflowTransactionColumnRegistry("USD", kinds, nil))
	if err != nil {
		t.Fatal(err)
	}
	cf := []gold.CashflowTransactionRow{
		{TxnLine: gold.TxnLine{SilverSourceID: "hand"}, Name: strp("Example Flat")},
		{TxnLine: gold.TxnLine{SilverSourceID: "bank"}, Name: strp("Sample Payer")},
		{TxnLine: gold.TxnLine{SilverSourceID: "hand"}},
	}
	table = rowsToTable(cf, cfCols, true, output.FormatCSV, nil)
	if got, want := cell(table.Columns, table.Rows, "name"), []string{"Example Flat", "***", ""}; !slices.Equal(got, want) {
		t.Errorf("cashflow name under -p = %q, want %q", got, want)
	}
}

// TestAddressMasksInEveryClass: the scrub runs before the per-column
// pass, so an address masks whole in a redacted column too, where a
// five-digit postal code would otherwise star first and hide the shape.
func TestAddressMasksInEveryClass(t *testing.T) {
	t.Parallel()
	type row struct{ label string }
	get := func(r row) string { return r.label }
	cols := []columnSpec[row]{
		{Name: "name", Extract: get},
		{Name: "account", Extract: get, Privacy: PrivacyAccountID},
		{Name: "description", Extract: get, Privacy: PrivacyFreeText},
	}
	for label, want := range map[string]string{
		"Sample Loan, Invented Way 7b, 12345 Nowhere": "Sample Loan, ******** *** **, ***** *******",
		"Sample Loan, Invented Way 7, 1234 Nowhere":   "Sample Loan, ******** *** *, **** *******",
	} {
		for _, format := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatCSVPlain, output.FormatJSON} {
			got := rowsToTable([]row{{label}}, cols, true, format, nil).Rows[0]
			if got[0] != want || got[1] != want || got[2] != freeTextPlaceholder {
				t.Errorf("%s: %q = %q, want %q in name and account, and %s", format, label, got, want, freeTextPlaceholder)
			}
		}
		if got := rowsToTable([]row{{label}}, cols, false, output.FormatTable, nil).Rows[0][0]; got != label {
			t.Errorf("privacy off = %q, want %q", got, label)
		}
	}
}

// TestAccountColTypedLabels: under -p an account label falls back to the
// account's id, redacted as one, where the source's kind says the name
// may be the customer's own words, the kind names an account by the
// bank's nickname, or the name is the account's nickname. Such an id
// that is not wholly id-shaped may hold typed words too and prints as a
// pseudonym named by the source's kind. A bank's product label stays.
// The rule keys on the kind, never on the source's free-form id, and
// every format shows the same cell. Without -p the label shows as it
// is.
func TestAccountColTypedLabels(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"wallets": "cointracking", "hand": "manual", "bank": "schwab", "roster": "chase",
		"cointracking": "ubs", "venture": "angellist"}
	col := accountCol(func(id string) string { return kinds[id] }, func(a accountRef) accountRef { return a })
	s := func(v string) *string { return &v }
	cases := []struct {
		name    string
		ref     accountRef
		privacy bool
		want    string
	}{
		{"cointracking wallet", accountRef{"wallets", s("Example Person"), nil, "WALLET0001"}, true, "******0001"},
		// A tracker's id names its user and the wallet's label.
		{"cointracking wallet keyed by user and label", accountRef{"wallets", s("Example Person"), nil,
			"cu_000001:Example Person"}, true, "cointracking-1"},
		{"manual account", accountRef{"hand", s("Example Person"), nil, "HAND00001234"}, true, "********1234"},
		// An id-shaped head does not make the words after it an id.
		{"manual account keyed by an id and words", accountRef{"hand", s("Example Person"), nil,
			"ACCT001:Example Person"}, true, "manual-1"},
		{"declared account", accountRef{canonical.DeclaredSourceID, s("Example Person"), nil, "DECL0001"}, true, "****0001"},
		{"manual account keyed by a word", accountRef{"hand", s("Example House"), nil, "example-house"}, true, "manual-1"},
		// Gold holds no kind for the declared accounts: their source names
		// the pseudonym.
		{"declared account keyed by a word", accountRef{canonical.DeclaredSourceID, nil, nil, "house"}, true,
			"declared-1"},
		{"bank account keyed by a word", accountRef{"bank", nil, nil, "savings"}, true, "savings"},
		// A bank that shows the nickname as the account's name shows the
		// customer's words: typed, whatever the kind.
		{"bank name that is the nickname", accountRef{"bank", s("Example Home"), s("Example Home"), "CHK000001234"},
			true, "********1234"},
		{"bank name that is the nickname, keyed by a word", accountRef{"bank", s("Example Home"), s("Example Home"),
			"savings"}, true, "schwab-1"},
		{"bank name beside a nickname", accountRef{"bank", s("Premier Checking"), s("Example Home"), "CHK000001234"},
			true, "Premier Checking"},
		{"bank product label", accountRef{"bank", s("Premier Checking"), nil, "CHK000001234"}, true, "Premier Checking"},
		// This bank names an account by its roster's nickname, which a
		// config nickname replaces in gold but not in the name.
		{"roster name beside an overridden nickname", accountRef{"roster", s("Example Home"), s("Joint"),
			"CHK000001234"}, true, "********1234"},
		{"roster name without a nickname", accountRef{"roster", s("Example Home"), nil, "CHK000001234"}, true,
			"********1234"},
		{"roster account without a name", accountRef{"roster", nil, nil, "CHK000005678"}, true, "********5678"},
		{"source named like a kind", accountRef{"cointracking", s("Savings"), nil, "SAV000001234"}, true, "Savings"},
		// An AngelList account's id is its holder's portal slug: typed,
		// whatever its shape, and so is a label that repeats it.
		{"angellist account keyed by a slug", accountRef{"venture", nil, nil, "example-holder"}, true, "angellist-1"},
		{"angellist account labelled by its slug", accountRef{"venture", s("example-holder"), nil, "example-holder"},
			true, "angellist-1"},
		{"angellist slug with a digit", accountRef{"venture", nil, nil, "exampleholder2"}, true, "angellist-1"},
		{"source of no known kind", accountRef{"other", s("Brokerage"), nil, "BRK000001234"}, true, "Brokerage"},
		// A typed name with a digit would redact as an id, leaving its
		// tail legible; the account's own id shows instead.
		{"typed name with digits", accountRef{"wallets", s("Person2"), nil, "WALLET0002"}, true, "******0002"},
		{"bank label with digits", accountRef{"bank", s("CHK1234"), nil, "CHK000001234"}, true, "****234"},
		{"wallet without a name", accountRef{"wallets", nil, nil, "WALLET0003"}, true, "******0003"},
		{"bank account without a name", accountRef{"bank", nil, nil, "CHK000005678"}, true, "********5678"},
		{"-p off: wallet", accountRef{"wallets", s("Example Person"), nil, "WALLET0001"}, false, "Example Person"},
		{"-p off: bank", accountRef{"bank", s("Premier Checking"), nil, "CHK000001234"}, false, "Premier Checking"},
		{"-p off: no name", accountRef{"wallets", nil, nil, "WALLET0003"}, false, "WALLET0003"},
		{"-p off: keyed by a word", accountRef{"hand", s("Example House"), nil, "example-house"}, false, "Example House"},
		{"-p off: keyed by a slug", accountRef{"venture", nil, nil, "example-holder"}, false, "example-holder"},
		{"-p off: name that is the nickname", accountRef{"bank", s("Example Home"), s("Example Home"), "CHK000001234"},
			false, "Example Home"},
	}
	for _, c := range cases {
		for _, f := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatJSON} {
			got := rowsToTable([]accountRef{c.ref}, []columnSpec[accountRef]{col}, c.privacy, f, nil).Rows[0][0]
			if got != c.want {
				t.Errorf("%s (%s): account = %q, want %q", c.name, f, got, c.want)
			}
		}
	}
}

// TestAccountIDColTypedIDs: under -p an account id redacts as one, but
// the id of an account whose name may be typed prints as a pseudonym
// unless it is wholly id-shaped: such an id can carry the typed words.
// An AngelList account's id is its holder's slug and prints as a
// pseudonym whatever its shape. A bank's id redacts as an id, whatever
// its shape.
func TestAccountIDColTypedIDs(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"wallets": "cointracking", "hand": "manual", "bank": "schwab", "venture": "angellist"}
	col := accountIDCol("account_id", func(id string) string { return kinds[id] }, func(a accountRef) accountRef { return a })
	s := func(v string) *string { return &v }
	cases := []struct {
		name    string
		ref     accountRef
		privacy bool
		want    string
	}{
		{"wallet keyed by user and label", accountRef{"wallets", s("Example Person"), nil, "cu_000001:Example Person"},
			true, "cointracking-1"},
		{"wallet keyed by an id-shaped user", accountRef{"wallets", s("Example Person"), nil, "cu000001:Example Person"},
			true, "cointracking-1"},
		{"wallet keyed by an id", accountRef{"wallets", s("Example Person"), nil, "WALLET0001"}, true, "******0001"},
		{"manual account keyed by an id and words", accountRef{"hand", nil, nil, "ACCT001:Example Person"}, true,
			"manual-1"},
		{"manual account keyed by a word", accountRef{"hand", nil, nil, "example-house"}, true, "manual-1"},
		{"declared account keyed by an id", accountRef{canonical.DeclaredSourceID, nil, nil, "DECL0001"}, true, "****0001"},
		{"angellist account keyed by a slug", accountRef{"venture", nil, nil, "example-holder"}, true, "angellist-1"},
		{"angellist account keyed by an id-shaped slug", accountRef{"venture", nil, nil, "exampleholder2"}, true,
			"angellist-1"},
		{"bank name that is the nickname, keyed by words", accountRef{"bank", s("Example Home"), s("Example Home"),
			"home:Example Home"}, true, "schwab-1"},
		{"bank account", accountRef{"bank", s("Premier Checking"), nil, "CHK000001234"}, true, "********1234"},
		{"bank account keyed by an id and a suffix", accountRef{"bank", s("Premier Checking"), nil, "PF000123:overlay"},
			true, "****0123:overlay"},
		{"-p off", accountRef{"wallets", s("Example Person"), nil, "cu_000001:Example Person"}, false,
			"cu_000001:Example Person"},
	}
	for _, c := range cases {
		for _, f := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatJSON} {
			got := rowsToTable([]accountRef{c.ref}, []columnSpec[accountRef]{col}, c.privacy, f, nil).Rows[0][0]
			if got != c.want {
				t.Errorf("%s (%s): account_id = %q, want %q", c.name, f, got, c.want)
			}
		}
	}
}

// TestPortfolioIDColTypedIDs: a portfolio id redacts by its shape, as an
// account id of its source does. A tracker's `cu_<user>` id names its
// user and prints as a pseudonym; a bank's portfolio id redacts as an
// id, and so does a line id of a source whose account ids are typed.
func TestPortfolioIDColTypedIDs(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"wallets": "cointracking", "bank": "ubs", "venture": "angellist"}
	col := typedIDCol("portfolio_id", func(id string) string { return kinds[id] },
		func(r [2]string) (string, string) { return r[0], r[1] })
	for _, c := range []struct {
		row     [2]string
		privacy bool
		want    string
	}{
		{[2]string{"wallets", "cu_000001"}, true, "cointracking-1"},
		{[2]string{"wallets", "CU000001"}, true, "****0001"},
		{[2]string{"bank", "PF000123"}, true, "****0123"},
		{[2]string{"bank", "Savings"}, true, "Savings"},
		{[2]string{"venture", "PF000123"}, true, "****0123"},
		{[2]string{"wallets", ""}, true, ""},
		{[2]string{"wallets", "cu_000001"}, false, "cu_000001"},
	} {
		got := rowsToTable([][2]string{c.row}, []columnSpec[[2]string]{col}, c.privacy, output.FormatTable, nil).Rows[0][0]
		if got != c.want {
			t.Errorf("%v (privacy %v): portfolio_id = %q, want %q", c.row, c.privacy, got, c.want)
		}
	}
}

// TestCashflowCoverageTypedLabelMasks: a coverage row names its account
// by one label, the nickname where the account has one, and has no id
// to show in its place. Under -p a label that may be typed or that is
// the row's nickname prints as a pseudonym; a bank's label stays.
func TestCashflowCoverageTypedLabelMasks(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"wallets": "cointracking", "bank": "chase", "venture": "angellist",
		"linked": "plaid"}
	cols, err := resolveColumns("silver_source,account", nil,
		buildCashflowCoverageColumnRegistry("monthly", func(id string) string { return kinds[id] }))
	if err != nil {
		t.Fatal(err)
	}
	s := func(v string) *string { return &v }
	rows := []gold.CashflowCoverageRow{{SourceID: "wallets", Account: "Example Person"},
		{SourceID: "bank", Account: "Premier Checking"},
		{SourceID: "bank", Account: "Example Home", Nickname: s("Example Home")},
		{SourceID: "venture", Account: "example-holder"},
		{SourceID: "linked", Account: "Rainy Day Fund", Nickname: s("Rainy Day Fund")},
		{SourceID: "linked", Account: "Example Bank …0000"}}
	for privacy, want := range map[bool][]string{
		true: {"cointracking-1", "Premier Checking", "nickname-1", "angellist-1", "nickname-2",
			"Example Bank …0000"},
		false: {"Example Person", "Premier Checking", "Example Home", "example-holder", "Rainy Day Fund",
			"Example Bank …0000"},
	} {
		tbl := rowsToTable(rows, cols, privacy, output.FormatTable, nil)
		for i, w := range want {
			if got := tbl.Rows[i][1]; got != w {
				t.Errorf("privacy %v, row %d: account = %q, want %q", privacy, i, got, w)
			}
		}
	}
}

// TestHoldingsAccountsPrivacyTypedLabel runs the CLI end to end: under -p
// a cointracking wallet named like a person prints its redacted id, a
// bank account's product label stays, its nickname prints as a
// pseudonym, and a bank account named by its nickname prints its
// redacted id. Every account is invented.
func TestHoldingsAccountsPrivacyTypedLabel(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('wallets', 'cointracking', '/tmp/wallets.db', -1, 0, 0), ('bank', 'schwab', '/tmp/bank.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, nickname, base_currency,
		 first_seen_at, last_seen_at)
		 VALUES ('wallets', 'WALLET0001', 'crypto', 'Example Person', NULL, 'USD', 0, 0),
		        ('bank', 'CHK000001234', 'cash', 'Premier Checking', 'Example Nickname', 'USD', 0, 0),
		        ('bank', 'CHK000005678', 'cash', 'Example Home', 'Example Home', 'USD', 0, 0)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		 VALUES ('wallets', ?, 'WALLET0001', 'USD', 'current', 100), ('bank', ?, 'CHK000001234', 'USD', 'current', 200),
		        ('bank', ?, 'CHK000005678', 'USD', 'current', 300)`)
	accounts := func(flags ...string) string {
		t.Helper()
		args := append([]string{"-c", cfg, "holdings", "accounts", "-d", "2026-06-30", "-f", "csv",
			"-C", "silver_source,account,account_nickname"}, flags...)
		so, se, code := run(t, args...)
		if code != 0 {
			t.Fatalf("holdings accounts %v: exit %d: %s", flags, code, se)
		}
		return so
	}
	on := accounts("-p")
	for _, want := range []string{"wallets,******0001,\n", "bank,Premier Checking,nickname-1\n",
		"bank,********5678,nickname-2\n"} {
		if !strings.Contains(on, want) {
			t.Errorf("-p output lacks %q:\n%s", want, on)
		}
	}
	if strings.Contains(on, "Example") {
		t.Errorf("-p output shows a typed name:\n%s", on)
	}
	off := accounts()
	for _, want := range []string{"wallets,Example Person,\n", "bank,Premier Checking,Example Nickname\n",
		"bank,Example Home,Example Home\n"} {
		if !strings.Contains(off, want) {
			t.Errorf("output without -p lacks %q:\n%s", want, off)
		}
	}
}

// TestYieldAccountsSeriesPrivacyTypedLabel: a series repeats a view's
// privacy-mode value too, so a yield accounts series under -p prints a
// cointracking account's redacted id, not its name.
func TestYieldAccountsSeriesPrivacyTypedLabel(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	changeYieldGold(t, cfg,
		`UPDATE silver_sources SET silver_kind = 'cointracking' WHERE silver_source_id = 'brk'`,
		`UPDATE accounts SET display_name = 'Example Person' WHERE silver_source_id = 'brk'`)
	so, se, code := run(t, "-c", cfg, "yield", "accounts", "--period", "monthly", "2026-06-01", "2026-06-30",
		"-p", "-f", "csv", "-C", "period,silver_source,account")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if !strings.Contains(so, ",brk,****7781\n") || strings.Contains(so, "Example Person") {
		t.Errorf("yield accounts series under -p:\n%s", so)
	}
}

// setupTypedLabelGold seeds a gold whose accounts carry typed names: a
// cointracking wallet named like a person and keyed `cu_<user>:<label>`
// as the tracker keys it, under a portfolio of the same name; a manual
// account under a portfolio keyed by a word and named by nothing else; a
// brokerage and a cash account the bank names by their nicknames, the
// brokerage in a category the config's account overrides set and the
// cash account in a category of the bank's; a deposit account a bank
// names by its roster's nickname, which a config nickname replaces in
// gold; an AngelList account keyed by its holder's portal
// slug, with no name, which funds its stock; and a Plaid account with no
// official name, shown as the adapter shows it: by its institution and
// mask, with its own name as the nickname. The manual account holds a
// flat, a plot, a loan and a mortgage, each named and keyed by hand: the
// loan is private debt, so its name is its issuer's, and a declared rate
// gives it a calendar; the mortgage states no terms, so the yield
// coverage lists it. The plot leaves the book through an exit line keyed
// `exit:<key>`, as the manual adapter keys it. The gold holds what each
// report reads of them: snapshots at two dates, a coin, a stock and a
// bond with their lots, trades, a dividend, interest, a salary and
// purchases with their categories. Every name, id and figure is
// invented.
func setupTypedLabelGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Each $name in a statement is a day of 2026, in epoch seconds: $t0
	// and $t1 the snapshots, the others a transaction's.
	var days []string
	for _, d := range []struct {
		name  string
		month time.Month
		day   int
	}{{"t0", time.April, 30}, {"t1", time.June, 30}, {"may05", time.May, 5}, {"may09", time.May, 9},
		{"may10", time.May, 10}, {"may15", time.May, 15}, {"may20", time.May, 20}, {"may25", time.May, 25},
		{"jun01", time.June, 1}, {"jun10", time.June, 10}, {"jun12", time.June, 12}, {"jun14", time.June, 14},
		{"jun15", time.June, 15}} {
		days = append(days, "$"+d.name, strconv.FormatInt(time.Date(2026, d.month, d.day, 0, 0, 0, 0, time.UTC).Unix(), 10))
	}
	at := strings.NewReplacer(days...)
	const wallet = "'cu_000001:Example Person'"
	for _, q := range []string{
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('wallets', 'cointracking', '/tmp/wallets.db', -1, 0, 0), ('bank', 'schwab', '/tmp/bank.db', -1, 0, 0),
		        ('hand', 'manual', '/tmp/hand.db', -1, 0, 0), ('deposits', 'chase', '/tmp/deposits.db', -1, 0, 0),
		        ('venture', 'angellist', '/tmp/venture.db', -1, 0, 0), ('linked', 'plaid', '/tmp/linked.db', -1, 0, 0)`,
		`INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
		 VALUES ('wallets', 'cu_000001', 'Example Person', 0, 0), ('hand', 'example-house', NULL, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, nickname,
		 portfolio_external_id, tax_wrapper, base_currency, first_seen_at, last_seen_at) VALUES
		 ('wallets', ` + wallet + `, 'crypto', 'Example Person', NULL, 'cu_000001', 'taxable_personal', 'USD', 0, 0),
		 ('bank', 'BRK000001234', 'brokerage', 'Example Home', 'Example Home', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('bank', 'CHK000005678', 'cash', 'Example Home Cash', 'Example Home Cash', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('hand', 'HAND00001234', 'brokerage', 'Example Person', NULL, 'example-house', 'taxable_personal', 'USD', 0, 0),
		 ('deposits', 'SAV000009999', 'cash', 'Example Home Savings', 'Joint', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('venture', 'example-holder', 'brokerage', NULL, NULL, NULL, 'taxable_personal', 'USD', 0, 0),
		 ('linked', 'PLD000000001', 'cash', 'Example Bank …0000', 'Rainy Day Fund', NULL, 'taxable_personal', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, cusip, name,
		 currency, first_seen_at, last_seen_at) VALUES
		 ('wallets', 'BTC', 'crypto', 'physical', 'BTC', NULL, 'Bitcoin', 'USD', 0, 0),
		 ('bank', 'QSTK', 'public_equity', 'stock', 'QSTK', NULL, 'Qorvant Corp', 'USD', 0, 0),
		 ('bank', 'QBND', 'fixed_income', 'bond', NULL, '000000AB1', 'QORVANT CORP NOTE 4.00000% 06/30/2031', 'USD', 0, 0),
		 ('hand', 'QSTK', 'public_equity', 'stock', 'QSTK', NULL, 'Qorvant Corp', 'USD', 0, 0),
		 ('venture', 'QSTK', 'public_equity', 'stock', 'QSTK', NULL, 'Qorvant Corp', 'USD', 0, 0),
		 ('hand', 'example-flat', 'real_estate', 'physical', NULL, NULL, 'Example Flat', 'USD', 0, 0),
		 ('hand', 'example-plot', 'real_estate', 'physical', NULL, NULL, 'Example Plot', 'USD', 0, 0),
		 ('hand', 'zephyr-loan', 'private_debt', 'loan', NULL, NULL, 'Zephyr Alder Loan', 'USD', 0, 0),
		 ('hand', 'larch-mortgage', 'real_estate', 'mortgage', NULL, NULL, 'Larch Lane Mortgage', 'USD', 0, 0)`,
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, coupon_pct, frequency, maturity)
		 VALUES (1, 'hand', 'zephyr-loan', 6, 1, DATE '2029-06-30')`,
		`UPDATE accounts SET account_category = CASE account_external_id WHEN 'BRK000001234' THEN 'Example Trust'
		 ELSE 'Custody' END WHERE silver_source_id = 'bank'`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
		 ('wallets', $t0, ` + wallet + `, 'BTC', 'BTC', 'crypto', 'physical', 'USD', 1, 50000, 40000, 'stated', 'lots', 'included'),
		 ('wallets', $t1, ` + wallet + `, 'BTC', 'BTC', 'crypto', 'physical', 'USD', 1, 60000, 40000, 'stated', 'lots', 'included'),
		 ('bank', $t0, 'BRK000001234', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 2000, 1500, 'stated', 'lots', 'included'),
		 ('bank', $t1, 'BRK000001234', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 2100, 1500, 'stated', 'lots', 'included'),
		 ('bank', $t0, 'BRK000001234', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 10000, 10000, 9900, 'stated', 'lots', 'included'),
		 ('bank', $t1, 'BRK000001234', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 10000, 10000, 9900, 'stated', 'lots', 'included'),
		 ('hand', $t0, 'HAND00001234', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 5, 1000, 800, 'stated', 'lots', 'included'),
		 ('hand', $t1, 'HAND00001234', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 5, 1050, 800, 'stated', 'lots', 'included'),
		 ('venture', $t0, 'example-holder', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 2, 400, 300, 'stated', 'lots', 'included'),
		 ('venture', $t1, 'example-holder', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 2, 420, 300, 'stated', 'lots', 'included'),
		 ('hand', $t0, 'HAND00001234', 'example-flat', 'example-flat', 'real_estate', 'physical', 'USD', NULL, 300000, 250000,
		  'stated', 'paid_in', 'unknown'),
		 ('hand', $t1, 'HAND00001234', 'example-flat', 'example-flat', 'real_estate', 'physical', 'USD', NULL, 310000, 250000,
		  'stated', 'paid_in', 'unknown'),
		 ('hand', $t0, 'HAND00001234', 'example-plot', 'example-plot', 'real_estate', 'physical', 'USD', NULL, 40000, 30000,
		  'stated', 'paid_in', 'unknown'),
		 ('hand', $t0, 'HAND00001234', 'zephyr-loan', 'zephyr-loan', 'private_debt', 'loan', 'USD', NULL, 20000, 20000,
		  'stated', 'paid_in', 'unknown'),
		 ('hand', $t1, 'HAND00001234', 'zephyr-loan', 'zephyr-loan', 'private_debt', 'loan', 'USD', NULL, 20000, 20000,
		  'stated', 'paid_in', 'unknown'),
		 ('hand', $t0, 'HAND00001234', 'larch-mortgage', 'larch-mortgage', 'real_estate', 'mortgage', 'USD', NULL, -150000,
		  -150000, 'stated', 'paid_in', 'unknown'),
		 ('hand', $t1, 'HAND00001234', 'larch-mortgage', 'larch-mortgage', 'real_estate', 'mortgage', 'USD', NULL, -150000,
		  -150000, 'stated', 'paid_in', 'unknown')`,
		`INSERT INTO position_lots (silver_source_id, snapshot_at, account_external_id, position_key, lot_key,
		 currency, quantity, book_value, acquisition_date, term, basis_origin) VALUES
		 ('wallets', $t1, ` + wallet + `, 'BTC', 'L1', 'USD', 1, 40000, DATE '2025-01-02', 'long', 'stated'),
		 ('bank', $t1, 'BRK000001234', 'QSTK', 'L2', 'USD', 10, 1500, DATE '2025-01-02', 'long', 'stated')`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount, payload) VALUES
		 ('wallets', $t0, ` + wallet + `, 'USD', 'current', 100, NULL),
		 ('wallets', $t1, ` + wallet + `, 'USD', 'current', 100, NULL),
		 ('bank', $t0, 'BRK000001234', 'USD', 'current', 500, NULL),
		 ('bank', $t1, 'BRK000001234', 'USD', 'current', 500, NULL),
		 ('bank', $t0, 'CHK000005678', 'USD', 'current', 5000, '{"interest_rate": "1.5"}'),
		 ('bank', $t1, 'CHK000005678', 'USD', 'current', 5000, '{"interest_rate": "1.5"}'),
		 ('deposits', $t0, 'SAV000009999', 'USD', 'current', 700, NULL),
		 ('deposits', $t1, 'SAV000009999', 'USD', 'current', 700, NULL),
		 ('venture', $t0, 'example-holder', 'USD', 'current', 50, NULL),
		 ('venture', $t1, 'example-holder', 'USD', 'current', 80, NULL),
		 ('linked', $t0, 'PLD000000001', 'USD', 'current', 900, NULL),
		 ('linked', $t1, 'PLD000000001', 'USD', 'current', 880, NULL)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id,
		 instrument_external_id, kind, currency, net_amount, quantity, counterparty) VALUES
		 ('wallets', 'W-DEP', $may05, ` + wallet + `, NULL, 'deposit', 'USD', 1000, NULL, NULL),
		 ('wallets', 'W-BUY', $may10, ` + wallet + `, 'BTC', 'buy', 'USD', -1000, 0.02, NULL),
		 ('wallets', 'W-SELL', $jun10, ` + wallet + `, 'BTC', 'sell', 'USD', 1200, -0.02, NULL),
		 ('wallets', 'W-INT', $jun12, ` + wallet + `, NULL, 'interest', 'USD', 2, NULL, NULL),
		 ('wallets', 'W-SHOP', $jun14, ` + wallet + `, NULL, 'purchase', 'USD', -20, NULL, 'Corner Market'),
		 ('bank', 'B-DIV', $may15, 'BRK000001234', 'QSTK', 'dividend', 'USD', 30, NULL, NULL),
		 ('bank', 'B-BUY', $may20, 'BRK000001234', 'QSTK', 'buy', 'USD', -200, 1, NULL),
		 ('bank', 'B-SELL', $jun15, 'BRK000001234', 'QSTK', 'sell', 'USD', 250, -1, NULL),
		 ('bank', 'C-SALARY', $may25, 'CHK000005678', NULL, 'deposit', 'USD', 5000, NULL, 'Blue Harbour Payroll'),
		 ('bank', 'C-SHOP', $may09, 'CHK000005678', NULL, 'purchase', 'USD', -60, NULL, 'Corner Market'),
		 ('bank', 'C-INT', $jun01, 'CHK000005678', NULL, 'interest', 'USD', 5, NULL, NULL),
		 ('hand', 'exit:example-plot', $jun10, 'HAND00001234', 'example-plot', 'sell', 'USD', 45000, NULL, NULL),
		 ('venture', 'funding:F0001', $may05, 'example-holder', NULL, 'deposit', 'USD', 30, NULL, NULL),
		 ('linked', 'P-SHOP', $may09, 'PLD000000001', NULL, 'purchase', 'USD', -20, NULL, 'Corner Market')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature, signature_version,
		 income_detailed, provenance, assigned_at) VALUES
		 ('bank', 'C-SALARY', 'BLUE HARBOUR PAYROLL', 1, 'INCOME_SALARY', 'rule', 1),
		 ('bank', 'B-DIV', NULL, 1, NULL, 'signature-only', 1),
		 ('bank', 'C-INT', NULL, 1, NULL, 'signature-only', 1),
		 ('wallets', 'W-INT', NULL, 1, NULL, 'signature-only', 1)`,
		`INSERT INTO stated_periods (silver_source_id, account_external_id, as_of, period_start, currency,
		 ending_value, net_additions, source_sha256) VALUES
		 ('bank', 'BRK000001234', $t1, $jun01, 'USD', 12000, 0, 'doc'),
		 ('wallets', ` + wallet + `, $t1, $jun01, 'USD', 60000, 0, 'doc')`,
	} {
		if _, err := db.ExecContext(ctx, at.Replace(q)); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, src := range []struct{ source, txn string }{{"bank", "C-SHOP"}, {"wallets", "W-SHOP"}, {"linked", "P-SHOP"}} {
		enrichtest.SeedSpend(t, db, enrichtest.Spend{Source: src.source, Txn: src.txn, Signature: "CORNER MARKET",
			Detailed: "FOOD_AND_DRINK_GROCERIES", Provenance: "signature-only", At: 1})
	}
	if _, err := gold.RebuildLots(ctx, db, gold.LotsOptions{Config: lots.Config{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := gold.RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := gold.RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfg, []byte(`{"gold_db": "`+goldPath+`", "default_currency": "USD",
		"silver_sources": [{"id": "bank", "kind": "schwab", "path": "/tmp/bank.db"}],
		"account_overrides": {"bank": {"BRK000001234": {"category": "Example Trust"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// typedLabelCall is one view of a report tool over setupTypedLabelGold:
// the MCP call's arguments and the CLI's argv after the config, nil
// where the CLI has no -p for it.
type typedLabelCall struct {
	name string
	args map[string]any
	cli  []string
}

// typedLabelCalls are every view of every report tool the MCP tool table
// lists, every column, over the fixture's window: as of its last
// snapshot, or over its two months, or, for income's forward views,
// from today's month to two years on.
func typedLabelCalls(s *mcpServer, today time.Time) []typedLabelCall {
	fwdFrom := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	fwd := [2]string{fwdFrom.Format(time.DateOnly), fwdFrom.AddDate(2, 0, -1).Format(time.DateOnly)}
	var out []typedLabelCall
	for _, spec := range s.tools() {
		fam := spec.family
		if fam == nil {
			continue
		}
		for _, view := range fam.viewsOrOne() {
			c := typedLabelCall{name: strings.TrimSpace(spec.name + " " + view),
				args: map[string]any{"format": "csv", "columns": "all", "max_chars": maxIntegerParam}, cli: []string{spec.name}}
			if view != "" {
				c.args["view"] = view
				c.cli = append(c.cli, view)
			}
			window := [2]string{"2026-04-01", "2026-06-30"}
			if fam.view(view).forward() {
				window = fwd
			}
			if spec.name == "cashflow" && view == "projection" {
				window = [2]string{fwdFrom.AddDate(0, 1, 0).Format(time.DateOnly), fwd[1]}
			}
			// A view that refuses the window (returns private) runs without one.
			from, _ := time.Parse(time.DateOnly, window[0])
			to, _ := time.Parse(time.DateOnly, window[1])
			v := fam.view(view)
			refused := v != nil && v.checkWindow != nil && v.checkWindow(from.Unix(), to.Unix()+86399, today.Unix()) != nil
			switch {
			case refused:
			case fam.window == asOfDate:
				c.args["as_of"] = "2026-06-30"
				c.cli = append(c.cli, "-d", "2026-06-30")
			case fam.window == noWindow:
			default:
				c.args["from"], c.args["to"] = window[0], window[1]
				c.cli = append(c.cli, window[0], window[1])
			}
			c.cli = append(c.cli, "-f", "csv", "-p")
			switch {
			case spec.name == "status" || spec.name == "snapshots" || spec.name == "resolutions":
				c.cli = nil // no -p
			case spec.name == "categorizations" || spec.name == "holdings" && view == "global":
				// no -C
			default:
				c.cli = append(c.cli, "-C", "all")
			}
			out = append(out, c)
		}
	}
	return out
}

// TestPrivacyHidesTypedAccountNames renders every column of every view
// of every report tool with privacy on, over a gold whose accounts and
// holdings carry typed names (setupTypedLabelGold), on fixtureToday.
// Neither through the MCP privacy endpoint nor through the CLI's -p, on
// stdout or stderr, does a typed word appear anywhere, in any case: an
// account or portfolio name, a typed source's portfolio id, a manual
// exit line's id, an AngelList account's slug, a config category or a
// nickname. Wherever a cell shows such a word without privacy, the same
// cell with privacy on is a pseudonym of the word's prefix. A manual
// holding's name and key are no such word: -p hides money and
// identifiers, never what is held, so some view shows each of them with
// privacy on as it shows it without. Without privacy every view shows
// what its account and portfolio columns hide, so no view passes over
// rows that lack them, and some view shows each typed word.
func TestPrivacyHidesTypedAccountNames(t *testing.T) {
	t.Parallel()
	cfg := setupTypedLabelGold(t)
	// The typed names; the typed portfolios' names, the manual one shown
	// by its id; and their ids: the tracker's names its user, the manual
	// one is a word.
	typed := []string{"Example Person", "Example Home"}
	portfolioNames := []string{"Example Person", "example-house"}
	portfolioIDs := []string{"cu_000001", "example-house"}
	// Each typed word that prints as a pseudonym, with the pseudonym's
	// prefix: the portfolio ids, the AngelList account's slug, the
	// config's category, the nickname a config set and the Plaid
	// account's own name.
	pseudonymOf := map[string]string{"cu_000001": "cointracking", "example-house": "manual",
		"example-holder": "angellist", "Example Trust": "category", "Joint": "nickname",
		"Rainy Day Fund": "nickname"}
	hidden := slices.Concat(typed, slices.Sorted(maps.Keys(pseudonymOf)))
	// The manual holdings' names and keys, which some view shows alike
	// with privacy on and off, and the issuer the loan names.
	legible := []string{"Example Flat", "example-flat", "Zephyr Alder Loan", "zephyr-loan",
		"Larch Lane Mortgage", "larch-mortgage", "ZEPHYR ALDER LOAN"}
	var shown struct {
		sync.Mutex
		words, legible map[string]bool
	}
	shown.words, shown.legible = map[string]bool{}, map[string]bool{}
	leaks := func(out string, needles []string) []string {
		var found []string
		for _, w := range needles {
			if strings.Contains(strings.ToLower(out), strings.ToLower(w)) {
				found = append(found, w)
			}
		}
		return found
	}
	// cells checks that wherever a cell of plain shows a word with a
	// pseudonym, the same cell of out is a pseudonym of the word's
	// prefix. Both are csv of the same rows in the same order.
	cells := func(t *testing.T, name, plain, out string) {
		t.Helper()
		want, got := csvCells(t, plain), csvCells(t, out)
		if len(want) != len(got) {
			t.Fatalf("%s: %d rows with privacy, %d without", name, len(got), len(want))
		}
		shown.Lock()
		defer shown.Unlock()
		for i, row := range want {
			for j, cell := range row {
				for _, w := range legible {
					if strings.Contains(cell, w) && j < len(got[i]) && got[i][j] == cell {
						shown.legible[w] = true
					}
				}
				for w, prefix := range pseudonymOf {
					if !strings.Contains(strings.ToLower(cell), strings.ToLower(w)) {
						continue
					}
					shown.words[w] = true
					if j >= len(got[i]) || !regexp.MustCompile(`^`+prefix+`-\d+$`).MatchString(got[i][j]) {
						t.Errorf("%s: %s %q of row %d reads %q with privacy on, want a %s pseudonym", name, want[0][j],
							cell, i, got[i][j:min(j+1, len(got[i]))], prefix)
					}
				}
			}
		}
	}
	// The calls run as parallel subtests inside one group, so every one
	// has finished when the group returns and the shown words are read.
	// Each reads its own copy of the gold (copyFixture).
	t.Run("calls", func(t *testing.T) {
		for _, c := range typedLabelCalls(newTestMCP(cfg, true), fixtureToday) {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				cfg := copyFixture(t, cfg)
				tool, _, _ := strings.Cut(c.name, " ")
				if tool == "returns" && c.args["view"] == "private" {
					withPrivateLegs(t, cfg)
				}
				full, priv := onFixtureToday(newTestMCP(cfg, false)), onFixtureToday(newTestMCP(cfg, true))
				plain, err := callTool(t, full, tool, c.args)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				_, body, _ := strings.Cut(plain, "\n")
				head, _, _ := strings.Cut(body, "\n")
				names := strings.Split(head, ",")
				has := func(cols ...string) bool {
					return slices.ContainsFunc(cols, func(c string) bool { return slices.Contains(names, c) })
				}
				returnsAt := func(level string) bool { return tool == "returns" && c.args["view"] == level }
				if (has("account", "account_id") || returnsAt("accounts")) && len(leaks(body, typed)) == 0 {
					t.Errorf("%s: no typed name without privacy, which proves nothing:\n%s", c.name, body)
				}
				for _, w := range []struct {
					on      bool
					needles []string
				}{
					{has("portfolio") || returnsAt("portfolios"), portfolioNames},
					{has("portfolio_id", "portfolio_external_id") || returnsAt("portfolios"), portfolioIDs},
				} {
					if found := leaks(body, w.needles); w.on && len(found) != len(w.needles) {
						t.Errorf("%s: only %q of %q without privacy, which proves too little:\n%s", c.name, found, w.needles, body)
					}
				}
				got, err := callTool(t, priv, tool, c.args)
				if err != nil {
					t.Fatalf("%s (privacy): %v", c.name, err)
				}
				if found := leaks(got, hidden); found != nil {
					t.Errorf("%s: the privacy endpoint shows %q:\n%s", c.name, found, got)
				}
				_, gotBody, _ := strings.Cut(got, "\n")
				cells(t, c.name, body, gotBody)
				if c.cli == nil {
					return
				}
				so, se, code := runToday(t, append([]string{"-c", cfg}, c.cli...)...)
				if code != 0 {
					t.Fatalf("%s: CLI exit %d: %s", c.name, code, se)
				}
				if found := leaks(so+se, hidden); found != nil {
					t.Errorf("%s: -p shows %q:\n%s%s", c.name, found, so, se)
				}
				plainCLI, _, code := runToday(t, append([]string{"-c", cfg}, slices.DeleteFunc(slices.Clone(c.cli),
					func(a string) bool { return a == "-p" })...)...)
				if code != 0 {
					t.Fatalf("%s: CLI exit %d without -p", c.name, code)
				}
				cells(t, c.name+" (CLI)", plainCLI, so)
			})
		}
	})
	for w := range pseudonymOf {
		if !shown.words[w] {
			t.Errorf("no view shows %q without privacy, which proves nothing", w)
		}
	}
	for _, w := range legible {
		if !shown.legible[w] {
			t.Errorf("no view shows the holding's %q with privacy on as it does without", w)
		}
	}
}

// csvCells reads a csv result's header and rows, past any line that
// follows them, such as an empty result's note.
func csvCells(t *testing.T, out string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("csv: %v\n%s", err, out)
	}
	if len(recs) == 0 {
		return nil
	}
	table := recs[:1]
	for _, rec := range recs[1:] {
		if len(rec) == len(recs[0]) {
			table = append(table, rec)
		}
	}
	return table
}

// TestPseudonymsNumberByFirstAppearance pins the pseudonym map: a value
// reads as its prefix and the order it first appeared in among the
// prefix's values, the same value of one source reads the same every
// time, the same text of two sources reads apart, and a fresh map counts
// from one again. A blank value stays blank.
func TestPseudonymsNumberByFirstAppearance(t *testing.T) {
	t.Parallel()
	p := newPseudonyms()
	for _, c := range []struct{ prefix, source, value, want string }{
		{"manual", "hand", "example-flat", "manual-1"},
		{"manual", "hand", "example-plot", "manual-2"},
		{"manual", "hand", "example-flat", "manual-1"},
		// Two books of one kind may spell a key alike.
		{"manual", "hand2", "example-flat", "manual-3"},
		// Each prefix counts its own values, and a value under another
		// prefix is another name.
		{"category", "", "example-flat", "category-1"},
		{"category", "", "Example Trust", "category-2"},
		{"manual", "hand", "example-plot", "manual-2"},
		{"manual", "hand", "", ""},
	} {
		if got := p.of(c.prefix, c.source, c.value); got != c.want {
			t.Errorf("of(%q, %q, %q) = %q, want %q", c.prefix, c.source, c.value, got, c.want)
		}
	}
	if got := newPseudonyms().of("manual", "hand", "example-plot"); got != "manual-1" {
		t.Errorf("a fresh map names its first value %q, want manual-1", got)
	}
}

// TestPseudonymColumnRendersPerRun drives the class through rowsToTable:
// cells number by first appearance down the rows and across the
// columns, in every format, JSON keeping the column; a map shared by two
// renderings names a value alike in both, and a fresh one counts anew. A
// column with no Pseudonym prefixes by its name. A typed holding's key
// and name redact as an id: their words stay and an id in them masks.
// Without -p the values show.
func TestPseudonymColumnRendersPerRun(t *testing.T) {
	t.Parallel()
	type row struct{ source, key, name string }
	cols := []columnSpec[row]{
		{Name: "position_key", Privacy: PrivacyPseudonym,
			Pseudonym: func(r row) (string, string) { return "manual", r.source },
			Extract:   func(r row) string { return r.key }},
		{Name: "name", Privacy: PrivacyPseudonym, Extract: func(r row) string { return r.name }},
	}
	rows := []row{{"hand", "example-plot", "Example Plot"}, {"hand", "example-flat", ""},
		{"hand", "example-plot", "Example Plot"}}
	want := [][]string{{"manual-1", "name-1"}, {"manual-2", ""}, {"manual-1", "name-1"}}
	for _, f := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatJSON} {
		if got := rowsToTable(rows, cols, true, f, nil).Rows; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("%s: rows = %q, want %q", f, got, want)
		}
	}
	names := newPseudonyms()
	rowsToTable(rows[1:2], cols, true, output.FormatTable, names)
	if got := rowsToTable(rows, cols, true, output.FormatTable, names).Rows[1][0]; got != "manual-1" {
		t.Errorf("a shared map names example-flat %q in its second rendering, want manual-1", got)
	}
	if got := rowsToTable(rows, cols, false, output.FormatTable, nil).Rows[0]; !slices.Equal(got, []string{"example-plot", "Example Plot"}) {
		t.Errorf("without -p row 0 = %q, want it legible", got)
	}

	kinds := map[string]string{"hand": "manual", "hand2": "manual"}
	key, name := holdingCols(func(id string) string { return kinds[id] }, func(r row) string { return r.source },
		func(r row) string { return r.key }, func(r row) string { return r.name })
	books := []row{{"hand", "house", "Example House"}, {"hand2", "house 123456", "Example Barn"},
		{"bank", "QSTK", "Qorvant Corp"}, {"hand", "hut", "Example Hut, Invented Way 7, 1234 Nowhere"}}
	want = [][]string{{"house", "Example House"}, {"house ***456", "Example Barn"}, {"QSTK", "Qorvant Corp"},
		{"hut", "Example Hut, ******** *** *, **** *******"}}
	if got := rowsToTable(books, []columnSpec[row]{key, name}, true, output.FormatTable, nil).Rows; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("holdings of two books: rows = %q, want %q", got, want)
	}
}

// TestTypedHoldingNamesMaskTheirIDs: under -p a manual holding's name
// keeps its words and masks an id typed into it wherever the name
// prints: a holding's name column, an issuer named after it, a cash
// flow or a transaction line that names it. A year, a short number or a
// single id-shaped word in such a name stays, for a name is never one
// id. Any other holding's name prints as it is, digits and all. A key
// redacts as an account id, so the same id-shaped word masks there. A
// key with no public identifier masks its typed holding's id alone, its
// source id kept, and any other's whole.
func TestTypedHoldingNamesMaskTheirIDs(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"hand": "manual", "hand2": "manual"}
	kindOf := func(id string) string { return kinds[id] }
	cases := []struct{ source, name, want string }{
		{"hand", "Example Loan XY-1234-5678", "Example Loan **-****-5678"},
		{"hand", "Example Loan 12345678", "Example Loan ****5678"},
		{"hand", "Example Plot 2024 Lot 12", "Example Plot 2024 Lot 12"},
		{"hand", "Unit7", "Unit7"},
		{"hand2", "Example Barn", "Example Barn"},
		{"bank", "Qorvant 4.125% 15.01.2030", "Qorvant 4.125% 15.01.2030"},
		{"bank", "Qorvant Corp", "Qorvant Corp"},
	}

	type row struct{ source, key, name string }
	key, name := holdingCols(kindOf, func(r row) string { return r.source },
		func(r row) string { return r.key }, func(r row) string { return r.name })
	type keyed struct{ key, name string }
	keyCol, keyName := holdingKeyCols("security", kindOf, func(r keyed) string { return r.key },
		func(r keyed) string { return r.name })
	issuers := typedIssuers{}
	var issuerRows []gold.ExposureIssuerRow
	var flows []gold.CashflowTransactionRow
	var lines []gold.TransactionRow
	for _, c := range cases {
		if typedHolding(kindOf, c.source) {
			issuers[strings.ToUpper(c.name)] = true
		}
		issuerRows = append(issuerRows, gold.ExposureIssuerRow{Issuer: c.name})
		flows = append(flows, gold.CashflowTransactionRow{TxnLine: gold.TxnLine{SilverSourceID: c.source}, Name: &c.name})
		lines = append(lines, gold.TransactionRow{TxnLine: gold.TxnLine{SilverSourceID: c.source}, Name: &c.name})
	}
	pick := func(t *testing.T, name string, cols []string) int {
		t.Helper()
		if i := slices.Index(cols, name); i >= 0 {
			return i
		}
		t.Fatalf("no %s column in %q", name, cols)
		return -1
	}
	issuerCols := buildExposureIssuerColumns("USD", kindOf, issuers)
	flowCols := buildCashflowTransactionColumnRegistry("USD", kindOf, typedCategories{})
	lineCols := buildTransactionColumnRegistry("USD", kindOf, typedCategories{})
	for i, c := range cases {
		views := map[string]string{
			"holding name": rowsToTable([]row{{c.source, "k", c.name}}, []columnSpec[row]{name}, true, output.FormatTable, nil).Rows[0][0],
			"issuer":       rowsToTable(issuerRows[i:i+1], issuerCols[:1], true, output.FormatTable, nil).Rows[0][0],
			"cash flow line": rowsToTable(flows[i:i+1], []columnSpec[gold.CashflowTransactionRow]{
				flowCols[pick(t, "name", columnNames(flowCols))]}, true, output.FormatCSV, nil).Rows[0][0],
			"transaction line": rowsToTable(lines[i:i+1], []columnSpec[gold.TransactionRow]{
				lineCols[pick(t, "name", columnNames(lineCols))]}, true, output.FormatCSV, nil).Rows[0][0],
		}
		if c.source != "bank" {
			views["security name"] = rowsToTable([]keyed{{"holding:" + c.source + ":k", c.name}},
				[]columnSpec[keyed]{keyName}, true, output.FormatTable, nil).Rows[0][0]
		}
		for view, got := range views {
			want := c.want
			if view == "cash flow line" && c.source == "bank" {
				// Another source's line names whatever the statement says.
				want = freeTextPlaceholder
			}
			if got != want {
				t.Errorf("%s %q of %s: -p prints %q, want %q", view, c.name, c.source, got, want)
			}
		}
	}
	for _, c := range []struct{ key, want string }{
		{"holding:hand2:house 123456", "holding:hand2:house ***456"},
		{"holding:hand:example-flat", "holding:hand:example-flat"},
		{"holding:hand:unit7", "holding:hand:***t7"},
		{"holding:bank:QSTK-PRIVATE", freeTextPlaceholder},
		{"US0000000001", "US0000000001"},
	} {
		if got := rowsToTable([]keyed{{c.key, ""}}, []columnSpec[keyed]{keyCol}, true, output.FormatTable, nil).Rows[0][0]; got != c.want {
			t.Errorf("key %q: -p prints %q, want %q", c.key, got, c.want)
		}
	}
	for k, want := range map[string]string{"house 123456": "house ***456", "unit7": "***t7"} {
		if got := rowsToTable([]row{{"hand", k, "Example House"}}, []columnSpec[row]{key}, true,
			output.FormatTable, nil).Rows[0][0]; got != want {
			t.Errorf("position_key %q: -p prints %q, want %q", k, got, want)
		}
	}
}

// TestPrivacyPseudonymsHoldWithinARun: within one rendering a typed
// value reads one pseudonym. A manual holding's key and name are no such
// value and show as they do without -p; a wallet keyed by its label, and
// an AngelList account keyed by its slug, read alike in their account
// and account-id columns, and the wallet in every period of a series. A
// bank's category stays where the config's prints as a pseudonym. On
// the MCP privacy endpoint the pseudonym a page shows is the one a
// filter reads, whatever the call's filter or sort.
func TestPrivacyPseudonymsHoldWithinARun(t *testing.T) {
	t.Parallel()
	cfg := setupTypedLabelGold(t)
	cli := func(args ...string) []map[string]string {
		t.Helper()
		so, se, code := runToday(t, append(append([]string{"-c", cfg}, args...), "-p", "-f", "csv")...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, se)
		}
		return csvRows(t, so)
	}
	held := map[string]string{}
	for _, r := range cli("holdings", "positions", "-d", "2026-06-30", "-C", "silver_source,asset_class,position_key,name") {
		if r["silver_source"] == "hand" {
			held[r["position_key"]] = r["name"]
		}
	}
	if want := map[string]string{"QSTK": "Qorvant Corp", "example-flat": "Example Flat",
		"larch-mortgage": "Larch Lane Mortgage", "zephyr-loan": "Zephyr Alder Loan"}; !maps.Equal(held, want) {
		t.Errorf("the manual holdings read %q under -p, want %q", held, want)
	}
	categories, venture := map[string]bool{}, false
	for _, r := range cli("holdings", "accounts", "-d", "2026-06-30", "-C", "silver_source,account,account_id,account_category") {
		if r["silver_source"] == "wallets" && (r["account"] != r["account_id"] || !strings.HasPrefix(r["account"], "cointracking-")) {
			t.Errorf("wallet: account %q, account_id %q, want one cointracking pseudonym", r["account"], r["account_id"])
		}
		if r["silver_source"] == "venture" {
			venture = true
			if r["account"] != r["account_id"] || !strings.HasPrefix(r["account"], "angellist-") {
				t.Errorf("angellist: account %q, account_id %q, want one angellist pseudonym", r["account"], r["account_id"])
			}
		}
		if r["silver_source"] == "bank" {
			categories[r["account_category"]] = true
		}
	}
	if !venture {
		t.Error("holdings accounts shows no angellist account")
	}
	if !categories["Custody"] || !categories["category-1"] || len(categories) != 2 {
		t.Errorf("bank categories under -p = %v, want the bank's Custody and category-1", categories)
	}
	wallet := map[string]string{}
	for _, r := range cli("yield", "accounts", "--period", "monthly", "2026-05-01", "2026-06-30",
		"-C", "period,silver_source,account") {
		if r["silver_source"] == "wallets" {
			wallet[r["period"]] = r["account"]
		}
	}
	if len(wallet) != 2 || wallet["2026-05"] != wallet["2026-06"] || !strings.HasPrefix(wallet["2026-05"], "cointracking-") {
		t.Errorf("the wallet across a series reads %q, want one cointracking pseudonym in each of two periods", wallet)
	}

	priv := onFixtureToday(newTestMCP(cfg, true))
	// bySource calls for the typed accounts' names: the wallet's and the
	// AngelList account's, each a pseudonym.
	typedSources := []string{"venture", "wallets"}
	bySource := func(args map[string]any) map[string]string {
		t.Helper()
		args["view"], args["as_of"], args["format"] = "accounts", "2026-06-30", "csv"
		args["columns"] = "silver_source,account,base_currency"
		out, err := callTool(t, priv, "holdings", args)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(out, "\n")
		body, _, _ = strings.Cut(body, "\nshowing rows")
		names := map[string]string{}
		for _, r := range csvRows(t, body) {
			if slices.Contains(typedSources, r["silver_source"]) {
				names[r["silver_source"]] = r["account"]
			}
		}
		return names
	}
	names := bySource(map[string]any{})
	if len(names) != len(typedSources) || !strings.HasPrefix(names["wallets"], "cointracking-") ||
		!strings.HasPrefix(names["venture"], "angellist-") {
		t.Fatalf("the privacy endpoint names the typed accounts %q, want a pseudonym each", names)
	}
	for source, name := range names {
		if got := bySource(map[string]any{"account": name}); !maps.Equal(got, map[string]string{source: name}) {
			t.Errorf("account=%s keeps %q, want the %s account alone", name, got, source)
		}
	}
	for offset := range len(typedSources) {
		got := bySource(map[string]any{"source": strings.Join(typedSources, ","), "sort": "-account",
			"limit": 1, "offset": offset})
		if len(got) != 1 {
			t.Errorf("sorted, at offset %d: %q, want one row", offset, got)
		}
		for source, name := range got {
			if names[source] != name {
				t.Errorf("sorted, at offset %d: %s reads %q, want %q as unpaged", offset, source, name, names[source])
			}
		}
	}
}

// TestPseudonymsNumberAsTheDefaultOutput: a CLI run and an MCP call
// number pseudonyms alike (numberedCells): the default columns first,
// over every row, then the rest. So a value a default column prints
// takes the first number even where a column the defaults leave out
// comes before it in the registry, as it does in the CLI's output.
func TestPseudonymsNumberAsTheDefaultOutput(t *testing.T) {
	t.Parallel()
	type row struct{ alias, label string }
	typed := func(name string, get func(row) string) columnSpec[row] {
		return columnSpec[row]{Name: name, Privacy: PrivacyPseudonym, Extract: get,
			Pseudonym: func(row) (string, string) { return "manual", "" }}
	}
	registry := []columnSpec[row]{typed("alias", func(r row) string { return r.alias }),
		typed("label", func(r row) string { return r.label })}
	rows := []row{{"Example Alias", "Example Label"}, {"Other Alias", "Example Alias"}}
	rep := newReport(registry, []string{"label"}, func(context.Context, *sql.DB) ([]row, error) { return rows, nil })
	fetched, err := rep.fetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rs := newResultSet(rep, fetched, true)
	want := [][]string{{"manual-2", "manual-1"}, {"manual-3", "manual-2"}}
	if !slices.EqualFunc(rs.cells, want, slices.Equal) {
		t.Errorf("cells = %q, want %q", rs.cells, want)
	}
	if got := fetched.table(nil, []int{1}, true, output.FormatTable, rs.names).Rows; got[0][0] != "manual-1" ||
		got[1][0] != "manual-2" {
		t.Errorf("the default output reads %q, want manual-1 and manual-2", got)
	}
}
