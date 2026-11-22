package all

import (
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// TestCatalogue holds every collector the adapters state to a
// registered kind and, where it is a subsource, to one of the kind's
// subsource kinds, and pins the collectors the setup offers.
func TestCatalogue(t *testing.T) {
	cat := silver.Catalogue()
	var names []string
	seen := map[string]bool{}
	for _, c := range cat {
		names = append(names, c.Name)
		if seen[c.Name] {
			t.Errorf("collector %q is stated twice", c.Name)
		}
		seen[c.Name] = true
		src, err := silver.Get(c.Kind)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		switch {
		case c.Subsource != "" && !slices.Contains(src.Subsources, c.Subsource):
			t.Errorf("%s: subsource %q is not one of kind %s's %v", c.Name, c.Subsource, c.Kind, src.Subsources)
		case c.Subsource == "" && len(src.Subsources) > 0:
			t.Errorf("%s: kind %s takes subsources, the collector names none", c.Name, c.Kind)
		}
		if c.Display == "" || c.Covers == "" {
			t.Errorf("%s: no display name or no line on what it covers", c.Name)
		}
		if c.PerItem != (len(c.Files) == 0) {
			t.Errorf("%s: a collector names its files, or keeps one per item", c.Name)
		}
		for _, f := range c.Files {
			if strings.Contains(f, "/") {
				t.Errorf("%s: file %q is not a name in the data folder", c.Name, f)
			}
		}
	}
	want := []string{"amex", "angellist", "carta", "chase", "cointracking", "curves", "equityzen",
		"fidelity-web", "firstcitizens", "fred", "manual", "plaid", "raiffeisen_at", "refdata",
		"relevate", "schwab-api", "schwab-web", "svb", "swissquote", "ubs-psn", "ubs-web", "viac"}
	if got := slices.Sorted(slices.Values(names)); !slices.Equal(got, want) {
		t.Errorf("catalogue = %v\nwant       %v", got, want)
	}
	// Grouped, the banks first and the market data last.
	for i := 1; i < len(cat); i++ {
		if cat[i].Group < cat[i-1].Group {
			t.Errorf("%s is listed after %s, out of its group", cat[i].Name, cat[i-1].Name)
		}
	}
	if cat[len(cat)-1].Group != silver.GroupMarketData {
		t.Errorf("the catalogue ends with %s, not the market data", cat[len(cat)-1].Name)
	}
}

func TestCollectorOf(t *testing.T) {
	for _, tc := range []struct{ kind, sub, path, want string }{
		{"viac", "", "/d/viac/viac.db", "viac"},
		{"viac", "", "/elsewhere/mine.db", "viac"},
		{"ubs", "ubs-web", "/d/ubs-web/ubs-web.db", "ubs-web"},
		{"ubs", "", "/d/ubs-psn/ubs-psn.db", "ubs-psn"},
		{"fidelity", "", "/d/svb/svb-deposit.db", "svb"},
		{"fidelity", "", "/d/fidelity-web/fidelity-web.db", "fidelity-web"},
		{"plaid", "", "/d/plaid/bank/bank.db", "plaid"},
		{"fidelity", "", "/d/other.db", ""},
		{"synthetic", "", "/d/x.db", ""},
	} {
		c, ok := silver.CollectorOf(tc.kind, tc.sub, tc.path)
		if got := c.Name; got != tc.want || ok != (tc.want != "") {
			t.Errorf("CollectorOf(%s, %s, %s) = %q, %v; want %q", tc.kind, tc.sub, tc.path, got, ok, tc.want)
		}
	}
}
