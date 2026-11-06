package main

import (
	"strings"
	"testing"
	"time"
)

func TestBridgeRefusesTheFlagsThatDoNotApply(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	for flagName, value := range map[string]string{"--method": "twr", "--netting": "off", "--inception": "strict",
		"--benchmark": "any", "--annualize": "never"} {
		_, se, code := runToday(t, "-c", cfg, "returns", "bridge", flagName, value)
		if code == 0 || !strings.Contains(se, "bridge takes no "+flagName) {
			t.Errorf("%s: exit %d: %s", flagName, code, se)
		}
	}
}

func TestBridgeWindowIsWholeMonths(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, time.May, 18, 23, 59, 59, 0, time.UTC).Unix()
	at := func(y int, m time.Month, d int, end bool) int64 {
		if end {
			return time.Date(y, m, d, 23, 59, 59, 0, time.UTC).Unix()
		}
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
	}
	cases := []struct {
		name     string
		from, to int64
		ok       bool
	}{
		{"since inception to today", 0, today, true},
		{"a calendar year", at(2025, time.January, 1, false), at(2025, time.December, 31, true), true},
		{"a leap February", at(2024, time.February, 1, false), at(2024, time.February, 29, true), true},
		{"to today, mid-month", at(2026, time.January, 1, false), today, true},
		{"to the future", at(2026, time.January, 1, false), at(2026, time.December, 15, true), true},
		{"from mid-month", at(2025, time.March, 15, false), at(2025, time.December, 31, true), false},
		{"to mid-month", at(2025, time.March, 1, false), at(2025, time.June, 15, true), false},
	}
	for _, c := range cases {
		if err := checkBridgeWindow(c.from, c.to, today); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// The default columns are the design's, and -C adds the two parts of
// net_saved. A bucket holds its identity as printed.
func TestBridgeColumns(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	out, se, code := runToday(t, "-c", cfg, "returns", "bridge", "-f", "csv", "--period", "annual")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	header, _, _ := strings.Cut(out, "\n")
	want := "period,start_net_worth_USD,net_saved_USD,onboarded_USD,gain_USD,end_net_worth_USD,quality"
	if header != want {
		t.Errorf("header = %s", header)
	}
	out, se, code = runToday(t, "-c", cfg, "returns", "bridge", "-f", "csv", "-C", "+net_flow,+debt_repaid")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if header, _, _ := strings.Cut(out, "\n"); !strings.Contains(header, "net_flow_USD") || !strings.Contains(header, "debt_repaid_USD") {
		t.Errorf("header = %s", header)
	}
}

func TestBridgeInMCPRefusesWhatTheCLIRefuses(t *testing.T) {
	t.Parallel()
	s := onFixtureToday(newTestMCP(setupReturnsGold(t), false))
	if _, err := callTool(t, s, "returns", map[string]any{"view": "bridge", "method": "twr"}); err == nil ||
		!strings.Contains(err.Error(), "bridge takes no method") {
		t.Errorf("method: %v", err)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "bridge", "from": "2024-03-15"}); err == nil ||
		!strings.Contains(err.Error(), "whole months") {
		t.Errorf("mid-month: %v", err)
	}
	if _, err := callTool(t, s, "returns", map[string]any{"view": "bridge", "source": "schwab"}); err == nil {
		t.Errorf("a bridge exists for the whole household only, yet source passed")
	}
}

func TestBridgePrivacyRedactsTheMoney(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	plain, _, code := runToday(t, "-c", cfg, "returns", "bridge", "--period", "total", "-f", "csv")
	if code != 0 {
		t.Fatal(plain)
	}
	out, se, code := runToday(t, "-c", cfg, "returns", "bridge", "--period", "total", "-f", "csv", "-p")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	lines := strings.Split(strings.TrimSpace(plain), "\n")
	cell := strings.Split(lines[1], ",")[1]
	if strings.Contains(out, cell) {
		t.Errorf("-p shows %s:\n%s", cell, out)
	}
}

func TestFeesRefusesAPeriodTooShortForARate(t *testing.T) {
	t.Parallel()
	cfg := setupSpendingGold(t)
	if _, se, code := runToday(t, "-c", cfg, "spending", "fees", "--period", "daily"); code == 0 {
		t.Errorf("daily passed: %s", se)
	}
	s := onFixtureToday(newTestMCP(cfg, false))
	if _, err := callTool(t, s, "spending", map[string]any{"view": "fees", "period": "weekly"}); err == nil {
		t.Errorf("weekly passed through MCP")
	}
	if _, err := callTool(t, s, "spending", map[string]any{"view": "fees", "level": "detailed"}); err == nil {
		t.Errorf("level passed through MCP")
	}
}

// Rates and returns stay under -p; the entity and the money do not.
func TestFeesPrivacy(t *testing.T) {
	t.Parallel()
	cfg := withFeeLine(t, setupSpendingGold(t))
	out, se, code := runToday(t, "-c", cfg, "spending", "fees", "2026-05-01", "2026-06-30", "-f", "csv", "-p")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if strings.Contains(out, "75.00") || strings.Contains(out, "CARD0001234") {
		t.Errorf("-p shows an amount or an id:\n%s", out)
	}
}

func TestStatusMarksFlags(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	if _, se, code := runToday(t, "-c", cfg, "status", "-f", "csv"); code == 0 || !strings.Contains(se, "--marks") {
		t.Errorf("-f without --marks: exit %d: %s", code, se)
	}
	if _, se, code := runToday(t, "-c", cfg, "status", "--marks", "-v"); code == 0 {
		t.Errorf("--marks -v passed: %s", se)
	}
	out, se, code := runToday(t, "-c", cfg, "status", "--marks", "-f", "csv", "-x", "CHF")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if header, _, _ := strings.Cut(out, "\n"); header != "month_end,marked_pct,value_marked_CHF,value_total_CHF,quality" {
		t.Errorf("header = %s", header)
	}
	s := onFixtureToday(newTestMCP(cfg, false))
	if _, err := callTool(t, s, "status", map[string]any{"marks": true, "source": "schwab"}); err == nil {
		t.Errorf("marks with a source passed through MCP")
	}
	if _, err := callTool(t, s, "status", map[string]any{"from": "2024"}); err == nil {
		t.Errorf("a window without marks passed through MCP")
	}
}
