package main

import (
	"testing"
	"time"
)

// Every date the CLI accepts is read by a third-party library, and
// every command that takes one is read-only — so a dependency bump
// that reinterprets an input moves a window silently and prints a
// confident wrong answer rather than failing. go-dateparser v1.4.3 did
// exactly that: it taught the relative parser to honour a leading
// sign, reversing "+1 year" from a year back to a year ahead. Nothing
// here noticed, because nothing here passed the library a string.
//
// These cases pin the vocabulary rather than the library: each is
// something a person can type and the instant it has to mean.

// dpNow is the instant every relative case below is read against. It
// matches the anchor the spending-window tests use, and sits
// mid-morning, mid-month and mid-year so that no boundary case passes
// by a coincidence of the anchor.
var dpNow = time.Date(2026, time.June, 15, 9, 30, 0, 0, time.UTC)

func dpAt(y int, m time.Month, d, hh, mm, ss int) time.Time {
	return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
}

type dpCase struct {
	in          string
	endOfPeriod bool
	want        time.Time
}

func runDPCases(t *testing.T, cases []dpCase) {
	t.Helper()
	for _, c := range cases {
		got, err := parseDate(c.in, dpNow, c.endOfPeriod)
		if err != nil {
			t.Errorf("parseDate(%q, endOfPeriod=%v): %v", c.in, c.endOfPeriod, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseDate(%q, endOfPeriod=%v) = %s, want %s",
				c.in, c.endOfPeriod, got.Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
	}
}

// TestParseDateAbsoluteForms pins the four grains the usage text
// advertises, at both ends of each. The two December / February cases
// are the ones that would catch a naive end-of-month: the month arm
// builds the first of the NEXT month and steps back a second, so
// month 12 has to roll the year and February has to follow the leap.
func TestParseDateAbsoluteForms(t *testing.T) {
	runDPCases(t, []dpCase{
		{"2025", false, dpAt(2025, time.January, 1, 0, 0, 0)},
		{"2025", true, dpAt(2025, time.December, 31, 23, 59, 59)},
		{"2025-06", false, dpAt(2025, time.June, 1, 0, 0, 0)},
		{"2025-06", true, dpAt(2025, time.June, 30, 23, 59, 59)},
		{"2025-12", true, dpAt(2025, time.December, 31, 23, 59, 59)},
		{"2025-02", true, dpAt(2025, time.February, 28, 23, 59, 59)},
		{"2024-02", true, dpAt(2024, time.February, 29, 23, 59, 59)},
		{"2025-06-15", false, dpAt(2025, time.June, 15, 0, 0, 0)},
		{"2025-06-15", true, dpAt(2025, time.June, 15, 23, 59, 59)},
		{"Jan 1 2025", false, dpAt(2025, time.January, 1, 0, 0, 0)},
		{"January 1, 2025", true, dpAt(2025, time.January, 1, 23, 59, 59)},
	})
}

// TestParseDateDiscardsTheTimeOfDay pins that a timestamped input is
// still read as its whole day. The library reports Day precision even
// for a string carrying an explicit time, so the time never survives —
// which is right for these commands, whose windows are day-grained,
// but is not what a reader would assume from the input.
func TestParseDateDiscardsTheTimeOfDay(t *testing.T) {
	runDPCases(t, []dpCase{
		{"2025-06-15 14:30:00", false, dpAt(2025, time.June, 15, 0, 0, 0)},
		{"2025-06-15 14:30:00", true, dpAt(2025, time.June, 15, 23, 59, 59)},
		{"2025-06-15T14:30:00Z", true, dpAt(2025, time.June, 15, 23, 59, 59)},
	})
}

// TestParseDateRelativeForms pins the natural language the doc comment
// offers. "last month" and "last year" keep their own grain — a whole
// month and a whole year — rather than collapsing to the day they land
// on, which is what makes them useful as a bare window argument.
func TestParseDateRelativeForms(t *testing.T) {
	runDPCases(t, []dpCase{
		{"today", false, dpAt(2026, time.June, 15, 0, 0, 0)},
		{"today", true, dpAt(2026, time.June, 15, 23, 59, 59)},
		{"yesterday", true, dpAt(2026, time.June, 14, 23, 59, 59)},
		{"2 weeks ago", false, dpAt(2026, time.June, 1, 0, 0, 0)},
		{"3 days ago", false, dpAt(2026, time.June, 12, 0, 0, 0)},
		{"in 3 days", false, dpAt(2026, time.June, 18, 0, 0, 0)},
		{"last month", false, dpAt(2026, time.May, 1, 0, 0, 0)},
		{"last month", true, dpAt(2026, time.May, 31, 23, 59, 59)},
		{"last year", false, dpAt(2025, time.January, 1, 0, 0, 0)},
		{"last year", true, dpAt(2025, time.December, 31, 23, 59, 59)},
	})
}

// TestParseDateSignedOffsets pins the direction of a bare offset, which
// is the boundary a dependency bump has already moved once: before
// go-dateparser v1.4.3 the sign was ignored and every offset read as
// the past, so "+1 year" meant a year BACK. An unsigned offset still
// reads as the past, which is what makes "2 weeks" a usable window.
func TestParseDateSignedOffsets(t *testing.T) {
	runDPCases(t, []dpCase{
		{"1 year", false, dpAt(2025, time.January, 1, 0, 0, 0)},
		{"-1 year", false, dpAt(2025, time.January, 1, 0, 0, 0)},
		{"+1 year", false, dpAt(2027, time.January, 1, 0, 0, 0)},
		{"2 months", false, dpAt(2026, time.April, 1, 0, 0, 0)},
		{"+2 months", false, dpAt(2026, time.August, 1, 0, 0, 0)},
		{"3 days", false, dpAt(2026, time.June, 12, 0, 0, 0)},
		{"+3 days", false, dpAt(2026, time.June, 18, 0, 0, 0)},
	})
}

// TestParseDateRejectsWhatItCannotRead pins that an unreadable input
// fails rather than resolving to something plausible — a date these
// commands silently invented would be worse than an error, since the
// answer would still print.
//
// The last two are forms a reader might expect to work and which the
// library does not support: a month named alongside a relative year.
// If a future bump starts reading them, this test is where that shows
// up, and the fix is to widen the documented vocabulary rather than to
// drop the case.
func TestParseDateRejectsWhatItCannotRead(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not a date at all",
		"1e400 years ago",
		"January 1st last year",
		"last January",
	} {
		if got, err := parseDate(in, dpNow, false); err == nil {
			t.Errorf("parseDate(%q) = %s, want an error", in, got.Format(time.RFC3339))
		}
	}
}

// TestParseAsOf pins the point-in-time wrapper: an empty argument is
// today through end-of-day, and anything else is its period's END —
// an as-of date has to include the day it names.
func TestParseAsOf(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"", dpAt(2026, time.June, 15, 23, 59, 59)},
		{"   ", dpAt(2026, time.June, 15, 23, 59, 59)},
		{"2025", dpAt(2025, time.December, 31, 23, 59, 59)},
		{"2025-06", dpAt(2025, time.June, 30, 23, 59, 59)},
		{"2025-06-15", dpAt(2025, time.June, 15, 23, 59, 59)},
		{"yesterday", dpAt(2026, time.June, 14, 23, 59, 59)},
	} {
		got, err := parseAsOf(c.in, dpNow)
		if err != nil {
			t.Errorf("parseAsOf(%q): %v", c.in, err)
			continue
		}
		if got != c.want.Unix() {
			t.Errorf("parseAsOf(%q) = %d (%s), want %d (%s)", c.in,
				got, time.Unix(got, 0).UTC().Format(time.RFC3339),
				c.want.Unix(), c.want.Format(time.RFC3339))
		}
	}
	if _, err := parseAsOf("not a date at all", dpNow); err == nil {
		t.Error("parseAsOf on an unreadable date returned no error")
	}
}

// TestParseDateRangeArity pins each arity of the positional window,
// including both open-ended sentinels. A one-argument window is the
// whole period the argument names, which is why the same string is
// parsed twice rather than once and reused.
func TestParseDateRangeArity(t *testing.T) {
	for _, c := range []struct {
		args     []string
		from, to time.Time
	}{
		{nil,
			dpAt(2026, time.May, 16, 0, 0, 0), dpAt(2026, time.June, 15, 23, 59, 59)},
		{[]string{"2025"},
			dpAt(2025, time.January, 1, 0, 0, 0), dpAt(2025, time.December, 31, 23, 59, 59)},
		{[]string{"2025-06"},
			dpAt(2025, time.June, 1, 0, 0, 0), dpAt(2025, time.June, 30, 23, 59, 59)},
		{[]string{"2025-06-15"},
			dpAt(2025, time.June, 15, 0, 0, 0), dpAt(2025, time.June, 15, 23, 59, 59)},
		{[]string{"2025-01-01", "2025-06-30"},
			dpAt(2025, time.January, 1, 0, 0, 0), dpAt(2025, time.June, 30, 23, 59, 59)},
		{[]string{"2025-01-01", "-"},
			dpAt(2025, time.January, 1, 0, 0, 0), dpAt(2026, time.June, 15, 23, 59, 59)},
	} {
		from, to, err := parseDateRange(c.args, dpNow)
		if err != nil {
			t.Errorf("parseDateRange(%v): %v", c.args, err)
			continue
		}
		if from != c.from.Unix() || to != c.to.Unix() {
			t.Errorf("parseDateRange(%v) = (%s, %s), want (%s, %s)", c.args,
				time.Unix(from, 0).UTC().Format(time.RFC3339),
				time.Unix(to, 0).UTC().Format(time.RFC3339),
				c.from.Format(time.RFC3339), c.to.Format(time.RFC3339))
		}
	}

	// The open-ended from-sentinel is the epoch itself, so it is
	// pinned apart from the table above rather than as a date.
	from, to, err := parseDateRange([]string{"-", "2025-06-30"}, dpNow)
	if err != nil {
		t.Fatalf(`parseDateRange(["-", "2025-06-30"]): %v`, err)
	}
	if from != 0 {
		t.Errorf("open-ended from = %d, want 0", from)
	}
	if want := dpAt(2025, time.June, 30, 23, 59, 59).Unix(); to != want {
		t.Errorf("to = %d, want %d", to, want)
	}
}

// TestParseDateRangeRejectsAnImpossibleWindow pins the two ways a
// window is refused: inverted, and more bounds than a range has.
func TestParseDateRangeRejectsAnImpossibleWindow(t *testing.T) {
	if _, _, err := parseDateRange([]string{"2025-06-30", "2025-01-01"}, dpNow); err == nil {
		t.Error("an inverted range returned no error")
	}
	if _, _, err := parseDateRange([]string{"2025", "2026", "2027"}, dpNow); err == nil {
		t.Error("three date arguments returned no error")
	}
}
