package viac

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// digitGroups is a number's text whose every underscore sits between two
// digits: 1_000 reads as 1000.
var digitGroups = regexp.MustCompile(`^[^_]*(?:[0-9]_[0-9][^_]*)*$`)

// strategyRateKeys is the stated-rate keys of an account payload whose
// strategy states the rate its cash earns (strategy.interestRate): one
// flat credit rate. A value that is no finite number, ASCII space around
// it aside and digits grouped by underscores, states none.
func strategyRateKeys(payload []byte) map[string]any {
	var p struct {
		Strategy map[string]json.RawMessage `json:"strategy"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	raw, ok := p.Strategy["interestRate"]
	if !ok || string(raw) == "null" {
		return nil
	}
	text := string(raw)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		text = s
	}
	text = strings.Trim(text, " \t\n\v\f\r")
	if strings.ContainsAny(text, "xX") || !digitGroups.MatchString(text) {
		return nil
	}
	pct, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
		return nil
	}
	return canonical.StatedRateKeys(canonical.StatedRate{Side: canonical.RateSideCredit,
		Tiers: []canonical.StatedTier{{Pct: pct}}})
}
