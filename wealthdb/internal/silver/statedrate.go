package silver

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// WithStatedRates is payload, a JSON object (empty for none), with the
// stated-rate keys of keys (canonical.StatedRateKeys). The payload's
// stated-rate keys are replaced whole, so none it held before stands
// beside the new ones; its other keys keep their values, numbers as
// written (re-encoded compact, keys sorted). A balance whose payload
// states no rate says nothing of it: unknown, never zero.
func WithStatedRates(payload json.RawMessage, keys map[string]any) (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &out); err != nil {
			return nil, fmt.Errorf("stated rate: payload is no JSON object: %w", err)
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
	}
	for _, k := range canonical.StatedRateKeyNames {
		delete(out, k)
	}
	for k, v := range keys {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("stated rate %s: %w", k, err)
		}
		out[k] = raw
	}
	return json.Marshal(out)
}

// WithCreditRate is WithStatedRates for one flat credit rate: pct in
// percent a year, held from validFrom (YYYY-MM-DD) where it is not
// empty.
func WithCreditRate(payload json.RawMessage, pct float64, validFrom string) (json.RawMessage, error) {
	r := canonical.StatedRate{Side: canonical.RateSideCredit, Tiers: []canonical.StatedTier{{Pct: pct}}}
	if validFrom != "" {
		t, err := time.Parse(time.DateOnly, validFrom)
		if err != nil {
			return nil, fmt.Errorf("stated rate: valid from %q: %w", validFrom, err)
		}
		r.ValidFrom = &t
	}
	return WithStatedRates(payload, canonical.StatedRateKeys(r))
}

// JSONText is a JSON value's text: a string's, or the JSON of any other
// value. A null or absent (nil) value has none.
func JSONText(raw json.RawMessage) (string, bool) {
	if raw == nil || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}

// digitGroups is a number's text whose every underscore sits between two
// digits: 1_000 reads as 1000.
var digitGroups = regexp.MustCompile(`^[^_]*(?:[0-9]_[0-9][^_]*)*$`)

// StatedNumber reads a stated value's text as a finite number: ASCII
// space around it ignored, digits grouped by underscores. A hexadecimal
// or non-finite number states none.
func StatedNumber(text string) (float64, bool) {
	text = strings.Trim(text, " \t\n\v\f\r")
	if strings.ContainsAny(text, "xX") || !digitGroups.MatchString(text) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}
