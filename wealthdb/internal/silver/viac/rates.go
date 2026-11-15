package viac

import (
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

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
	text, ok := silver.JSONText(p.Strategy["interestRate"])
	if !ok {
		return nil
	}
	pct, ok := silver.StatedNumber(text)
	if !ok {
		return nil
	}
	return canonical.StatedRateKeys(canonical.StatedRate{Side: canonical.RateSideCredit,
		Tiers: []canonical.StatedTier{{Pct: pct}}})
}
