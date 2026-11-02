package main

import (
	"strings"
	"testing"
)

// TestCLIFilter: -F keeps the matching rows; the table ends with their
// count and the conditions as read, csv carries the rows alone, and a
// bad condition is a usage error naming the views that print its
// column, before gold is opened.
func TestCLIFilter(t *testing.T) {
	t.Parallel()
	cfg := setupSpendingGold(t)
	out, se, code := runToday(t, "-c", cfg, "spending", "transactions", "2026-05-01", "2026-06-30",
		"-F", "merchant contains corner", "-F", "value <= -50")
	if code != 0 || !strings.Contains(out, "CornerMart") || !strings.HasSuffix(out, "1 of 5 rows match: merchant contains corner · value_USD <= -50\n") {
		t.Errorf("table (exit %d, %s):\n%s", code, se, out)
	}
	csvOut, _, code := runToday(t, "-c", cfg, "spending", "transactions", "2026-05-01", "2026-06-30", "-f", "csv",
		"-F", "merchant contains corner")
	if code != 0 || strings.Count(csvOut, "\n") != 3 || strings.Contains(csvOut, "match") {
		t.Errorf("csv:\n%s", csvOut)
	}
	empty, _, _ := runToday(t, "-c", cfg, "spending", "transactions", "2026-05-01", "2026-06-30", "-F", "category = Rent")
	if !strings.Contains(empty, "0 rows match: of 5 rows, category = Rent kept 0 (category values: ") {
		t.Errorf("empty:\n%s", empty)
	}
	_, se, code = runToday(t, "-c", cfg, "spending", "summary", "2026-05-01", "2026-06-30", "-F", "merchant = x", "-F", "value => 1")
	want := `wealthdb spending summary: -F "merchant = x": the summary view has no column "merchant"; merchant is printed by wealthdb spending transactions`
	if code != 2 || !strings.Contains(se, want) || !strings.Contains(se, `wealthdb spending summary: -F "value => 1": "=>" is not an operator; write "value >= 1"`) {
		t.Errorf("exit %d:\n%s", code, se)
	}
	_, se, code = runToday(t, "-c", cfg, "spending", "transactions", "-p", "-F", "value < -50")
	if code != 2 || !strings.Contains(se, "value_USD is redacted under -p, so it cannot select rows") {
		t.Errorf("privacy: exit %d:\n%s", code, se)
	}
	help, se, _ := runToday(t, "help", "filter")
	if !strings.Contains(help+se, "Operators (a row is kept when the cell ...)") || !strings.Contains(help+se, "-F") {
		t.Errorf("help filter:\n%s%s", help, se)
	}
}
