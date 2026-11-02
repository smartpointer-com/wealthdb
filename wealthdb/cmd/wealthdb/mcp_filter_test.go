package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPFilter: a condition on a column only a finer view prints moves
// the call there, the header echoes it in canonical form, and a
// filtered result ends with the total of its money.
func TestMCPFilter(t *testing.T) {
	t.Parallel()
	s := onFixtureToday(newTestMCP(setupSpendingGold(t), false))
	text, err := callTool(t, s, "spending", map[string]any{"filter": []any{"merchant contains corner", "value == -100"}})
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(text, "\n")
	for _, want := range []string{"spending transactions", "merchant contains corner", "value_USD = -100", "1 row",
		"view transactions chosen for the merchant condition"} {
		if !strings.Contains(head, want) {
			t.Errorf("header lacks %q: %s", want, head)
		}
	}
	if !strings.HasSuffix(text, "total of 1 row: value_USD -100.00") {
		t.Errorf("no total line:\n%s", text)
	}
	plain, err := callTool(t, s, "spending", map[string]any{"view": "transactions"})
	if err != nil || strings.Contains(plain, "total of") {
		t.Errorf("an unfiltered result has a total line: %v\n%s", err, plain)
	}
	priv := onFixtureToday(newTestMCP(setupSpendingGold(t), true))
	redacted, err := callTool(t, priv, "spending", map[string]any{"view": "transactions", "filter": "category = groceries"})
	if err != nil || strings.Contains(redacted, "total of") || !strings.Contains(redacted, "category = groceries") {
		t.Errorf("privacy endpoint: %v\n%s", err, redacted)
	}
}

// TestMCPFilterErrorsBeforeGold: every bad condition is refused at bind,
// in one reply, before gold is opened: a missing database is never
// reached.
func TestMCPFilterErrorsBeforeGold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := `{"gold_db": "` + filepath.Join(dir, "none.db") + `", "default_currency": "USD", "silver_sources": []}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestMCP(cfg, false)
	_, err := callTool(t, s, "yield", map[string]any{"view": "positions", "filter": []any{"value => 1", "maturity_date 2027", "nosuch = x"}})
	if err == nil || strings.Contains(err.Error(), "init") || strings.Count(err.Error(), "\nyield: filter ") != 2 ||
		!strings.Contains(err.Error(), `no column "nosuch"`) {
		t.Errorf("err = %v", err)
	}
}

// TestMCPFilterNamesTheViewThatPrintsIt: a column no view at or after
// the requested one prints is an error naming where it is printed.
func TestMCPFilterNamesTheViewThatPrintsIt(t *testing.T) {
	t.Parallel()
	s := onFixtureToday(newTestMCP(setupYieldGold(t), false))
	_, err := callTool(t, s, "yield", map[string]any{"view": "cash", "filter": "rating = AA"})
	if err == nil || !strings.Contains(err.Error(), "rating is printed by view=positions or view=securities") {
		t.Errorf("err = %v", err)
	}
	_, err = callTool(t, s, "yield", map[string]any{"view": "positions", "filter": "merchant = x"})
	if err == nil || !strings.Contains(err.Error(), "merchant is printed by the transactions tool") &&
		!strings.Contains(err.Error(), "merchant is printed by the spending tool's transactions view") {
		t.Errorf("another tool: %v", err)
	}
}

// TestMCPNamedFiltersYieldIncome: yield takes symbol and income takes
// kind, the views that print the key column carrying them.
func TestMCPNamedFiltersYieldIncome(t *testing.T) {
	t.Parallel()
	s := onFixtureToday(newTestMCP(setupYieldGold(t), false))
	text, err := callTool(t, s, "yield", map[string]any{"symbol": "QBF"})
	if err != nil || !strings.Contains(text, "view positions chosen for the symbol filter") || !strings.Contains(text, "| QBF |") {
		t.Errorf("yield symbol: %v\n%s", err, text)
	}
	text, err = callTool(t, s, "income", map[string]any{"view": "calendar", "kind": "income", "symbol": "QBF"})
	if err != nil || !strings.Contains(text, "12 rows") {
		t.Errorf("income kind: %v\n%s", err, text)
	}
}
