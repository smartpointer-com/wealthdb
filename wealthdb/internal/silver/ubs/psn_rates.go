package ubs

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A PSN cash account record states the account's interest conditions in
// columns of its own. Per side (Cdt credit, Dbt debit) it holds up to
// four tiers, each a rate (CdtIntrstRate1) on the balance up to a limit
// (CdtLmtAmt1), and the day the side holds from (CdtIntrstVldFrmDt). The
// account's term ends on EndOfDurtn, 9999 for an open account, and
// CalIntrstBasisDesc names its day count. The book balance
// (appendBookBalances) carries the record as its payload, and the
// adapter states those conditions on it under the stated-rate keys
// (canonical.StatedRateKeys).

// psnRateSides are the record's two sides and their column prefixes.
var psnRateSides = []struct{ side, prefix string }{
	{canonical.RateSideCredit, "Cdt"}, {canonical.RateSideDebit, "Dbt"},
}

// psnTiers is the most tiers a side states.
const psnTiers = 4

// psnShaped reports whether a payload states PSN's tiers: its first
// credit rate is set, a value or not. Such a balance's rates are the
// record's alone.
func psnShaped(payload []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return false
	}
	raw, ok := m["CdtIntrstRate1"]
	return ok && string(raw) != "null"
}

// psnRateKeys is the stated-rate keys of a PSN cash account record: one
// rate per side that states a tier, the credit side first. A side's tier
// is one with a rate, numbered by its column, and a tier past the first
// counts only after a column whose limit lies between 0.005 and 1e14; a
// limit outside that is none.
// A side holds from its day; the term's end is the record's before the
// open one (a year below 9000); the day count is canonical.DayCount's
// name for the record's, case and spaces aside. A value that does not
// read states nothing. No tier is no keys.
func psnRateKeys(payload []byte) map[string]any {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	var termEnd *time.Time
	if d, ok := psnDate(m, "EndOfDurtn"); ok && d.Year() < 9000 {
		termEnd = &d
	}
	var dayCount canonical.DayCount
	if s, ok := psnText(m, "CalIntrstBasisDesc"); ok {
		s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
		for _, dc := range canonical.DayCounts() {
			if s == strings.ToUpper(string(dc)) {
				dayCount = dc
			}
		}
	}
	var rates []canonical.StatedRate
	for _, side := range psnRateSides {
		limit := func(tier int) (float64, bool) {
			v, ok := psnNumber(m, side.prefix+"LmtAmt"+strconv.Itoa(tier))
			return v, ok && v >= 0.005 && v <= 1e14
		}
		r := canonical.StatedRate{Side: side.side, TermEnd: termEnd, DayCount: dayCount}
		for tier := 1; tier <= psnTiers; tier++ {
			if _, capped := limit(tier - 1); tier > 1 && !capped {
				continue
			}
			pct, ok := psnNumber(m, side.prefix+"IntrstRate"+strconv.Itoa(tier))
			if !ok {
				continue
			}
			t := canonical.StatedTier{Pct: pct, Number: tier}
			if up, ok := limit(tier); ok {
				t.UpTo = &up
			}
			r.Tiers = append(r.Tiers, t)
		}
		if len(r.Tiers) == 0 {
			continue
		}
		if d, ok := psnDate(m, side.prefix+"IntrstVldFrmDt"); ok {
			r.ValidFrom = &d
		}
		rates = append(rates, r)
	}
	return canonical.StatedRateKeys(rates...)
}

// psnText is a record value's text: a string's, or the JSON of any other
// value. A null or absent value has none.
func psnText(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}

// psnDigitGroups is a number's text whose every underscore sits between
// two digits: 1_000 reads as 1000.
var psnDigitGroups = regexp.MustCompile(`^[^_]*(?:[0-9]_[0-9][^_]*)*$`)

// psnNumber reads a record value as a finite number: its text, ASCII
// space around it ignored, digits grouped by underscores.
func psnNumber(m map[string]json.RawMessage, key string) (float64, bool) {
	s, ok := psnText(m, key)
	if !ok {
		return 0, false
	}
	s = strings.Trim(s, " \t\n\v\f\r")
	if strings.ContainsAny(s, "xX") || !psnDigitGroups.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// psnDatePattern is a day as the record writes one: a year, a month and
// a day, split by two dashes or two slashes, the month and the day of
// one digit or two, a time of day after it ignored with its zone.
var psnDatePattern = regexp.MustCompile(`^(\d{1,4})(?:-(\d{1,2})-(\d{1,2})|/(\d{1,2})/(\d{1,2}))` +
	`(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}(?::?\d{2})?)?)?$`)

// psnDate reads a record value as a day.
func psnDate(m map[string]json.RawMessage, key string) (time.Time, bool) {
	s, ok := psnText(m, key)
	if !ok {
		return time.Time{}, false
	}
	p := psnDatePattern.FindStringSubmatch(strings.Trim(s, " \t\n\v\f\r"))
	if p == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(p[1])
	mo, _ := strconv.Atoi(p[2] + p[4])
	d, _ := strconv.Atoi(p[3] + p[5])
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}
