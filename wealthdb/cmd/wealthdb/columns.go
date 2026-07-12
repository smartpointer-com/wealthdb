package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// columnSpec describes one named output column. T is the row
// type the Extract function takes — gold.PositionRow,
// gold.TransactionRow, gold.AccountRow, gold.PortfolioRow in this
// codebase.
type columnSpec[T any] struct {
	Name    string
	Header  string // empty ⇒ same as Name (use for dynamic headers like value_<CCY>)
	Align   output.Alignment
	Extract func(T) string
	// Privacy controls how the cell is redacted when the
	// caller runs in privacy mode (`-p` / `--privacy` on the
	// readout subcommands). PrivacyNone leaves the cell
	// unchanged.
	Privacy PrivacyClass
	// PrivacyFunc, when non-nil, replaces Privacy for per-row
	// decisions. Used where the right class depends on row
	// context — typically silver_source: e.g. UBS portfolio
	// labels ("Savings") are bank-assigned categories and stay
	// legible, while cointracking portfolio names are user-chosen
	// account identifiers and must be redacted regardless of
	// their character class.
	PrivacyFunc func(T) PrivacyClass
}

// PrivacyClass tags a column with the redaction shape applied
// when `-p` / `--privacy` is on. Mirrors what a banking-app
// privacy toggle does: hide identifying numbers, amounts, and
// share counts; leave taxonomy / dates / currency labels alone.
type PrivacyClass int

const (
	// PrivacyNone: never redacted (default).
	PrivacyNone PrivacyClass = iota
	// PrivacyAccountID: account / portfolio / relationship /
	// transaction identifiers. Redacted by redactAccountID —
	// stars in the middle, last 2-4 chars exposed (length-
	// dependent so very short IDs don't reveal too much), IBAN-
	// style 2-letter country code preserved at the front.
	// Purely-alphabetic strings pass through unredacted, since
	// at this layer they're typically bank-assigned taxonomic
	// labels (UBS "Savings" / "Brokerage").
	PrivacyAccountID
	// PrivacyCustomerLabel: user-chosen customer-identifying
	// strings — cointracking portfolio names, for example.
	// Redacted by the same shape as PrivacyAccountID but WITHOUT
	// the purely-alphabetic exemption: every alphanumeric string
	// of length ≥ 3 is treated as an identifier.
	PrivacyCustomerLabel
	// PrivacyQuantity: share counts. Rendered as "***" verbatim.
	PrivacyQuantity
	// PrivacyMoney: any monetary amount (prices, balances,
	// market values, net amounts, fees, etc.). Rendered as
	// "*****.**" verbatim.
	PrivacyMoney
)

// header returns the column's display label — Header if set,
// else Name. Used so registries can carry a stable lookup name
// while rendering a -x/--currency-suffixed header.
func (c columnSpec[T]) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

// resolveColumns implements the standard -C / --columns parsing
// shared by positions / accounts / portfolios / transactions:
//
//   - empty / "default" → the per-command defaults list
//   - "all"             → every registered column
//   - "+adds,...-removes,..." → delta against the defaults
//   - otherwise         → a comma-separated absolute list
//
// Unknown names produce a helpful error listing every column the
// command knows about.
func resolveColumns[T any](flagValue string, defaults []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	flagValue = strings.TrimSpace(flagValue)
	if adds, removes, isDelta := parseColumnsDelta(flagValue); isDelta {
		return columnsByName(applyColumnsDelta(defaults, adds, removes), registry)
	}
	switch flagValue {
	case "", "default":
		return columnsByName(defaults, registry)
	case "all":
		out := make([]columnSpec[T], len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return columnsByName(names, registry)
}

func columnsByName[T any](names []string, registry []columnSpec[T]) ([]columnSpec[T], error) {
	index := make(map[string]columnSpec[T], len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]columnSpec[T], 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinColumnNames[T any](registry []columnSpec[T]) string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// rowsToTable assembles an output.Table from a typed row slice
// and a column set. All four readout subcommands funnel their
// data through this on the way to the formatter. The `format`
// drives privacy mode's per-format behaviour:
//
//   - table        — redacted cells render as visible placeholders
//     ("****1234" / "***" / "*****.**") so the
//     eye can still scan rows.
//   - csv / csv_plain — quantity / money cells render empty
//     (",," between separators) so downstream
//     parsers see "missing" rather than the
//     literal asterisks. Account-ID columns
//     still render the visible placeholder so
//     row identity is preserved for grep / awk.
//   - json         — quantity / money columns are DROPPED from
//     the output entirely (the keys don't appear)
//     so JSON consumers see only fields they're
//     allowed to know. Account-ID columns still
//     surface the visible placeholder.
//
// `privacy` is false ⇒ no redaction regardless of format.
func rowsToTable[T any](rows []T, cols []columnSpec[T], privacy bool, format output.Format) output.Table {
	if privacy && format == output.FormatJSON {
		kept := make([]columnSpec[T], 0, len(cols))
		for _, c := range cols {
			if c.Privacy == PrivacyMoney || c.Privacy == PrivacyQuantity {
				continue
			}
			kept = append(kept, c)
		}
		cols = kept
	}
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Align
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cell := c.Extract(r)
			if privacy {
				class := c.Privacy
				if c.PrivacyFunc != nil {
					class = c.PrivacyFunc(r)
				}
				cell = applyPrivacy(cell, class, format)
				// Defence-in-depth: mask structured bank
				// identifiers wherever they appear, including in
				// PrivacyNone columns the per-column pass skips
				// (e.g. a UBS account number surfacing in
				// position_key / instrument).
				cell = scrubSensitiveIDs(cell)
			}
			cells[i] = cell
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}

// applyPrivacy is the dispatcher for the per-column redaction
// classes. Empty cells stay empty.
//
// Format-specific behaviour for Quantity / Money:
//   - table:     visible placeholder ("***" / "*****.**")
//   - csv*:      empty string ⇒ ",," between separators
//   - json:      empty string (the caller in rowsToTable has
//     already dropped the column from the JSON output
//     so this branch isn't reached for JSON, but
//     returning "" is the safe fallback).
//
// AccountID is always the visible placeholder — every format
// needs row-identity to remain scannable.
func applyPrivacy(value string, class PrivacyClass, format output.Format) string {
	if value == "" {
		return value
	}
	switch class {
	case PrivacyAccountID:
		return redactAccountID(value, false)
	case PrivacyCustomerLabel:
		return redactAccountID(value, true)
	case PrivacyQuantity:
		if format == output.FormatCSV || format == output.FormatCSVPlain {
			return ""
		}
		return "***"
	case PrivacyMoney:
		if format == output.FormatCSV || format == output.FormatCSVPlain {
			return ""
		}
		return "*****.**"
	}
	return value
}

// redactAccountID masks the middle of an identifier-shaped
// string, exposing only a length-appropriate trailing slice
// plus (for IBAN-style strings) the 2-letter country prefix.
//
// Rules:
//   - Strings with any non-alphanumeric character (spaces,
//     parens, slashes) pass through unchanged. Synthetic display
//     strings like "Portfolio overlay" / "(no portfolio)" /
//     "Brokerage (other)" stay legible.
//   - Strings that are alphanumeric-only AND contain at least
//     one digit are treated as bank-issued IDs and redacted.
//     Purely-alphabetic tokens (UBS portfolio LABELS like
//     "Savings" / "Brokerage") are left alone; their info
//     content is taxonomy, not identifier.
//   - Trailing-slice length scales down with the input so very
//     short IDs don't reveal a guessable amount:
//     length ≥ 8  → last 4
//     length 6-7  → last 3
//     length 4-5  → last 2
//     length ≤ 3  → fully starred
//   - Output length always matches the input. The redacted form
//     is visually a drop-in replacement for the original so
//     column widths and eyeball-scan shape are preserved
//     between privacy-on and privacy-off views.
//   - IBAN-shape (length 15-34, starts with two ASCII letters):
//     the 2-letter country code stays legible at the front, so
//     CH / DE / etc. is identifiable. The 15-34 bound matches
//     the real IBAN length range and excludes long alphanumeric
//     tokens (Schwab hashValues happen to be 64 hex chars and
//     can start with letters; those don't qualify as IBANs).
func redactAccountID(s string, forceAlpha bool) string {
	// Synthetic-suffix IDs (currently only the UBS-adapter
	// "<portfolio_id>:overlay" form): redact the ID portion,
	// preserve the suffix verbatim — it's a structural marker,
	// not an identifier component.
	if i := strings.IndexByte(s, ':'); i > 0 {
		return redactAccountID(s[:i], forceAlpha) + s[i:]
	}
	if !isLikelyAccountID(s, forceAlpha) {
		return s
	}
	n := len(s)
	if n <= 3 {
		return strings.Repeat("*", n)
	}
	suffixLen := 4
	switch {
	case n <= 5:
		suffixLen = 2
	case n <= 7:
		suffixLen = 3
	}
	suffix := s[n-suffixLen:]
	if n >= 15 && n <= 34 && isASCIILetter(s[0]) && isASCIILetter(s[1]) {
		return s[:2] + strings.Repeat("*", n-2-suffixLen) + suffix
	}
	return strings.Repeat("*", n-suffixLen) + suffix
}

// isLikelyAccountID matches the redactor's contract: at least 3
// chars, alphanumeric-only. With `forceAlpha=false` (the default
// PrivacyAccountID behaviour), additionally requires at least one
// digit — purely-alphabetic strings are bank-assigned taxonomic
// labels at this layer (UBS "Education" / "Authorized") and pass
// through. With `forceAlpha=true` (PrivacyCustomerLabel) the
// digit requirement is dropped — every alphanumeric token is
// treated as an identifier. Strings with any non-alphanumeric
// character (spaces, parens, slashes) pass through unchanged in
// both modes; synthetic display strings like "Portfolio overlay"
// / "(no portfolio)" stay legible.
func isLikelyAccountID(s string, forceAlpha bool) bool {
	if len(s) < 3 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return forceAlpha || hasDigit
}

func isASCIILetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// sensitiveIDPatterns are bank-identifier shapes that scrubSensitiveIDs
// masks wherever they appear in a rendered cell, independent of the
// column's PrivacyClass. Each entry pairs a tight shape regex with a
// masker that preserves the match's length + separators so column
// widths and scan-shape survive (matching redactAccountID's contract).
//
// This is a content-based defence-in-depth layer over the per-column
// privacy machinery. The per-column pass only redacts columns whose
// whole value IS an identifier (account_id, relationship_id, …); it
// can't reach an identifier that leaks inside a column that's
// PrivacyNone by design — notably position_key / symbol, which stay
// legible under -p because they normally hold public ISIN/ticker
// instrument ids. A UBS mortgage uses its (private) account number as
// the instrument identity, so it surfaces there raw; this pass catches
// it by shape regardless of which column carries it.
var sensitiveIDPatterns = []struct {
	re   *regexp.Regexp
	mask func(string) string
}{
	{
		// UBS banking relationship / account base: a 4-digit branch,
		// a space, then the 8-digit account base (e.g. the
		// relationship_id "<branch> <base>"). It also prefixes the
		// UBS mortgage account number ("<branch> <base>.MMM <n>").
		// The digits-space-digits shape is distinctive in holdings
		// data — ISINs/CUSIPs carry no internal space, monetary
		// amounts have decimal separators, valor numbers are shorter
		// — so matching by shape rarely false-positives. Keeps the
		// last 4 digits for cross-referencing; masks the rest.
		re:   regexp.MustCompile(`\b\d{4} \d{8}\b`),
		mask: func(m string) string { return "**** ****" + m[len(m)-4:] },
	},
}

// scrubSensitiveIDs masks structured bank identifiers wherever they
// appear in s. It runs on every cell under -p/--privacy as a final
// pass after applyPrivacy, so an identifier that leaks inside an
// otherwise-non-private column is caught even when that column's
// PrivacyClass is None.
func scrubSensitiveIDs(s string) string {
	for _, p := range sensitiveIDPatterns {
		s = p.re.ReplaceAllStringFunc(s, p.mask)
	}
	return s
}
