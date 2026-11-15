package raiffeisenat

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// An account's details state its current credit and debit rates as text
// (details.interest_credit, details.interest_debit): "0,111% ab
// 01.01.2024", the rate with a comma or a dot decimal, and the day it
// holds from. The account record states them under the stated-rate keys
// (canonical.StatedRateKeys).

// The patterns of a rate's text: a digit before a percent sign states a
// rate; the first number before one is the rate, the first DD.MM.YYYY
// the day it holds from.
var (
	statesRate = regexp.MustCompile(`[0-9]\s*%`)
	ratePct    = regexp.MustCompile(`(-?[0-9]+([.,][0-9]+)?)\s*%`)
	rateDay    = regexp.MustCompile(`([0-9]{2}\.[0-9]{2}\.[0-9]{4})`)
)

// detailsRateSides are the details keys of a rate and the side each
// states.
var detailsRateSides = []struct{ side, key string }{
	{canonical.RateSideCredit, "interest_credit"}, {canonical.RateSideDebit, "interest_debit"},
}

// detailsRateKeys is the stated-rate keys of an account payload's
// details: one rate per side whose text states one, the credit side
// first, from its day where the text states a valid one. No rate is no
// keys.
func detailsRateKeys(payload []byte) map[string]any {
	var p struct {
		Details map[string]json.RawMessage `json:"details"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var rates []canonical.StatedRate
	for _, s := range detailsRateSides {
		text, ok := silver.JSONText(p.Details[s.key])
		if !ok || !statesRate.MatchString(text) {
			continue
		}
		pct, err := strconv.ParseFloat(strings.ReplaceAll(ratePct.FindStringSubmatch(text)[1], ",", "."), 64)
		if err != nil {
			continue
		}
		r := canonical.StatedRate{Side: s.side, Tiers: []canonical.StatedTier{{Pct: pct}}}
		if d, err := time.Parse("02.01.2006", rateDay.FindString(text)); err == nil {
			r.ValidFrom = &d
		}
		rates = append(rates, r)
	}
	return canonical.StatedRateKeys(rates...)
}
