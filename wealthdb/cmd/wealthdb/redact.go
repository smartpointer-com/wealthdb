package main

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

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
// needs row-identity to remain scannable. FreeText is likewise
// format-independent, but it masks the whole value: there is no
// safe slice of a narrative to expose, since the identifying part
// can sit anywhere in it. A Pseudonym cell takes its name from the
// run's pseudonyms, which rowsToTable holds and applies before this; a
// Pseudonym cell that reaches here all the same masks whole, so a
// caller without the pseudonyms fails closed.
func applyPrivacy(value string, class PrivacyClass, format output.Format) string {
	if value == "" {
		return value
	}
	switch class {
	case PrivacyAccountID:
		return redactAccountID(value)
	case PrivacyFreeText, PrivacyPseudonym:
		return freeTextPlaceholder
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

// freeTextPlaceholder is what a PrivacyFreeText cell renders as —
// the same fixed star form PrivacyQuantity uses, so privacy mode
// keeps one visual vocabulary instead of a per-class one.
const freeTextPlaceholder = "***"

// pseudonyms are the names -p gives the values a person typed, in one
// rendering: a CLI run, or one MCP call's result set. A value prints as
// its prefix and a number, `manual-1`, counted per prefix in the order
// the values first appear. So the same value of one source reads the
// same in every row and column, and two values, or one text of two
// sources, read apart. The numbers follow the rendering's rows: another
// run may number the same value apart.
type pseudonyms struct {
	names map[[3]string]string
	count map[string]int
	given map[string]bool // the names given out, in normalizeCell's form
}

func newPseudonyms() *pseudonyms {
	return &pseudonyms{names: map[[3]string]string{}, count: map[string]int{}, given: map[string]bool{}}
}

// of is the pseudonym under prefix of value, the words of source (blank
// for a value that reads alike across sources); a blank value stays
// blank.
func (p *pseudonyms) of(prefix, source, value string) string {
	if value == "" {
		return ""
	}
	key := [3]string{prefix, source, value}
	if name, ok := p.names[key]; ok {
		return name
	}
	p.count[prefix]++
	name := prefix + "-" + strconv.Itoa(p.count[prefix])
	p.names[key] = name
	p.given[normalizeCell(name)] = true
	return name
}

// named reports whether s, in normalizeCell's form, is a name the set
// has given out (of).
func (p *pseudonyms) named(s string) bool { return p.given[s] }

// idJoints are the characters a composite identifier joins its parts
// with: `<id>:<iban>:<id>`, `<id>@<iban>`, `<digits>#<hash>`. Each part
// redacts on its own, so each id keeps its own tail.
const idJoints = ":@#"

// redactAccountID hides every identifier in a PrivacyAccountID value
// and leaves the rest legible. The value splits at idJoints, and each
// part reads alone:
//
//   - A part wholly id-shaped (isLikelyAccountID) is one id, masked by
//     maskID. So is a plain id, the value being its only part.
//   - Any other part reads as runs (redactIDRuns). So a word stays, as
//     in the structural suffix of `<id>:overlay` and the label
//     "Brokerage (other)", and an id spaced or punctuated still masks.
//
// Output length always matches the input, so column widths and the
// eyeball-scan shape hold between privacy-on and privacy-off views.
func redactAccountID(s string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(idJoints, s[i]) >= 0 {
			b.WriteString(redactIDPart(s[start:i]))
			b.WriteByte(s[i])
			start = i + 1
		}
	}
	b.WriteString(redactIDPart(s[start:]))
	return b.String()
}

// redactIDPart masks one part of a composite value: as one id where it
// is wholly id-shaped, else by its runs.
func redactIDPart(s string) string {
	if isLikelyAccountID(s) {
		return maskID(s)
	}
	return redactIDRuns(s)
}

// maxLegibleDigits is the most digits a run keeps legible. Four digits
// are what -p shows of any id, so masking them hides nothing, and they
// are what a label's year or plan number holds: `Portfolio 2024`,
// `401(k)`.
const maxLegibleDigits = 4

// minMixedIDLen is the length from which one group of letters and
// digits together is an id however few digits it holds, as a token
// beside a padding `=` is: the length at which a plain id shows its
// full tail.
const minMixedIDLen = 8

// idGroup is a stretch of letters and digits, by its byte offsets.
type idGroup struct {
	start, end, chars, digits int
}

// mixedID reports whether the group alone is an id (minMixedIDLen).
func (g idGroup) mixedID() bool {
	return g.digits > 0 && g.digits < g.chars && g.chars >= minMixedIDLen
}

// redactIDRuns masks the runs of s that can be an id. A run is groups
// of letters and digits joined by separators:
//
//   - Two groups join through a dash or a dot, as in `XXX-######`.
//   - A digit-bearing group joins a run that holds a digit through any
//     other separator, as in `#### #### ####`, unless that separator
//     ends a clause (endsClause).
//   - Nothing else joins, so a letter group never joins through a
//     space, and `Mortgage 12345678` keeps its word.
//
// A run holding more than maxLegibleDigits digits is one id (maskID),
// and so is a run holding a mixedID group.
func redactIDRuns(s string) string {
	groups := idGroups(s)
	var out []byte
	done := 0
	for first := 0; first < len(groups); {
		last, digits, mixed := first, groups[first].digits, groups[first].mixedID()
		for last+1 < len(groups) {
			next := groups[last+1]
			sep := s[groups[last].end:next.start]
			if sep != "-" && sep != "." && (next.digits == 0 || digits == 0 || endsClause(sep)) {
				break
			}
			last++
			digits += next.digits
			mixed = mixed || next.mixedID()
		}
		if digits > maxLegibleDigits || mixed {
			start, end := groups[first].start, groups[last].end
			out = append(append(out, s[done:start]...), maskID(s[start:end])...)
			done = end
		}
		first = last + 1
	}
	return string(append(out, s[done:]...))
}

// endsClause reports whether a separator closes one phrase of a label
// and opens the next: no id is spaced that way. A street number and a
// postal code read so are no id, and the scrub masks them as an address.
func endsClause(sep string) bool {
	return strings.Contains(sep, ", ") || strings.Contains(sep, "; ")
}

// idGroups lists the groups of letters and digits in s.
func idGroups(s string) []idGroup {
	var groups []idGroup
	open := false
	for i, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			open = false
			continue
		}
		if !open {
			groups = append(groups, idGroup{start: i})
			open = true
		}
		g := &groups[len(groups)-1]
		g.end = i + utf8.RuneLen(r)
		g.chars++
		if unicode.IsDigit(r) {
			g.digits++
		}
	}
	return groups
}

// maskID stars one id's letters and digits, keeping its separators and
// a slice of its characters. The slice scales down with the id, so a
// very short id does not reveal a guessable amount:
//
//	8 or more → the last 4
//	6 or 7    → the last 3
//	4 or 5    → the last 2
//	3 or less → none
//
// An IBAN-shaped id, 15 to 34 characters starting with two ASCII
// letters, also keeps its country code, so CH or DE stays readable.
// The bound matches the real IBAN length range and excludes long tokens
// such as Schwab's 64-character hashValues, which can start with
// letters.
func maskID(s string) string {
	chars := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			chars++
		}
	}
	tail := 4
	switch {
	case chars <= 3:
		tail = 0
	case chars <= 5:
		tail = 2
	case chars <= 7:
		tail = 3
	}
	head := 0
	if chars >= 15 && chars <= 34 && startsWithTwoLetters(s) {
		head = 2
	}
	seen := 0
	return strings.Map(func(r rune) rune {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return r
		}
		seen++
		if seen <= head || seen > chars-tail {
			return r
		}
		return '*'
	}, s)
}

// startsWithTwoLetters reports whether an id's first two letters and
// digits are ASCII letters: an IBAN's country code, spaced or not.
func startsWithTwoLetters(s string) bool {
	seen := 0
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		if r >= utf8.RuneSelf || !isASCIILetter(byte(r)) {
			return false
		}
		if seen++; seen == 2 {
			return true
		}
	}
	return false
}

// isLikelyAccountID reports whether a value is wholly id-shaped: at
// least 3 chars, alphanumeric-only, and containing at least one digit.
// Purely-alphabetic strings are bank-assigned taxonomic labels at this
// layer (UBS "Savings" / "Brokerage"). A value that fails this test can
// still hold an id, which redactAccountID finds by its runs.
//
// The shape test is deliberately narrow, and stays that way:
// accountRef.lineClass reads it to decide whether an id of a typed
// source redacts as an id or prints as a pseudonym, and redactIDPart to
// mask a wholly id-shaped part as one id. A value that needs hiding
// whatever its shape belongs to a class that asks no shape question at
// all, rather than to a widened test here: a statement narrative to
// PrivacyFreeText, a customer-chosen label to PrivacyPseudonym.
func isLikelyAccountID(s string) bool {
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
	return hasDigit
}

func isASCIILetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
