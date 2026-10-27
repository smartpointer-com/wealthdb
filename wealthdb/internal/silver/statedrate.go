package silver

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// WithStatedRates is payload, a JSON object (empty for none), with the
// stated-rate keys of keys (canonical.StatedRateKeys). The payload's
// stated-rate keys are replaced whole, so none it held before stands
// beside the new ones; its other keys stay byte for byte. A balance
// whose payload states no rate says nothing of it: unknown, never zero.
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
