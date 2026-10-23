package main

import (
	"context"
	"encoding/csv"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
	off := rowsToTable(rows, cols, false, output.FormatTable, nil)
	if off.Rows[0][0] != "1234 00000001.MMM 0000" {
		t.Errorf("privacy off = %q, want it legible", off.Rows[0][0])
	}
	// privacy on: scrubbed even though the column is PrivacyNone.
	on := rowsToTable(rows, cols, true, output.FormatTable, nil)
	if on.Rows[0][0] != "**** ****0001.MMM 0000" {
		t.Errorf("privacy on = %q, want the account number masked", on.Rows[0][0])
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
// one of its source's nicknames prints as a pseudonym; a bank's label
// stays.
func TestCashflowCoverageTypedLabelMasks(t *testing.T) {
	t.Parallel()
	kinds := map[string]string{"wallets": "cointracking", "bank": "chase", "venture": "angellist"}
	nicknames := accountNicknames{"bank": {"CHK000001234": "Example Home"}}
	cols, err := resolveColumns("silver_source,account", nil,
		buildCashflowCoverageColumnRegistry("monthly", func(id string) string { return kinds[id] }, nicknames))
	if err != nil {
		t.Fatal(err)
	}
	rows := []gold.CashflowCoverageRow{{SourceID: "wallets", Account: "Example Person"},
		{SourceID: "bank", Account: "Premier Checking"}, {SourceID: "bank", Account: "Example Home"},
		{SourceID: "venture", Account: "example-holder"}}
	for privacy, want := range map[bool][]string{
		true:  {"cointracking-1", "Premier Checking", "nickname-1", "angellist-1"},
		false: {"Example Person", "Premier Checking", "Example Home", "example-holder"},
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
// cash account in a category of the bank's; and a deposit account a bank
// names by its roster's nickname, which a config nickname has since
// replaced in gold; and an AngelList account keyed by its holder's
// portal slug, with no name, which funds its stock. The manual account
// holds a flat, a plot, a loan and
// a mortgage, each named and keyed by hand: the loan is private debt, so
// its name is its issuer's, and a declared rate gives it a calendar; the
// mortgage states no terms, so the yield coverage lists it. The plot
// leaves the book through an exit line keyed `exit:<key>`, as the manual
// adapter keys it. The gold holds what each report reads of them:
// snapshots at two dates, a coin, a stock and a bond with their lots,
// trades, a dividend, interest, a salary and a purchase with their
// categories. Every name, id and figure is invented.
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
		        ('venture', 'angellist', '/tmp/venture.db', -1, 0, 0)`,
		`INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
		 VALUES ('wallets', 'cu_000001', 'Example Person', 0, 0), ('hand', 'example-house', NULL, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, nickname,
		 portfolio_external_id, tax_wrapper, base_currency, first_seen_at, last_seen_at) VALUES
		 ('wallets', ` + wallet + `, 'crypto', 'Example Person', NULL, 'cu_000001', 'taxable_personal', 'USD', 0, 0),
		 ('bank', 'BRK000001234', 'brokerage', 'Example Home', 'Example Home', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('bank', 'CHK000005678', 'cash', 'Example Home Cash', 'Example Home Cash', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('hand', 'HAND00001234', 'brokerage', 'Example Person', NULL, 'example-house', 'taxable_personal', 'USD', 0, 0),
		 ('deposits', 'SAV000009999', 'cash', 'Example Home Savings', 'Joint', NULL, 'taxable_personal', 'USD', 0, 0),
		 ('venture', 'example-holder', 'brokerage', NULL, NULL, NULL, 'taxable_personal', 'USD', 0, 0)`,
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
		 ('bank', $t0, 'CHK000005678', 'USD', 'current', 5000, '{"CdtIntrstRate1": "1.5", "CdtLmtAmt1": "0"}'),
		 ('bank', $t1, 'CHK000005678', 'USD', 'current', 5000, '{"CdtIntrstRate1": "1.5", "CdtLmtAmt1": "0"}'),
		 ('deposits', $t0, 'SAV000009999', 'USD', 'current', 700, NULL),
		 ('deposits', $t1, 'SAV000009999', 'USD', 'current', 700, NULL),
		 ('venture', $t0, 'example-holder', 'USD', 'current', 50, NULL),
		 ('venture', $t1, 'example-holder', 'USD', 'current', 80, NULL)`,
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
		 ('venture', 'funding:F0001', $may05, 'example-holder', NULL, 'deposit', 'USD', 30, NULL, NULL)`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature, signature_version,
		 income_detailed, provenance, assigned_at) VALUES
		 ('bank', 'C-SALARY', 'BLUE HARBOUR PAYROLL', 1, 'INCOME_SALARY', 'rule', 1),
		 ('bank', 'B-DIV', NULL, 1, NULL, 'signature-only', 1),
		 ('bank', 'C-INT', NULL, 1, NULL, 'signature-only', 1),
		 ('wallets', 'W-INT', NULL, 1, NULL, 'signature-only', 1)`,
		`INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id, merchant_signature, signature_version,
		 spend_detailed, provenance, far_silver_source_id, far_account_external_id, far_class, assigned_at) VALUES
		 ('bank', 'C-SHOP', 'CORNER MARKET', 1, 'FOOD_AND_DRINK_GROCERIES', 'signature-only', NULL, NULL, NULL, 1),
		 ('wallets', 'W-SHOP', 'CORNER MARKET', 1, 'FOOD_AND_DRINK_GROCERIES', 'signature-only', NULL, NULL, NULL, 1)`,
	} {
		if _, err := db.ExecContext(ctx, at.Replace(q)); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
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
		views := fam.views
		if views == nil {
			views = []string{""}
		}
		for _, view := range views {
			c := typedLabelCall{name: strings.TrimSpace(spec.name + " " + view),
				args: map[string]any{"format": "csv", "columns": "all", "limit": 0}, cli: []string{spec.name}}
			if view != "" {
				c.args["view"] = view
				c.cli = append(c.cli, view)
			}
			window := [2]string{"2026-04-01", "2026-06-30"}
			if spec.name == "income" && isForwardView(view) {
				window = fwd
			}
			switch fam.window {
			case asOfDate:
				c.args["as_of"] = "2026-06-30"
				c.cli = append(c.cli, "-d", "2026-06-30")
			case noWindow:
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
// holding's name or key (an issuer upper-cases one), an exit line's id,
// an AngelList account's slug, a config category or a nickname. Wherever a cell shows such a word
// without privacy, the same cell with privacy on is a pseudonym of the
// word's prefix. Without privacy every view shows what its account and
// portfolio columns hide, so no view passes over rows that lack them,
// and some view shows each typed word.
func TestPrivacyHidesTypedAccountNames(t *testing.T) {
	t.Parallel()
	cfg := setupTypedLabelGold(t)
	full, priv := onFixtureToday(newTestMCP(cfg, false)), onFixtureToday(newTestMCP(cfg, true))
	// The typed names; the typed portfolios' names, the manual one shown
	// by its id; and their ids: the tracker's names its user, the manual
	// one is a word.
	typed := []string{"Example Person", "Example Home"}
	portfolioNames := []string{"Example Person", "example-house"}
	portfolioIDs := []string{"cu_000001", "example-house"}
	// Each typed word that prints as a pseudonym, with the pseudonym's
	// prefix: the portfolio ids, the manual holdings' names and keys (the
	// exit line's id holds the plot's), the AngelList account's slug, the
	// config's category and the nickname a config set.
	pseudonymOf := map[string]string{"cu_000001": "cointracking", "example-house": "manual",
		"Example Flat": "manual", "example-flat": "manual", "Example Plot": "manual", "example-plot": "manual",
		"Zephyr Alder Loan": "manual", "zephyr-loan": "manual", "Larch Lane Mortgage": "manual",
		"larch-mortgage": "manual", "example-holder": "angellist", "Example Trust": "category", "Joint": "nickname"}
	hidden := slices.Concat(typed, slices.Sorted(maps.Keys(pseudonymOf)))
	shown := map[string]bool{}
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
	cells := func(name, plain, out string) {
		t.Helper()
		want, got := csvCells(t, plain), csvCells(t, out)
		if len(want) != len(got) {
			t.Fatalf("%s: %d rows with privacy, %d without", name, len(got), len(want))
		}
		for i, row := range want {
			for j, cell := range row {
				for w, prefix := range pseudonymOf {
					if !strings.Contains(strings.ToLower(cell), strings.ToLower(w)) {
						continue
					}
					shown[w] = true
					if j >= len(got[i]) || !regexp.MustCompile(`^`+prefix+`-\d+$`).MatchString(got[i][j]) {
						t.Errorf("%s: %s %q of row %d reads %q with privacy on, want a %s pseudonym", name, want[0][j],
							cell, i, got[i][j:min(j+1, len(got[i]))], prefix)
					}
				}
			}
		}
	}
	for _, c := range typedLabelCalls(priv, fixtureToday) {
		tool, _, _ := strings.Cut(c.name, " ")
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
		cells(c.name, body, gotBody)
		if c.cli == nil {
			continue
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
		cells(c.name+" (CLI)", plainCLI, so)
	}
	for w := range pseudonymOf {
		if !shown[w] {
			t.Errorf("no view shows %q without privacy, which proves nothing", w)
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
// and name read as one pseudonym, apart from another book's holding of
// the same key. Without -p the values show.
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
	books := []row{{"hand", "house", "Example House"}, {"hand2", "house", "Example Barn"}, {"bank", "QSTK", "Qorvant Corp"}}
	want = [][]string{{"manual-1", "manual-1"}, {"manual-2", "manual-2"}, {"QSTK", "Qorvant Corp"}}
	if got := rowsToTable(books, []columnSpec[row]{key, name}, true, output.FormatTable, nil).Rows; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("holdings of two books: rows = %q, want %q", got, want)
	}
}

// TestPrivacyPseudonymsHoldWithinARun: within one rendering a typed
// value reads one pseudonym. A manual holding's key and name read alike
// and apart from the other holdings'; a wallet keyed by its label, and
// an AngelList account keyed by its slug, read alike in their account
// and account-id columns, and the wallet in every period of a series. A bank's category stays where the config's prints as a
// pseudonym. On the MCP privacy endpoint the pseudonym a page shows is
// the one a filter reads, whatever the call's filter or sort.
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
	pseudonym := regexp.MustCompile(`^manual-\d+$`)
	held := map[string]string{}
	for _, r := range cli("holdings", "positions", "-d", "2026-06-30", "-C", "silver_source,asset_class,position_key,name") {
		if r["silver_source"] != "hand" {
			continue
		}
		if !pseudonym.MatchString(r["position_key"]) || r["name"] != r["position_key"] {
			t.Errorf("manual %s holding: position_key %q, name %q, want one manual pseudonym", r["asset_class"],
				r["position_key"], r["name"])
		}
		held[r["asset_class"]] = r["position_key"]
	}
	if len(held) != 3 || len(slices.Compact(slices.Sorted(maps.Values(held)))) != 3 {
		t.Errorf("the manual holdings read %q, want three pseudonyms", held)
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
	// byClass calls for the manual holdings' keys, by asset class.
	byClass := func(args map[string]any) map[string]string {
		t.Helper()
		args["view"], args["as_of"], args["format"] = "positions", "2026-06-30", "csv"
		args["columns"] = "silver_source,asset_class,position_key"
		out, err := callTool(t, priv, "holdings", args)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(out, "\n")
		body, _, _ = strings.Cut(body, "\nshowing rows")
		keys := map[string]string{}
		for _, r := range csvRows(t, body) {
			if r["silver_source"] == "hand" {
				keys[r["asset_class"]] = r["position_key"]
			}
		}
		return keys
	}
	keys := byClass(map[string]any{})
	flat := keys["real_estate"]
	if !pseudonym.MatchString(flat) {
		t.Fatalf("the flat reads %q on the privacy endpoint, want a manual pseudonym", flat)
	}
	if got := byClass(map[string]any{"symbol": flat}); !maps.Equal(got, map[string]string{"real_estate": flat}) {
		t.Errorf("symbol=%s keeps %q, want the flat alone", flat, got)
	}
	for offset := range 2 {
		got := byClass(map[string]any{"source": "hand", "sort": "-position_key", "limit": 1, "offset": offset})
		if len(got) != 1 {
			t.Errorf("sorted, at offset %d: %q, want one row", offset, got)
		}
		for class, key := range got {
			if keys[class] != key {
				t.Errorf("sorted, at offset %d: %q, want one of %q", offset, got, keys)
			}
		}
	}
}
