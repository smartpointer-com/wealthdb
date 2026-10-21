package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The rate skeleton `yield coverage -f ndjson` prints loads once its rate
// is filled in: its keys, the currency's among them, are the rate
// record's. Left empty, it is refused.
func TestARateSkeletonLoadsFilledIn(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(yield.Skeleton{SilverSourceID: "manual", Account: "savings-1",
		Rate: &yield.RateSkeleton{Currency: "CHF"}, Note: "Savings"})
	if err != nil {
		t.Fatal(err)
	}
	filled := strings.Replace(string(raw), `"rate":{`, `"rate":{"pct":0.5,`, 1)
	d, err := declarations(t, filled+"\n")
	if err != nil || len(d) != 1 || d[0].Rate == nil || d[0].Rate.Pct != 0.5 || d[0].Rate.Currency != "CHF" ||
		d[0].AccountExternalID != "savings-1" {
		t.Errorf("%s: %+v %v", filled, d, err)
	}
	if _, err := declarations(t, string(raw)+"\n"); err == nil {
		t.Errorf("%s loads unfilled", raw)
	}
}
