package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	dateparser "github.com/markusmobius/go-dateparser"
	dpdate "github.com/markusmobius/go-dateparser/date"
)

// Shared CLI-side date parsing. Powered by
// github.com/markusmobius/go-dateparser — handles ISO-8601, common
// short forms, and natural language ("yesterday", "last month",
// "last year", "2 weeks ago", "in 3 days"). A month named alongside
// a relative year ("January 1st last year") is NOT read: the
// vocabulary is whole-string, either absolute or relative, never
// half of each. English-only and UTC throughout; the silver
// databases don't carry timezone information, so honouring $TZ
// would just invite misalignment at the gold-load boundary.
//
// The library is the whole vocabulary, so a version bump can move a
// boundary without a line here changing — and every caller is a
// read-only report, which would print the moved window rather than
// fail. dates_test.go pins the forms against a fixed reference
// instant for that reason.

// dateparser.Parser caches locale data internally and is
// goroutine-safe, so one shared instance per process is the
// recommended pattern. The CLI is single-threaded but `sync.Once`
// is the right idiom for a lazy module-global.
var (
	parserOnce sync.Once
	parser     *dateparser.Parser
)

func getDateParser() *dateparser.Parser {
	parserOnce.Do(func() {
		parser = &dateparser.Parser{}
	})
	return parser
}

func newParseConfig(now time.Time) *dateparser.Configuration {
	return &dateparser.Configuration{
		Languages:       []string{"en"},
		DefaultTimezone: time.UTC,
		CurrentTime:     now.UTC(),
	}
}

// parseDate parses a single date string into a UTC time, anchored
// to the start (endOfPeriod=false) or end (endOfPeriod=true) of
// the parsed precision unit:
//
//	"2025-06-15"          → Day:   00:00 / 23:59 of that day
//	"2025-06"             → Month: 2025-06-01 00:00 / 2025-06-30 23:59
//	"2025"                → Year:  2025-01-01 00:00 / 2025-12-31 23:59
//	"yesterday"           → Day:   00:00 / 23:59 yesterday
//	"last month"          → Month: the whole month before this one
//	"2025-06-15 14:30:00" → Day:   00:00 / 23:59 of that day
//
// A time of day is read but not kept: the library reports Day
// precision even for a string carrying one, so the last form above
// is anchored like any other day. These windows are day-grained, so
// that is the right answer — but it is not what the input looks
// like, which is why it is spelled out.
//
// `now` is the reference point for relative inputs.
func parseDate(s string, now time.Time, endOfPeriod bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date string")
	}
	dt, err := getDateParser().Parse(newParseConfig(now), s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %q: %w", s, err)
	}
	if dt.IsZero() {
		return time.Time{}, fmt.Errorf("could not parse %q as a date", s)
	}
	t := dt.Time.UTC()
	switch dt.Period {
	case dpdate.Year:
		if endOfPeriod {
			return time.Date(t.Year(), 12, 31, 23, 59, 59, 0, time.UTC), nil
		}
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC), nil
	case dpdate.Month:
		if endOfPeriod {
			// First of next month minus one second = last second
			// of this month, regardless of length.
			next := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			return next.Add(-time.Second), nil
		}
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case dpdate.Day:
		return anchorToDay(t, endOfPeriod), nil
	default:
		// Hour-or-finer precision. Unreachable as the library
		// stands — it never reports finer than Day, even for an
		// input carrying a time — but the precision exists in its
		// enum, so a version that starts reporting one lands here
		// and keeps the instant rather than widening it to a day.
		return t, nil
	}
}

// anchorToDay returns t with the time-of-day set to either
// 00:00:00 (endOfPeriod=false) or 23:59:59 (endOfPeriod=true).
func anchorToDay(t time.Time, endOfPeriod bool) time.Time {
	t = t.UTC()
	if endOfPeriod {
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// parseAsOf is the convenience wrapper used by the point-in-time
// subcommands (`holdings positions -d`, `holdings accounts -d`, …).
// Empty string means "today, end-of-day"; any non-empty string
// is delegated to parseDate with endOfPeriod=true.
func parseAsOf(s string, now time.Time) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return anchorToDay(now, true).Unix(), nil
	}
	t, err := parseDate(s, now, true)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// parseDateRange interprets the positional args used by the
// range-style subcommands — `transactions`, and `returns` / `spending`
// through their own bare-invocation defaults. Returns
// [fromEpoch, toEpoch] Unix seconds inclusive.
//
//	0 args            → past 30 days
//	1 arg             → the parsed period's bounds: "2025" →
//	                    full year, "2025-06" / "last month" →
//	                    full month, "2025-06-15" / "yesterday"
//	                    → that day.
//	2 args            → explicit from/to. "-" on either side is
//	                    the open-ended sentinel (epoch on from,
//	                    today's end-of-day on to).
func parseDateRange(args []string, now time.Time) (int64, int64, error) {
	nowUTC := now.UTC()
	endOfNow := anchorToDay(nowUTC, true).Unix()

	switch len(args) {
	case 0:
		startOfToday := anchorToDay(nowUTC, false)
		from := startOfToday.AddDate(0, 0, -30).Unix()
		return from, endOfNow, nil
	case 1:
		from, err := parseDate(args[0], nowUTC, false)
		if err != nil {
			return 0, 0, err
		}
		to, err := parseDate(args[0], nowUTC, true)
		if err != nil {
			return 0, 0, err
		}
		return from.Unix(), to.Unix(), nil
	case 2:
		from, err := parseRangeBound(args[0], nowUTC, false)
		if err != nil {
			return 0, 0, fmt.Errorf("from: %w", err)
		}
		to, err := parseRangeBound(args[1], nowUTC, true)
		if err != nil {
			return 0, 0, fmt.Errorf("to: %w", err)
		}
		if from > to {
			return 0, 0, fmt.Errorf("from > to")
		}
		return from, to, nil
	default:
		return 0, 0, fmt.Errorf("expected 0, 1, or 2 date arguments; got %d", len(args))
	}
}

// parseRangeBound parses one side of a two-arg explicit range.
// "-" is the open-ended sentinel: epoch on the from side,
// today's end-of-day on the to side.
func parseRangeBound(s string, now time.Time, isTo bool) (int64, error) {
	if s == "-" {
		if isTo {
			return anchorToDay(now, true).Unix(), nil
		}
		return 0, nil
	}
	t, err := parseDate(s, now, isTo)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}
