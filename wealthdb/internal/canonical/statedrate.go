package canonical

import (
	"encoding/json"
	"fmt"
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
	// PayloadInterestConditions states several rates on one payload: a
	// list of objects, each with its own PayloadInterestRate or
	// PayloadInterestTiers, PayloadInterestValidFrom and
	// PayloadInterestSide; none inherits the payload's. A credit and a
	// debit side go here.
	PayloadInterestConditions = "interest_conditions"
)

// The keys of one tier in PayloadInterestTiers.
const (
	StatedTierRate = "rate"
	StatedTierUpTo = "up_to"
)

// The sides of a stated rate.
const (
	RateSideCredit = "credit"
	RateSideDebit  = "debit"
)

// StatedRate is one rate a payload states: its side, its tiers in
// order, the day it holds from, and the payload key it was read from.
type StatedRate struct {
	Side      string
	Tiers     []StatedTier
	ValidFrom *time.Time
	Field     string
}

// StatedTier is one tier of a stated rate: percent a year on the part
// of the balance up to UpTo, nil for the last.
type StatedTier struct {
	Pct  float64
	UpTo *float64
}

// ReadStatedRates reads the rates a payload states under the keys
// above: the conditions in PayloadInterestConditions, else the one the
// payload's own keys state. A condition reads its tiers, else its flat
// rate. A rate is a number or its text (0.75 or 0.75%); a tier's limit a
// number or its text, with no %. A null value is not stated. A
// condition whose rate, tier, side or day does not read states nothing,
// so a misread value never stands for a rate. Of two conditions of one
// side and day, the first holds: a payload states one schedule per side
// and day.
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
	if raw, ok := c[PayloadInterestValidFrom]; ok && string(raw) != "null" {
		s, _ := jsonText(raw)
		t, err := time.Parse(time.DateOnly, strings.TrimSpace(s))
		if err != nil {
			return r, false
		}
		r.ValidFrom = &t
	}
	if raw, ok := c[PayloadInterestTiers]; ok && string(raw) != "null" {
		var tiers []map[string]json.RawMessage
		if json.Unmarshal(raw, &tiers) != nil || len(tiers) == 0 {
			return r, false
		}
		for i, t := range tiers {
			pct, ok := jsonPercent(t[StatedTierRate])
			if !ok {
				return r, false
			}
			tier := StatedTier{Pct: pct}
			if up, ok := t[StatedTierUpTo]; ok && string(up) != "null" {
				v, ok := jsonNumber(up)
				if !ok || v <= 0 {
					return r, false
				}
				tier.UpTo = &v
			} else if i < len(tiers)-1 {
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
// is a flat PayloadInterestRate, else its PayloadInterestTiers. Each
// rate states its side, and its day where it has one. No rate is no
// keys.
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
	if len(r.Tiers) == 1 && r.Tiers[0].UpTo == nil {
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
	}
	keys[PayloadInterestTiers] = tiers
	return keys
}

// StatedRateKeyNames is every key the stated-rate keys use at the top of
// a payload: the keys a writer replaces whole.
var StatedRateKeyNames = []string{PayloadInterestRate, PayloadInterestTiers, PayloadInterestValidFrom,
	PayloadInterestSide, PayloadInterestConditions}

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
