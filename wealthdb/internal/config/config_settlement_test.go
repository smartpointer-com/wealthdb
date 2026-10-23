package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// cyclesText is settlement cycles as market, day and days, one per cycle.
func cyclesText(cs []canonical.SettlementCycle) []string {
	var out []string
	for _, c := range cs {
		from := "-"
		if c.From != nil {
			from = c.From.Format(time.DateOnly)
		}
		out = append(out, fmt.Sprintf("%s %s %d %s", c.Market, from, c.Days, c.Origin))
	}
	return out
}

// A market the block names replaces its built-in cycles, in the block's
// order; one it adds joins them; an empty list leaves the market none.
// Every other market keeps its built-in cycles, and the rows go by market.
func TestSettlementCycleRows(t *testing.T) {
	t.Parallel()
	c, err := yieldConfig(t, `{"settlement_cycles": {
        "US": [{"days": 3}, {"from": "2030-01-02", "days": 1}],
        "HK": [{"from": "2020-01-02", "days": 2}],
        "JP": []}}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var want []canonical.SettlementCycle
	for _, b := range canonical.BuiltinSettlementCycles() {
		if b.Market != "US" && b.Market != "JP" {
			want = append(want, b)
		}
	}
	day := func(s string) *time.Time {
		d, _ := time.Parse(time.DateOnly, s)
		return &d
	}
	want = append(want, canonical.SettlementCycle{Market: "US", Days: 3, Origin: canonical.OriginConfig},
		canonical.SettlementCycle{Market: "US", From: day("2030-01-02"), Days: 1, Origin: canonical.OriginConfig},
		canonical.SettlementCycle{Market: "HK", From: day("2020-01-02"), Days: 2, Origin: canonical.OriginConfig})
	slices.SortStableFunc(want, func(a, b canonical.SettlementCycle) int { return strings.Compare(a.Market, b.Market) })
	if got := cyclesText(c.YieldRows().SettlementCycles); !slices.Equal(got, cyclesText(want)) {
		t.Errorf("settlement cycles:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(cyclesText(want), "\n"))
	}
	empty, err := yieldConfig(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Config{{}, empty} {
		if got := cyclesText(c.YieldRows().SettlementCycles); !slices.Equal(got,
			cyclesText(canonical.BuiltinSettlementCycles())) {
			t.Errorf("settlement cycles without the block = %q", got)
		}
	}
}

// A market is a country, its cycles ascend by day, each of one business
// day or more, and only the first may leave its day out.
func TestSettlementCycleRejects(t *testing.T) {
	t.Parallel()
	for block, want := range map[string]string{
		`{"settlement_cycles": {"us": [{"days": 1}]}}`:                                                          `yield.settlement_cycles["us"]: not a market: want an ISO 3166 alpha-2 code`,
		`{"settlement_cycles": {"QQ": [{"days": 1}]}}`:                                                          `yield.settlement_cycles["QQ"]: not a market`,
		`{"settlement_cycles": {"EU": [{"days": 1}]}}`:                                                          `yield.settlement_cycles["EU"]: not a market`,
		`{"settlement_cycles": {"US": [{"days": 0}]}}`:                                                          `yield.settlement_cycles["US"][0].days 0: want 1 or more business days`,
		`{"settlement_cycles": {"US": [{"from": "2020-01-02"}]}}`:                                               `yield.settlement_cycles["US"][0].days 0: want 1 or more`,
		`{"settlement_cycles": {"US": [{"days": 2}, {"days": 1}]}}`:                                             `yield.settlement_cycles["US"][1]: from is required past the first cycle`,
		`{"settlement_cycles": {"US": [{"from": "2020-13-01", "days": 1}]}}`:                                    `yield.settlement_cycles["US"][0].from "2020-13-01": want YYYY-MM-DD`,
		`{"settlement_cycles": {"US": [{"from": "2024-05-28", "days": 2}, {"from": "2024-05-28", "days": 1}]}}`: `yield.settlement_cycles["US"][1].from 2024-05-28: the cycles ascend by day`,
		`{"settlement_cycles": {"US": [{"from": "2024-05-28", "days": 1}, {"from": "2017-09-05", "days": 2}]}}`: `yield.settlement_cycles["US"][1].from 2017-09-05: the cycles ascend`,
		`{"settlement_cycles": {"US": [{"days": 1, "cycle": 2}]}}`:                                              `unknown field "cycle"`,
		`{"settlement_cycles": {"US": {"days": 1}}}`:                                                            "cannot unmarshal",
	} {
		if _, err := yieldConfig(t, block); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", block, err, want)
		}
	}
}
