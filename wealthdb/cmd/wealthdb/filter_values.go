package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// fixableValue is a value that does not read, with the plain forms that
// would.
type fixableValue struct {
	why   string
	fixes []string
}

func (e fixableValue) Error() string { return e.why }

var (
	plainNumberRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)$`)
	groupedRe     = regexp.MustCompile(`^[+-]?\d{1,3}(,\d{3})+(\.\d+)?$`)
	scaledRe      = regexp.MustCompile(`(?i)^([+-]?\d+(\.\d+)?)\s*(k|thousand|m|mm|mn|million|bn|b|billion)$`)
	unitRe        = regexp.MustCompile(`(?i)^(?:([a-z]{3})\s+)?([+-]?[\d,]+(\.\d+)?)\s*(%|percent|bps?|[a-z]{3}|dollars?|francs?|euros?)?$`)
)

var numberWords = map[string]int{"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "twenty": 20, "fifty": 50, "hundred": 100, "a hundred": 100, "one hundred": 100,
	"thousand": 1000, "a thousand": 1000, "one thousand": 1000, "million": 1000000, "a million": 1000000, "one million": 1000000}

// parseNumberValue reads a number or money value: a plain number, or
// one with a currency sign or thousands commas, or a percent sign on a
// percentage column, all echoed plain. A number in words, a scale such
// as 1k, or a unit is refused with its plain form.
func parseNumberValue(v string, pct bool) (float64, string, error) {
	s := strings.TrimSpace(strings.ReplaceAll(v, "−", "-"))
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	body = strings.TrimLeft(body, "$€£")
	if neg {
		body = "-" + body
	}
	if pct {
		for _, sign := range []string{"%", "percent", "pct"} {
			if strings.HasSuffix(asciiLower(body), sign) {
				body = strings.TrimSpace(body[:len(body)-len(sign)])
			}
		}
	}
	if groupedRe.MatchString(body) {
		body = strings.ReplaceAll(body, ",", "")
	}
	plain := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	if plainNumberRe.MatchString(body) {
		f, err := strconv.ParseFloat(body, 64)
		if err == nil {
			return f, plain(f), nil
		}
	}
	if m := scaledRe.FindStringSubmatch(s); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		mult := map[string]float64{"k": 1e3, "thousand": 1e3, "m": 1e6, "mm": 1e6, "mn": 1e6, "million": 1e6,
			"b": 1e9, "bn": 1e9, "billion": 1e9}[strings.ToLower(m[3])]
		return 0, "", fixableValue{why: "a scale is not read", fixes: []string{plain(math.Round(f*mult*100) / 100)}}
	}
	if n, ok := numberWords[strings.ToLower(s)]; ok {
		return 0, "", fixableValue{why: "a number in words is not read", fixes: []string{strconv.Itoa(n)}}
	}
	if m := unitRe.FindStringSubmatch(s); m != nil && (m[1] != "" || m[4] != "") {
		num := strings.ReplaceAll(m[2], ",", "")
		if unit := strings.ToLower(m[4]); pct && (unit == "bp" || unit == "bps") {
			f, _ := strconv.ParseFloat(num, 64)
			return 0, "", fixableValue{why: "basis points are not read; the column is in percent", fixes: []string{plain(f / 100)}}
		}
		why := "a unit is not read; amounts are plain numbers in the result's currency"
		if m[4] == "%" || strings.EqualFold(m[4], "percent") {
			why = "a percent sign is read on a _pct column only, whose cells are percentages"
		}
		return 0, "", fixableValue{why: why, fixes: []string{num}}
	}
	return 0, "", fixableValue{}
}

// valueDateForms are the forms a condition's date value takes.
const valueDateForms = "YYYY, YYYY-Qn, YYYY-MM, YYYY-MM-DD, today, yesterday, this month, last month, this year, last year"

var (
	isoDayRe     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[ T]\d{2}:\d{2}(?::\d{2})?Z?)?$`)
	isoMonthRe   = regexp.MustCompile(`^(\d{4})-(\d{2})$`)
	isoYearRe    = regexp.MustCompile(`^(\d{4})$`)
	isoQuarterRe = regexp.MustCompile(`(?i)^(\d{4})-q([1-4])$`)
	relativeRe   = regexp.MustCompile(`(?i)^(today|yesterday|(this|last) (month|year|week)|\d+ (day|week|month|year)s? ago)$`)
	slashDayRe   = regexp.MustCompile(`^(\d{1,2})[/.](\d{1,2})[/.](\d{4})$`)
	ymdSlashRe   = regexp.MustCompile(`^(\d{4})[/.](\d{1,2})(?:[/.](\d{1,2}))?$`)
	quarterRe    = regexp.MustCompile(`(?i)^(?:q([1-4])[\s/-]*(\d{4})|(\d{4})[\s/-]*q([1-4]))$`)
)

// parseDateValue reads a date value as the period it names, in epoch
// days: a year, a quarter, a month or a day, or a relative word. Every
// form a date or a period cell prints reads. A near form (03/15/2027,
// March 2027, Q1 2026) is refused with its ISO form, both readings
// where day and month are ambiguous.
func parseDateValue(v string, now time.Time) (int64, int64, error) {
	s := strings.TrimSpace(v)
	day := func(y, m, d int) int64 {
		return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Unix() / gold.SecondsPerDay
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case isoDayRe.MatchString(s):
		m := isoDayRe.FindStringSubmatch(s)
		y, mo, d := atoi(m[1]), atoi(m[2]), atoi(m[3])
		if t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC); int(t.Month()) != mo || t.Day() != d {
			return 0, 0, fixableValue{why: "no such day"}
		}
		return day(y, mo, d), day(y, mo, d), nil
	case isoMonthRe.MatchString(s):
		m := isoMonthRe.FindStringSubmatch(s)
		y, mo := atoi(m[1]), atoi(m[2])
		if mo < 1 || mo > 12 {
			return 0, 0, fixableValue{why: "no such month"}
		}
		return day(y, mo, 1), day(y, mo+1, 1) - 1, nil
	case isoYearRe.MatchString(s):
		y := atoi(s)
		return day(y, 1, 1), day(y+1, 1, 1) - 1, nil
	case isoQuarterRe.MatchString(s):
		m := isoQuarterRe.FindStringSubmatch(s)
		y, q := atoi(m[1]), atoi(m[2])
		return day(y, 3*q-2, 1), day(y, 3*q+1, 1) - 1, nil
	case relativeRe.MatchString(s):
		lo, err := parseDate(s, now, false)
		if err != nil {
			return 0, 0, fixableValue{}
		}
		hi, err := parseDate(s, now, true)
		if err != nil {
			return 0, 0, fixableValue{}
		}
		return lo.Unix() / gold.SecondsPerDay, hi.Unix() / gold.SecondsPerDay, nil
	}
	pad := func(n int) string { return fmt.Sprintf("%02d", n) }
	if m := slashDayRe.FindStringSubmatch(s); m != nil {
		a, b, y := atoi(m[1]), atoi(m[2]), m[3]
		valid := func(mo, d int) bool {
			t := time.Date(atoi(y), time.Month(mo), d, 0, 0, 0, 0, time.UTC)
			return mo >= 1 && mo <= 12 && int(t.Month()) == mo && t.Day() == d
		}
		switch {
		case !valid(a, b) && !valid(b, a):
			return 0, 0, fixableValue{why: "no such day"}
		case !valid(b, a):
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(a) + "-" + pad(b)}}
		case !valid(a, b):
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(b) + "-" + pad(a)}}
		case a != b:
			return 0, 0, fixableValue{why: "dates are ISO, and day and month are ambiguous here",
				fixes: []string{y + "-" + pad(a) + "-" + pad(b), y + "-" + pad(b) + "-" + pad(a)}}
		case a == b:
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{y + "-" + pad(a) + "-" + pad(b)}}
		}
	}
	if m := ymdSlashRe.FindStringSubmatch(s); m != nil {
		fix := m[1] + "-" + pad(atoi(m[2]))
		if m[3] != "" {
			fix += "-" + pad(atoi(m[3]))
		}
		return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{fix}}
	}
	if m := quarterRe.FindStringSubmatch(s); m != nil {
		y, q := m[2], m[1]
		if y == "" {
			y, q = m[3], m[4]
		}
		return 0, 0, fixableValue{why: "a quarter is written YYYY-Qn", fixes: []string{y + "-Q" + q}}
	}
	// A date in words ("March 2027", "15 March 2027"): its ISO form.
	if strings.IndexFunc(s, unicode.IsLetter) >= 0 && strings.IndexFunc(s, unicode.IsDigit) >= 0 {
		if dt, err := getDateParser().Parse(newParseConfig(now), s); err == nil && !dt.IsZero() {
			t := dt.Time.UTC()
			fix := t.Format("2006-01-02")
			if !strings.ContainsAny(strings.TrimSpace(strings.TrimRight(strings.Fields(s)[0], ",")), "0123456789") &&
				len(strings.Fields(s)) == 2 {
				fix = t.Format("2006-01")
			}
			return 0, 0, fixableValue{why: "dates are ISO", fixes: []string{fix}}
		}
	}
	return 0, 0, fixableValue{}
}

// periodNote is the header's reading of a date value under op: the
// days it keeps.
func periodNote(op string, lo, hi int64) string {
	switch op {
	case "<":
		return "before " + formatEpochDay(lo)
	case "<=":
		return "to " + formatEpochDay(hi)
	case ">":
		return "after " + formatEpochDay(hi)
	case ">=":
		return "from " + formatEpochDay(lo)
	}
	if lo == hi {
		return formatEpochDay(lo)
	}
	return formatEpochDay(lo) + " → " + formatEpochDay(hi)
}

// cellDay is the first day a date or period cell names, in epoch days:
// YYYY, YYYY-Qn, YYYY-MM, or YYYY-MM-DD with any time after it; false
// for a blank or a cell that is no date (total). It reads the forms by
// hand, since it runs for every row a date condition reads.
func cellDay(cell string) (int64, bool) {
	cell = strings.TrimSpace(cell)
	if len(cell) > 10 && isoDayRe.MatchString(cell) {
		cell = cell[:10]
	}
	digits := func(s string) (int, bool) {
		n := 0
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
			n = 10*n + int(s[i]-'0')
		}
		return n, true
	}
	day := func(y, m, d int) (int64, bool) {
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		return t.Unix() / gold.SecondsPerDay, m >= 1 && m <= 12 && int(t.Month()) == m && t.Day() == d
	}
	y, ok := digits(cell[:min(4, len(cell))])
	switch {
	case !ok || len(cell) < 4:
		return 0, false
	case len(cell) == 4:
		return day(y, 1, 1)
	case cell[4] != '-':
		return 0, false
	case len(cell) == 7 && (cell[5] == 'q' || cell[5] == 'Q') && cell[6] >= '1' && cell[6] <= '4':
		return day(y, 3*int(cell[6]-'0')-2, 1)
	case len(cell) == 7:
		if m, ok := digits(cell[5:]); ok {
			return day(y, m, 1)
		}
	case len(cell) == 10 && cell[7] == '-':
		m, okM := digits(cell[5:7])
		d, okD := digits(cell[8:])
		if okM && okD {
			return day(y, m, d)
		}
	}
	return 0, false
}

// missingCell is a cell with no value.
func missingCell(cell string) bool {
	cell = strings.TrimSpace(cell)
	return cell == "" || cell == "n/a"
}

// thousandsComma is a comma between digits that groups thousands: three
// digits follow, then no digit.
var thousandsComma = regexp.MustCompile(`\d,\d{3}(?:[^\d]|$)`)

// splitNumberList splits a comma list of numbers, keeping a comma that
// groups thousands inside its number. Such a comma is one where the list
// separates its items with ", ", or where a 0 follows it (1,000 is no
// list of 1 and 000); else it is ambiguous, and 100,200 may be 100 and
// 200 or 100200.
func splitNumberList(s string) (items []string, ambiguous bool) {
	spaced := strings.Contains(s, ", ")
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ',' && i > 0 && thousandsComma.MatchString(s[i-1:min(len(s), i+5)]) && !spaced && s[i+1] != '0' {
			ambiguous = true
		}
	}
	return splitNumbers(s, true), ambiguous
}

// splitNumbers splits s at its commas, but for those that group
// thousands where thousands says so.
func splitNumbers(s string, thousands bool) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ',' || thousands && i > 0 && thousandsComma.MatchString(s[i-1:min(len(s), i+5)]) {
			continue
		}
		out = append(out, unquote(strings.TrimSpace(s[start:i])))
		start = i + 1
	}
	out = append(out, unquote(strings.TrimSpace(s[start:])))
	kept := out[:0]
	for _, item := range out {
		if item != "" {
			kept = append(kept, strings.ReplaceAll(item, ",", ""))
		}
	}
	return kept
}
