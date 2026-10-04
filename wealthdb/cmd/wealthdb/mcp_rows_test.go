package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// ---- arguments -----------------------------------------------------------

// TestParseArgsIsLenientAboutForm pins the slips small models make that
// the server reads as meant: a year as a number, an integer as a
// string, a boolean as a word, an enum in any case or by an alias, and
// "" or null for "not given".
func TestParseArgsIsLenientAboutForm(t *testing.T) {
	t.Parallel()
	params := []param{
		stringParam("from", ""),
		stringParam("period", "", reportPeriodNames...),
		{name: "limit", kind: paramInteger},
		{name: "newest_first", kind: paramBoolean},
		stringParam("to", ""),
		stringParam("search", ""),
	}
	a, err := parseArgs("t", params, json.RawMessage(`{"from": 2025, "period": "Yearly", "limit": "10",
		"newest_first": "yes", "to": "", "search": null}`))
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if a.str("from") != "2025" || a.str("period") != "annual" {
		t.Errorf("from = %q, period = %q; want 2025 and annual", a.str("from"), a.str("period"))
	}
	if n, ok := a.integer("limit"); !ok || n != 10 {
		t.Errorf("limit = %d %v, want 10", n, ok)
	}
	if !a.flag("newest_first", false) {
		t.Error("newest_first not read as true")
	}
	if a.has("to") || a.has("search") {
		t.Error(`"" and null must read as not given`)
	}
}

// TestParseArgsNamesTheFix pins the errors: each names what is wrong
// and what to send instead.
func TestParseArgsNamesTheFix(t *testing.T) {
	t.Parallel()
	params := []param{
		stringParam("view", "", "global", "accounts"),
		stringParam("as_of", ""),
		{name: "limit", kind: paramInteger},
	}
	cases := []struct{ args, want string }{
		{`{"year": 2025}`, `no parameter "year"; its parameters are view, as_of, limit`},
		{`{"asof": "2025"}`, `did you mean "as_of"?`},
		{`{"view": "summary"}`, `"summary" is not one of its values; use global or accounts`},
		{`{"limit": 2.5}`, "must be a whole number"},
		{`{"limit": "lots"}`, "must be a whole number"},
		{`[1]`, "not a JSON object"},
	}
	for _, c := range cases {
		_, err := parseArgs("holdings", params, json.RawMessage(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.args, err, c.want)
		}
	}
}

func TestNearMiss(t *testing.T) {
	t.Parallel()
	cands := []string{"twr", "mwr", "net_spend", "start", "end"}
	for in, want := range map[string]string{"twrr": "twr", "twtr": "twr", "netspend": "net_spend"} {
		if got, ok := nearMiss(in, cands); !ok || got != want {
			t.Errorf("nearMiss(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	// Too far, or as close to two candidates: no guess.
	for _, in := range []string{"quality", "wr"} {
		if got, ok := nearMiss(in, cands); ok {
			t.Errorf("nearMiss(%q) = %q, want no guess", in, got)
		}
	}
}

// ---- the row layer -------------------------------------------------------

func sp(s string) *string { return &s }

// testSpendLines is a spending-transactions result set built from typed
// rows, so the filter, sort and lookup tests run over the real registry
// without a database.
func testSpendLines(t *testing.T, privacy bool) *resultSet {
	t.Helper()
	rows := []gold.SpendTransactionRow{
		{SilverSourceID: "bank", MerchantName: sp("Corner Market"), SpendLabel: sp("Groceries"),
			SpendDetailed: sp("FOOD_AND_DRINK_GROCERIES"), ValueOutCcy: sp("-42.10"), DisplayName: sp("Everyday")},
		{SilverSourceID: "card", MerchantName: sp("Heron Air"), SpendLabel: sp("Flights"),
			SpendDetailed: sp("TRAVEL_FLIGHTS"), ValueOutCcy: sp("-900.00"), DisplayName: sp("Rewards card")},
		{SilverSourceID: "card", MerchantName: sp("Corner Market"), SpendLabel: sp("Groceries"),
			SpendDetailed: sp("FOOD_AND_DRINK_GROCERIES"), ValueOutCcy: sp("15.00"), DisplayName: sp("Rewards card")},
		{SilverSourceID: "bank", MerchantName: sp("Paper | Ink"), SpendLabel: sp("Office supplies"),
			SpendDetailed: sp("GENERAL_MERCHANDISE_OFFICE_SUPPLIES"), DisplayName: sp("Everyday")},
	}
	rep := newReport(buildSpendTransactionColumnRegistry("USD"), defaultSpendTransactionColumns,
		func(context.Context, *sql.DB) ([]gold.SpendTransactionRow, error) { return rows, nil })
	fetched, err := rep.fetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return newResultSet(rep, fetched, privacy)
}

func colIndex(t *testing.T, rs *resultSet, name string) int {
	t.Helper()
	for i, c := range rs.rep.columns {
		if c.name == name {
			return i
		}
	}
	t.Fatalf("no column %q", name)
	return -1
}

// TestFilterReadsLabelAndIDSpellings: a category filter matches the
// label and the id, an underscore reads as a space, a comma list keeps
// either, and source matches whole ids only.
func TestFilterReadsLabelAndIDSpellings(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, false)
	count := func(name, value string) int {
		return len(rs.filter([]rowFilter{newRowFilter(rs.rep, name, value)}, ""))
	}
	for value, want := range map[string]int{
		"groceries": 2, "food and drink": 2, "TRAVEL_FLIGHTS": 1, "flights,office": 2, "nothing": 0,
	} {
		if got := count("category", value); got != want {
			t.Errorf("category %q kept %d rows, want %d", value, got, want)
		}
	}
	if got := count("source", "ban"); got != 0 {
		t.Errorf("source matched a substring (%d rows); it matches whole ids", got)
	}
	if got := count("source", "bank, card"); got != 4 {
		t.Errorf("source list kept %d rows, want 4", got)
	}
	if got := len(rs.filter(nil, "heron")); got != 1 {
		t.Errorf("search kept %d rows, want 1", got)
	}
}

// TestPrivacyFiltersReadWhatIsShown: on the privacy endpoint a merchant
// filter cannot probe a name it hides.
func TestPrivacyFiltersReadWhatIsShown(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, true)
	if got := len(rs.filter([]rowFilter{newRowFilter(rs.rep, "merchant", "corner")}, "")); got != 0 {
		t.Errorf("a merchant filter matched %d redacted rows", got)
	}
	if got := len(rs.filter(nil, "heron")); got != 0 {
		t.Errorf("search matched %d redacted rows", got)
	}
}

// TestSortMoneyByMagnitude: "largest first" on spending lines, which
// are negative, means the largest amount; a blank sorts last either way.
func TestSortMoneyByMagnitude(t *testing.T) {
	t.Parallel()
	rs := testSpendLines(t, false)
	value := colIndex(t, rs, "value")
	merchant := colIndex(t, rs, "merchant")
	order := func(col int, desc bool) []string {
		idx := rs.filter(nil, "")
		rs.sortRows(idx, col, desc)
		var out []string
		for _, i := range idx {
			out = append(out, rs.cells[i][value])
		}
		return out
	}
	if got := strings.Join(order(value, true), ","); got != "-900.00,-42.10,15.00," {
		t.Errorf("descending by value = %s", got)
	}
	if got := strings.Join(order(value, false), ","); got != "15.00,-42.10,-900.00," {
		t.Errorf("ascending by value = %s", got)
	}
	// Text sorts case-insensitively; ties keep the report's order.
	if got := strings.Join(order(merchant, false), ","); got != "-42.10,15.00,-900.00," {
		t.Errorf("by merchant = %s", got)
	}
}

// TestColumnLookup pins the names a model may use for a column.
func TestColumnLookup(t *testing.T) {
	t.Parallel()
	accounts := holdingsReport(request{view: "accounts", currency: "USD"})
	returns := returnsReport(request{currency: "USD", method: "both"}, nil)
	spending := spendingReport(request{view: "categories", currency: "USD", period: "total"})
	cases := []struct {
		rep       *report
		principal string
		name      string
		sort      bool
		want      string // header of the column found
		noted     bool
	}{
		{accounts, "total_value_outccy", "total_value_USD", true, "total_value_USD", false},
		// A sort by a base-currency aggregate means its converted twin.
		{accounts, "total_value_outccy", "total_value", true, "total_value_USD", true},
		{accounts, "total_value_outccy", "total_value", false, "total_value", false},
		{accounts, "total_value_outccy", "value", true, "total_value_USD", true},
		// A misspelt name with the currency suffix still means the converted column.
		{accounts, "total_value_outccy", "totl_value_usd", false, "total_value_USD", true},
		{returns, "twr", "twrr", true, "twr_%", true},
		{returns, "twr", "twr_%", true, "twr_%", false},
		{returns, "twr", "end", true, "end_USD", true},
		{returns, "twr", "return", true, "twr_%", true},
		{spending, "net_spend", "netspend", true, "net_spend_USD", true},
		{spending, "net_spend", "amount", true, "net_spend_USD", true},
	}
	for _, c := range cases {
		l := columnLookup{cols: c.rep.columns, currency: "USD", principal: c.principal}
		i, note, err := l.resolve(c.name, c.sort)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := c.rep.columns[i].header; got != c.want || (note != "") != c.noted {
			t.Errorf("%s (sort %v) = %s, note %q; want %s, noted %v", c.name, c.sort, got, note, c.want, c.noted)
		}
	}
	// A currency suffix means the converted column, whichever currency
	// it names; the note says what the result is in.
	chf := holdingsReport(request{view: "accounts", currency: "CHF"})
	l := columnLookup{cols: chf.columns, currency: "CHF", principal: "total_value_outccy"}
	for _, c := range []struct {
		name string
		sort bool
		note string
	}{
		{"total_value_usd", false, "pass currency=USD"},
		{"totl_value_chf", false, `for "totl_value_chf"`},
		{"total_valeu", true, `for "total_valeu"`},
	} {
		i, note, err := l.resolve(c.name, c.sort)
		if err != nil || chf.columns[i].header != "total_value_CHF" || !strings.Contains(note, c.note) {
			t.Errorf("%s (CHF): %v, note %q", c.name, err, note)
		}
	}
	l = columnLookup{cols: accounts.columns, currency: "USD"}
	if _, _, err := l.resolve("colour", false); err == nil || !strings.Contains(err.Error(), "available: silver_source") {
		t.Errorf("unknown column: err = %v, want the available list", err)
	}
}

// TestColumnsParameter covers default, all, a list and a delta.
func TestColumnsParameter(t *testing.T) {
	t.Parallel()
	rep := spendingReport(request{view: "categories", currency: "USD", period: "total"})
	l := columnLookup{cols: rep.columns, currency: "USD"}
	headers := func(expr string) string {
		idx, _, err := l.pick(expr, rep.defaults)
		if err != nil {
			return "error: " + err.Error()
		}
		var hs []string
		for _, i := range idx {
			hs = append(hs, rep.columns[i].header)
		}
		return strings.Join(hs, ",")
	}
	for expr, want := range map[string]string{
		"":                              "period,category,txn_count,spend_USD,refunds_USD,net_spend_USD,share_%",
		"category,share":                "category,share_%",
		"+category_id,-period,-refunds": "category,txn_count,spend_USD,net_spend_USD,share_%,category_id",
	} {
		if got := headers(expr); got != want {
			t.Errorf("columns %q = %s, want %s", expr, got, want)
		}
	}
	if got := headers("all"); strings.Count(got, ",") != len(rep.columns)-1 {
		t.Errorf("columns all = %s", got)
	}
}

// TestEscalate: a filter moves the call to the first view, coarse to
// fine, that carries every named filter.
func TestEscalate(t *testing.T) {
	t.Parallel()
	f := holdingsFamily
	for _, c := range []struct {
		view    string
		filters []string
		want    string
		ok      bool
	}{
		{"global", nil, "global", true},
		{"global", []string{"tax_wrapper"}, "accounts", true},
		{"global", []string{"source"}, "sources", true},
		{"accounts", []string{"symbol"}, "positions", true},
		{"positions", []string{"tax_wrapper"}, "positions", false},
		{"global", []string{"symbol", "tax_wrapper"}, "global", false},
	} {
		got, ok := escalate(f.views, f.carries, c.view, c.filters)
		if got != c.want || ok != c.ok {
			t.Errorf("escalate(%s, %v) = %s %v, want %s %v", c.view, c.filters, got, ok, c.want, c.ok)
		}
	}
}

// TestEveryCarriedFilterHasAColumn holds the view-filter table against
// the registries: a filter a view claims must read a column the view
// has, or it would match nothing.
func TestEveryCarriedFilterHasAColumn(t *testing.T) {
	t.Parallel()
	s := newTestMCP("", false)
	for _, tool := range s.tools() {
		fam := tool.family
		if fam == nil {
			continue
		}
		views := fam.views
		if views == nil {
			views = []string{""}
		}
		for _, v := range views {
			req := request{view: v, currency: "USD", period: "total", level: "primary", method: "both", investing: "whole"}
			rep := fam.build(nil, req, &toolArgs{values: map[string]any{}})
			for _, f := range fam.carries[v] {
				if len(newRowFilter(rep, f, "x").cols) == 0 {
					t.Errorf("%s %s carries %s, but has none of its columns %v", tool.name, v, f, filterColumns[f])
				}
				found := false
				for _, p := range tool.params {
					found = found || p.name == f
				}
				if !found {
					t.Errorf("%s %s carries %s, which is not one of the tool's parameters", tool.name, v, f)
				}
			}
		}
	}
}

func TestThousands(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1532: "1,532", 1234567: "1,234,567", -1000: "-1,000"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %s, want %s", n, got, want)
		}
	}
}
