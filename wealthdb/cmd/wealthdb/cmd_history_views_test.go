package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
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
	// The window's bounds go by the words both fronts read.
	if _, err := callTool(t, s, "returns", map[string]any{"view": "bridge", "from": "2024-03-15"}); err == nil ||
		!strings.Contains(err.Error(), "whole months: from 2024-03-15") {
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
	if _, err := callTool(t, s, "spending", map[string]any{"view": "fees", "level": "detailed"}); err == nil ||
		!strings.Contains(err.Error(), "fees takes no level") {
		t.Errorf("level passed through MCP: %v", err)
	}
	if _, err := callTool(t, s, "spending", map[string]any{"view": "fees", "category": "groceries"}); err == nil ||
		!strings.Contains(err.Error(), "no category filter") {
		t.Errorf("category passed through MCP: %v", err)
	}
}

// A side view refuses the options of its siblings by name on both
// fronts, from one list each.
func TestSideViewsRefuseTheirSiblingsOptions(t *testing.T) {
	t.Parallel()
	cfg := withFeeLine(t, setupSpendingGold(t))
	s := onFixtureToday(newTestMCP(cfg, false))
	// A value each parameter's schema admits, so the refusal is the view's.
	mcpValue := map[string]any{"method": "twr", "period": "total", "annualize": "never", "netting": false,
		"inception": "strict", "benchmark": "x", "level": "detailed"}
	for _, c := range []struct {
		tool, view, cmd string
		names           []string
	}{
		{"returns", "stated", "returns", returnsOptionNames},
		{"returns", "bridge", "returns", bridgeRefusedFlags},
		{"returns", "private", "returns", returnsOptionNames},
		{"spending", "fees", "spending", feesRefusedFlags},
	} {
		for _, name := range c.names {
			if _, se, code := runToday(t, "-c", cfg, c.cmd, c.view, "--"+name, "x"); code == 0 ||
				!strings.Contains(se, c.view+" takes no --"+name) {
				t.Errorf("%s %s --%s: exit %d: %s", c.cmd, c.view, name, code, se)
			}
			if _, err := callTool(t, s, c.tool, map[string]any{"view": c.view, name: mcpValue[name]}); err == nil ||
				!strings.Contains(err.Error(), c.view+" takes no "+name) {
				t.Errorf("%s view=%s %s: %v", c.tool, c.view, name, err)
			}
		}
	}
	desc := ""
	for _, spec := range s.tools() {
		if spec.name == "returns" {
			desc = spec.description
		}
	}
	if !strings.Contains(desc, "it takes no method, period, annualize, netting, inception, benchmark or currency") {
		t.Errorf("the returns description does not list what stated refuses:\n%s", desc)
	}
}

// Rates and returns stay under -p; the entity and the money do not.
func TestFeesPrivacy(t *testing.T) {
	t.Parallel()
	cfg := withFeeLine(t, setupSpendingGold(t))
	args := []string{"-c", cfg, "spending", "fees", "2026-05-01", "2026-06-30", "-f", "csv"}
	out, se, code := runToday(t, append(args, "-p")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if strings.Contains(out, "75.00") || strings.Contains(out, "CARD0001234") {
		t.Errorf("-p shows an amount or an id:\n%s", out)
	}
	plain, se, code := runToday(t, args...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	want, got := csvRows(t, plain), csvRows(t, out)
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("%d rows with -p, %d without:\n%s", len(got), len(want), plain)
	}
	for i := range want {
		for _, col := range []string{"fee_rate_pct", "twr_pct"} {
			if got[i][col] != want[i][col] {
				t.Errorf("row %d: %s reads %q under -p, %q without", i, col, got[i][col], want[i][col])
			}
		}
	}
	// The fixture's fee sits on a card, which holds no value to rate, so
	// a row with a rate and a return renders through the registry too.
	rate, twr := 0.0125, 0.0431
	row := []gold.FeeRow{{EntityKind: "account", SilverSourceID: "bank", EntityID: "BRK000001234", FeeRate: &rate, TWR: &twr}}
	cols := buildFeeColumnRegistry("USD", func(string) string { return "schwab" }, accountNicknames{})
	var rates []columnSpec[gold.FeeRow]
	for _, c := range cols {
		if c.Name == "fee_rate_pct" || c.Name == "twr_pct" {
			rates = append(rates, c)
		}
	}
	for _, f := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatJSON} {
		if got := rowsToTable(row, rates, true, f, nil).Rows[0]; !slices.Equal(got, []string{"1.25", "4.31"}) {
			t.Errorf("%s: -p prints the rates %q, want 1.25 and 4.31", f, got)
		}
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

// -p redacts the two money columns of status --marks; marked_pct stays.
func TestStatusMarksPrivacy(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	args := []string{"-c", cfg, "status", "--marks", "-f", "csv"}
	plain, se, code := runToday(t, args...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	out, se, code := runToday(t, append(args, "-p")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	want, got := csvRows(t, plain), csvRows(t, out)
	if len(got) != len(want) || len(want) == 0 {
		t.Fatalf("%d rows with -p, %d without:\n%s", len(got), len(want), plain)
	}
	marked := false
	for i := range want {
		if got[i]["marked_pct"] != want[i]["marked_pct"] {
			t.Errorf("row %d: marked_pct %q under -p, %q without", i, got[i]["marked_pct"], want[i]["marked_pct"])
		}
		marked = marked || want[i]["marked_pct"] != ""
		for _, col := range []string{"value_marked_USD", "value_total_USD"} {
			if got[i][col] != "" {
				t.Errorf("row %d: %s reads %q under -p", i, col, got[i][col])
			}
		}
	}
	if !marked {
		t.Errorf("no month end has a marked_pct, which proves nothing:\n%s", plain)
	}
}

// status --marks binds a condition as the MCP marks shape does: a money
// word names value_total on both fronts, the result header says it is
// marks, and describe lists the marks columns.
func TestStatusMarksBindsAsTheServerDoes(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	out, se, code := runToday(t, "-c", cfg, "status", "--marks", "-f", "csv", "-F", "value > 0")
	if code != 0 {
		t.Fatalf("CLI: exit %d: %s", code, se)
	}
	s := onFixtureToday(newTestMCP(cfg, false))
	got, err := callTool(t, s, "status", map[string]any{"marks": true, "format": "csv", "filter": []any{"value > 0"}})
	if err != nil {
		t.Fatalf("MCP: %v", err)
	}
	head, body, _ := strings.Cut(got, "\n")
	if i := strings.LastIndex(body, "\ntotal of "); i >= 0 {
		body = body[:i]
	}
	if strings.Count(out, "\n") < 2 || body != strings.TrimRight(out, "\n") {
		t.Errorf("MCP and CLI differ\nMCP:\n%s\nCLI:\n%s", body, out)
	}
	for _, want := range []string{"marks", "value_total_USD > 0"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q lacks %q", head, want)
		}
	}
	help, err := callTool(t, s, "describe", map[string]any{"topic": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help, "  marks=true: month_end*, marked_pct*, value_marked_USD*, value_total_USD*, quality*") {
		t.Errorf("describe status does not list the marks columns:\n%s", help)
	}
	// A condition's message names the command typed, and no view: the
	// request's marks is no view a command spells.
	_, se, code = runToday(t, "-c", cfg, "status", "--marks", "-F", "no_such_column > 0")
	if code == 0 || !strings.Contains(se, `wealthdb status: -F "no_such_column > 0": this result has no column`) {
		t.Errorf("exit %d, want the command's message: %s", code, se)
	}
}
