package canonical

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// The keys a cash balance's or an account's payload states its interest
// rate under, for every source alike. On a bond's payload
// PayloadInterestRate is the coupon (PayloadTermsKeys); on cash it is the
// rate the balance earns or pays. A liability's rate is its terms', never
// these keys: a mortgage or a loan stated here would read as a deposit's.
const (
	// PayloadInterestTiers states a tiered rate: a list of
	// {StatedTierRate: percent, StatedTierUpTo: the balance the tier runs
	// to}, the last tier's limit null or absent. It replaces
	// PayloadInterestRate.
	PayloadInterestTiers = "interest_tiers"
	// PayloadInterestValidFrom is the day the rate holds from:
	// YYYY-MM-DD.
	PayloadInterestValidFrom = "interest_valid_from"
	// PayloadInterestSide is credit (the balance earns the rate, the
	// default) or debit (it pays it).
	PayloadInterestSide = "interest_side"
	// PayloadInterestTermEnd is the day a term deposit's rate ends:
	// YYYY-MM-DD.
	PayloadInterestTermEnd = "interest_term_end"
	// PayloadInterestDayCount is how the rate counts its days: a
	// DayCount, as DayCounts names it.
	PayloadInterestDayCount = "interest_day_count"
	// PayloadInterestConditions states several rates on one payload: a
	// list of objects, each with its own PayloadInterestRate or
	// PayloadInterestTiers, PayloadInterestValidFrom, PayloadInterestSide,
	// PayloadInterestTermEnd and PayloadInterestDayCount; none inherits
	// the payload's. A credit and a debit side go here.
	PayloadInterestConditions = "interest_conditions"
)

// The keys of one tier in PayloadInterestTiers. StatedTierNumber is the
// tier's own number, counted from 1, where a source numbers its tiers
// and may leave one out: every tier of the list states one, each above
// the one before, and a tier's limit may then be absent before the last.
const (
	StatedTierRate   = "rate"
	StatedTierUpTo   = "up_to"
	StatedTierNumber = "tier"
)

// The sides of a stated rate.
const (
	RateSideCredit = "credit"
	RateSideDebit  = "debit"
)

// StatedRate is one rate a payload states: its side, its tiers in
// order, the day it holds from, a term deposit's end, its day count (""
// where it states none), and the payload key it was read from.
type StatedRate struct {
	Side      string
	Tiers     []StatedTier
	ValidFrom *time.Time
	TermEnd   *time.Time
	DayCount  DayCount
	Field     string
}

// StatedTier is one tier of a stated rate: percent a year on the part
// of the balance up to UpTo, nil for the last. Number is the tier's own
// number where the source states one, else 0: the tier is then numbered
// by its place.
type StatedTier struct {
	Pct    float64
	UpTo   *float64
	Number int
}

// ReadStatedRates reads the rates a payload states under the keys
// above: the conditions in PayloadInterestConditions, else the one the
// payload's own keys state. A condition reads its tiers, else its flat
// rate. A rate is a number or its text (0.75 or 0.75%); a tier's limit a
// number or its text, with no %. A null value is not stated. A
// condition whose rate, tier, side, day or day count does not read
// states nothing, so a misread value never stands for a rate. A day
// count reads as DayCounts names it. Of two conditions of one side and
// day, the first holds: a payload states one schedule per side and day.
func ReadStatedRates(payload []byte) []StatedRate {
	var top map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &top) != nil {
		return nil
	}
	if raw, ok := top[PayloadInterestConditions]; ok {
		var list []map[string]json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return nil
		}
		var out []StatedRate
		for i, c := range list {
			r, ok := readStatedCondition(c, fmt.Sprintf("%s[%d]", PayloadInterestConditions, i))
			if ok && !slices.ContainsFunc(out, func(o StatedRate) bool {
				return o.Side == r.Side && (o.ValidFrom == nil) == (r.ValidFrom == nil) &&
					(o.ValidFrom == nil || o.ValidFrom.Equal(*r.ValidFrom))
			}) {
				out = append(out, r)
			}
		}
		return out
	}
	if r, ok := readStatedCondition(top, ""); ok {
		return []StatedRate{r}
	}
	return nil
}

// readStatedCondition reads one condition's keys; prefix names where it
// sits in the payload, empty at the top.
func readStatedCondition(c map[string]json.RawMessage, prefix string) (StatedRate, bool) {
	field := func(key string) string {
		if prefix == "" {
			return key
		}
		return prefix + "." + key
	}
	r := StatedRate{Side: RateSideCredit}
	if raw, ok := c[PayloadInterestSide]; ok && string(raw) != "null" {
		s, ok := jsonText(raw)
		switch s = strings.ToLower(strings.TrimSpace(s)); {
		case ok && (s == RateSideCredit || s == RateSideDebit):
			r.Side = s
		default:
			return r, false
		}
	}
	for _, d := range []struct {
		key string
		dst **time.Time
	}{{PayloadInterestValidFrom, &r.ValidFrom}, {PayloadInterestTermEnd, &r.TermEnd}} {
		if raw, ok := c[d.key]; ok && string(raw) != "null" {
			s, _ := jsonText(raw)
			t, err := time.Parse(time.DateOnly, strings.TrimSpace(s))
			if err != nil {
				return r, false
			}
			*d.dst = &t
		}
	}
	if raw, ok := c[PayloadInterestDayCount]; ok && string(raw) != "null" {
		s, _ := jsonText(raw)
		if r.DayCount = DayCount(s); !r.DayCount.Valid() {
			return r, false
		}
	}
	if raw, ok := c[PayloadInterestTiers]; ok && string(raw) != "null" {
		var tiers []map[string]json.RawMessage
		if json.Unmarshal(raw, &tiers) != nil || len(tiers) == 0 {
			return r, false
		}
		_, numbered := tiers[0][StatedTierNumber]
		for i, t := range tiers {
			pct, ok := jsonPercent(t[StatedTierRate])
			if !ok {
				return r, false
			}
			tier := StatedTier{Pct: pct}
			if n, ok := t[StatedTierNumber]; ok != numbered {
				return r, false
			} else if ok {
				v, ok := jsonNumber(n)
				if !ok || v != math.Trunc(v) || v < 1 || (i > 0 && int(v) <= r.Tiers[i-1].Number) {
					return r, false
				}
				tier.Number = int(v)
			}
			if up, ok := t[StatedTierUpTo]; ok && string(up) != "null" {
				v, ok := jsonNumber(up)
				if !ok || v <= 0 {
					return r, false
				}
				tier.UpTo = &v
			} else if i < len(tiers)-1 && !numbered {
				return r, false
			}
			r.Tiers = append(r.Tiers, tier)
		}
		r.Field = field(PayloadInterestTiers)
		return r, true
	}
	pct, ok := jsonPercent(c[PayloadInterestRate])
	if !ok {
		return r, false
	}
	r.Tiers, r.Field = []StatedTier{{Pct: pct}}, field(PayloadInterestRate)
	return r, true
}

// StatedRateKeys is the payload keys that state rs, the inverse of
// ReadStatedRates: one rate under the payload's own keys, several under
// PayloadInterestConditions in order. A rate of one tier with no limit
// and no number past 1 is a flat PayloadInterestRate, else its
// PayloadInterestTiers, each with its number where it has one. Each
// rate states its side, and its day, its term's end and its day count
// where it has them. No rate is no keys.
func StatedRateKeys(rs ...StatedRate) map[string]any {
	switch len(rs) {
	case 0:
		return nil
	case 1:
		return rs[0].keys()
	}
	list := make([]map[string]any, len(rs))
	for i, r := range rs {
		list[i] = r.keys()
	}
	return map[string]any{PayloadInterestConditions: list}
}

// keys is one rate's keys for StatedRateKeys.
func (r StatedRate) keys() map[string]any {
	keys := map[string]any{PayloadInterestSide: r.Side}
	if r.ValidFrom != nil {
		keys[PayloadInterestValidFrom] = r.ValidFrom.Format(time.DateOnly)
	}
	if r.TermEnd != nil {
		keys[PayloadInterestTermEnd] = r.TermEnd.Format(time.DateOnly)
	}
	if r.DayCount != "" {
		keys[PayloadInterestDayCount] = string(r.DayCount)
	}
	if len(r.Tiers) == 1 && r.Tiers[0].UpTo == nil && r.Tiers[0].Number <= 1 {
		keys[PayloadInterestRate] = r.Tiers[0].Pct
		return keys
	}
	tiers := make([]map[string]any, len(r.Tiers))
	for i, t := range r.Tiers {
		var upTo any
		if t.UpTo != nil {
			upTo = *t.UpTo
		}
		tiers[i] = map[string]any{StatedTierRate: t.Pct, StatedTierUpTo: upTo}
		if t.Number != 0 {
			tiers[i][StatedTierNumber] = t.Number
		}
	}
	keys[PayloadInterestTiers] = tiers
	return keys
}

// StatedRateKeyNames is every key the stated-rate keys use at the top of
// a payload: the keys a writer replaces whole.
var StatedRateKeyNames = []string{PayloadInterestRate, PayloadInterestTiers, PayloadInterestValidFrom,
	PayloadInterestSide, PayloadInterestTermEnd, PayloadInterestDayCount, PayloadInterestConditions}

// jsonText is a JSON string's text, or a number's.
func jsonText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), true
	}
	return "", false
}

// jsonPercent reads a number, or a number's text with an optional %.
func jsonPercent(raw json.RawMessage) (float64, bool) {
	if raw == nil {
		return 0, false
	}
	s, ok := jsonText(raw)
	if !ok {
		return 0, false
	}
	return readStatedPercent(s)
}

// jsonNumber reads a number, or a number's text.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	s, ok := jsonText(raw)
	if !ok {
		return 0, false
	}
	return readStatedNumber(s)
}
