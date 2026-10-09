package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestGainsCLIEndToEnd(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)

	for _, view := range gainsViews {
		so, se, code := run(t, "-c", cfg, "gains", view, "-", "today", "-x", "CHF")
		if code != 0 {
			t.Errorf("gains %s: exit=%d stderr=%s", view, code, se)
			continue
		}
		if !strings.Contains(so, "silver_source") && !strings.Contains(so, "period") {
			t.Errorf("gains %s: no header row:\n%s", view, so)
		}
	}
	if so, se, code := run(t, "-c", cfg, "gains", "realized", "--documents", "all", "-r", "-", "today"); code != 0 {
		t.Errorf("realized --documents all -r: exit=%d stderr=%s out=%s", code, se, so)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"gains", "summary", "--documents", "all"}, "belong to the realized view"},
		{[]string{"gains", "lots", "-r"}, "belong to the realized view"},
		{[]string{"gains", "realized", "--documents", "some"}, "invalid --documents"},
		{[]string{"gains", "summary", "--period", "hourly"}, "invalid --period"},
		{[]string{"gains", "nope"}, "unknown view"},
		{[]string{"gains"}, "a view subcommand is required"},
	} {
		_, se, code := run(t, append([]string{"-c", cfg}, c.args...)...)
		if code != 2 || !strings.Contains(se, c.want) {
			t.Errorf("%v: exit=%d stderr=%q, want 2 and %q", c.args, code, se, c.want)
		}
	}
}

// TestGainsViewsResolveTheirDefaults pins each view's default column
// set against its registry, and the aggregate views' reconciling
// grains against the usage text.
func TestGainsViewsResolveTheirDefaults(t *testing.T) {
	t.Parallel()
	usage := gainsUsage()
	for _, view := range gainsViews {
		rep := gainsReport(request{view: view, currency: "USD", period: "monthly"})
		if _, err := rep.pick("default"); err != nil {
			t.Errorf("%s defaults: %v", view, err)
		}
		if !strings.Contains(usage, "\n  "+view+" ") {
			t.Errorf("usage does not list the %s view", view)
		}
	}
}

// TestReportHeadersAreIdentifiers holds every report's column names and
// headers to letters, digits and underscores, so a JSON key or CSV
// header never needs quoting: a percentage is _pct, never _%.
func TestReportHeadersAreIdentifiers(t *testing.T) {
	t.Parallel()
	ident := regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	reports := map[string]*report{
		"transactions": transactionsReport(request{currency: "USD"}),
		"returns":      returnsReport(request{currency: "USD", method: "both"}, nil),
	}
	for _, v := range holdingsFamily.views {
		reports["holdings "+v] = holdingsReport(request{view: v, currency: "USD"})
	}
	for _, v := range spendingFamily.views {
		reports["spending "+v] = spendingReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for _, v := range incomeFamily.views {
		reports["income "+v] = incomeReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for _, v := range cashflowFamily.views {
		reports["cashflow "+v] = cashflowReport(request{view: v, currency: "USD", period: "monthly", level: "group"})
	}
	for _, v := range gainsViews {
		reports["gains "+v] = gainsReport(request{view: v, currency: "USD", period: "monthly"})
	}
	for name, rep := range reports {
		for _, c := range rep.columns {
			if !ident.MatchString(c.name) || !ident.MatchString(c.header) {
				t.Errorf("%s: column %q header %q has a special character", name, c.name, c.header)
			}
		}
	}
}
