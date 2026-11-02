package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestConditionReadings: the canonical form each written condition
// reads as, the lenient readings included, and what a name resolved to.
func TestConditionReadings(t *testing.T) {
	t.Parallel()
	tg := bondTarget(testBonds(t, false), false)
	for _, c := range []struct {
		written string
		want    []string
	}{
		{"value > 1000", []string{"value_USD > 1000"}},
		{"value_USD >= -500.5", []string{"value_USD >= -500.5"}},
		{"value > $1,000", []string{"value_USD > 1000"}},
		{"coupon_pct > 5%", []string{"coupon_pct > 5"}},
		{"value == 50210", []string{"value_USD = 50210"}},
		{"value <> 5", []string{"value_USD != 5"}},
		{"value ≥ 100", []string{"value_USD >= 100"}},
		{"value ≤ 100", []string{"value_USD <= 100"}},
		{"value ≠ 100", []string{"value_USD != 100"}},
		{"WHERE value > 100", []string{"value_USD > 100"}},
		{"(value > 5)", []string{"value_USD > 5"}},
		{"maturity_date is null", []string{"maturity_date is empty"}},
		{"maturity_date is not null", []string{"maturity_date is not empty"}},
		{"country =", []string{"country is empty"}},
		{"name like '%cash%'", []string{"name contains cash"}},
		{"name not like '%cash%'", []string{"name not contains cash"}},
		{"asset_class in (cash, fixed_income)", []string{"asset_class in cash, fixed_income"}},
		{`asset_class in "cash, x", 'fixed_income'`, []string{`asset_class in "cash, x", fixed_income`}},
		{"name = 'Paper Cash'", []string{"name = Paper Cash"}},
		{"name = Miller's Pantry", []string{"name = Miller's Pantry"}},
		{"name = A and B Corp", []string{"name = A and B Corp"}},
		{"name = Bread and Butter is good", []string{"name = Bread and Butter is good"}},
		{"name contains credit limit fee", []string{"name contains credit limit fee"}},
		{`name = "pay order by mail"`, []string{"name = pay order by mail"}},
		{"name = İİİİ and value > 5", []string{"name = İİİİ", "value_USD > 5"}},
		{"name = KKK KKK", []string{"name = KKK KKK"}},
		{"country = US and value between 100 and 30000", []string{"country = US", "value_USD >= 100", "value_USD <= 30000"}},
		{"value between 100 and 30000 and country = US", []string{"value_USD >= 100", "value_USD <= 30000", "country = US"}},
		{"(value > 5) and (country = US)", []string{"value_USD > 5", "country = US"}},
		{"value in 1,000, 2,000", []string{"value_USD in 1000, 2000"}},
		{"quantity in 100, 200", []string{"quantity in 100, 200"}},
		{"quantity in 1,000", []string{"quantity in 1000"}},
		{"value > 5;", []string{"value_USD > 5"}},
		{"value > 5 and country = US;", []string{"value_USD > 5", "country = US"}},
		{"value in $1,000, $2,000", []string{"value_USD in 1000, 2000"}},
		{"name = ''", []string{"name is empty"}},
		{`value != ""`, []string{"value_USD is not empty"}},
		{"maturity-date = 2027", []string{"maturity_date = 2027 (2027-01-01 → 2027-12-31)"}},
		{"coupon_pct > 5 percent", []string{"coupon_pct > 5"}},
		{"value >= 100 and maturity_date < 2030", []string{"value_USD >= 100", "maturity_date < 2030 (before 2030-01-01)"}},
		{"value between 100 and 30000", []string{"value_USD >= 100", "value_USD <= 30000"}},
		{"maturity_date = 2027", []string{"maturity_date = 2027 (2027-01-01 → 2027-12-31)"}},
		{"maturity_date >= 2027-07", []string{"maturity_date >= 2027-07 (from 2027-07-01)"}},
		{"maturity_date > 2027", []string{"maturity_date > 2027 (after 2027-12-31)"}},
		{"maturity_date <= 2027-Q1", []string{"maturity_date <= 2027-Q1 (to 2027-03-31)"}},
		{"maturity_date = 2027-03-15", []string{"maturity_date = 2027-03-15"}},
		{"period = 2026-q1", []string{"period = 2026-q1 (2026-01-01 → 2026-03-31)"}},
		{"maturity_date = 2027-03-15 10:00", []string{"maturity_date = 2027-03-15 10:00 (2027-03-15)"}},
		{"maturity_date < last year", []string{"maturity_date < last year (before 2025-01-01)"}},
		{"market value > 5", nil}, // no market_value here: an error, below
	} {
		conds, err := bindAll(tg, c.written)
		if c.want == nil {
			if err == nil {
				t.Errorf("%q read, want an error", c.written)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.written, err)
			continue
		}
		if got := canonicalForms(conds); strings.Join(got, " · ") != strings.Join(c.want, " · ") {
			t.Errorf("%q reads as %q, want %q", c.written, got, c.want)
		}
	}
	conds, err := bindAll(tg, "maturty_date = 2027")
	if err != nil || conds[0].note != `maturity_date for "maturty_date"` {
		t.Errorf("a name one typo off: %v %+v", err, conds)
	}
}

// TestConditionsKeepRows: each operator over each kind, a period as the
// whole period and a period cell by its first day, money signed, a
// blank cell meeting only is empty and the negations.
func TestConditionsKeepRows(t *testing.T) {
	t.Parallel()
	rs := testBonds(t, false)
	tg := bondTarget(rs, false)
	all := rs.filter(nil, "")
	for _, c := range []struct {
		conds []string
		want  []string // the kept rows' names
	}{
		{[]string{"maturity_date = 2027"}, []string{"Treasury 4.25% 2027", "Northgate Utilities 5.10%"}},
		{[]string{"maturity_date < 2027-06"}, []string{"Treasury 4.25% 2027"}},
		{[]string{"maturity_date >= 2027-06"}, []string{"Northgate Utilities 5.10%", "Arden County GO"}},
		{[]string{"maturity_date > 2027"}, []string{"Arden County GO"}},
		{[]string{"maturity_date is empty"}, []string{"Corner Market Fund", "Paper Cash"}},
		{[]string{"period = 2026-Q2", "value > 0"}, []string{"Arden County GO"}},
		{[]string{"period >= 2026-04"}, []string{"Arden County GO", "Corner Market Fund", "Paper Cash"}},
		{[]string{"value < 0"}, []string{"Corner Market Fund"}},
		{[]string{"value <= -1200"}, []string{"Corner Market Fund"}},
		{[]string{"asset_class = Fixed Income"}, []string{"Treasury 4.25% 2027", "Northgate Utilities 5.10%", "Arden County GO"}},
		{[]string{"asset_class not in fixed_income, cash"}, []string{"Corner Market Fund"}},
		{[]string{"country != US"}, []string{"Arden County GO", "Corner Market Fund", "Paper Cash"}},
		{[]string{"name contains market"}, []string{"Corner Market Fund"}},
		{[]string{"name not contains a"}, nil},
		{[]string{"coupon_pct >= 4.25", "country in US, CH"}, []string{"Treasury 4.25% 2027", "Northgate Utilities 5.10%"}},
		{[]string{"quantity = 10"}, []string{"Corner Market Fund"}},
		{[]string{"cost_basis is not empty", "unrealized_gain > 0"}, []string{"Treasury 4.25% 2027"}},
	} {
		conds, err := bindAll(tg, c.conds...)
		if err != nil {
			t.Fatalf("%v: %v", c.conds, err)
		}
		kept, _ := keepRows(rs.cells, all, conds)
		var names []string
		for _, i := range kept {
			names = append(names, rs.cells[i][colIndex(t, rs, "name")])
		}
		if strings.Join(names, "|") != strings.Join(c.want, "|") {
			t.Errorf("%v kept %v, want %v", c.conds, names, c.want)
		}
	}
}

// TestConditionMessages is the catalogue: every message quotes the
// condition, says what is wrong, and gives the fix; the two front-ends
// share the text but for the prefix and the parameters a hint names.
func TestConditionMessages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		written  string
		mcp, cli string // cli "" when the text is the server's
	}{
		// 1. Unknown column: elsewhere, nearest, the fitting kind.
		{"rating = AA", `the positions view has no column "rating"; rating is printed by view=securities. Its text columns: name, asset_class, country`, ""},
		{"colour = red", `the positions view has no column "colour". Nearest: country ("country = red"). Its text columns: name, asset_class, country`, ""},
		{"nosuch > 5", `the positions view has no column "nosuch". Its number and money columns: value_USD, cost_basis_USD, unrealized_gain_USD, coupon_pct, share_pct, quantity`, ""},
		{"maturiti > 2027", `the positions view has no column "maturiti". Nearest: maturity_date ("maturity_date > 2027"). Its date, number and money columns: period, value_USD, cost_basis_USD, unrealized_gain_USD, coupon_pct, share_pct, quantity, maturity_date`, ""},
		{"nosuch > 1000", `the positions view has no column "nosuch". Its number and money columns: value_USD, cost_basis_USD, unrealized_gain_USD, coupon_pct, share_pct, quantity`, ""},
		// 2. No operator.
		{"maturity_date 2027", `no operator; the operators are = != < <= > >= in, not in, contains, not contains, is empty, is not empty. Write "maturity_date = 2027"`, ""},
		// 3. Near-miss operators.
		{"value => 100", `"=>" is not an operator; write "value >= 100"`, ""},
		{"value =< 100", `"=<" is not an operator; write "value <= 100"`, ""},
		{"name !contains x", `"!contains" is not an operator; write "name not contains x" (a "!" before a word is history expansion in a shell, so every negation but != is a word)`, ""},
		{"asset_class not_in cash", `"not_in" is not an operator; write "asset_class not in cash"`, ""},
		{"asset_class notin cash", `"notin" is not an operator; write "asset_class not in cash"`, ""},
		{"country is not US", `"is not" is not an operator; write "country != US"`, ""},
		// 4. Operator and kind.
		{"name > M", `">" compares numbers, money and dates; name is text, which takes =, !=, in, not in, contains, not contains, is empty, is not empty`, ""},
		{"value contains 5", `contains reads text, and value_USD is money; write "value_USD = 5"`, ""},
		// 5. Values.
		{"maturity_date = 03/15/2027", `"03/15/2027" is not a date: dates are ISO; write "maturity_date = 2027-03-15"`, ""},
		{"maturity_date = 03/04/2027", `"03/04/2027" is not a date: dates are ISO, and day and month are ambiguous here; write "maturity_date = 2027-03-04" or "maturity_date = 2027-04-03"`, ""},
		{"maturity_date = 15.03.2027", `"15.03.2027" is not a date: dates are ISO; write "maturity_date = 2027-03-15"`, ""},
		{"maturity_date = March 2027", `"March 2027" is not a date: dates are ISO; write "maturity_date = 2027-03"`, ""},
		{"maturity_date = Q1 2027", `"Q1 2027" is not a date: a quarter is written YYYY-Qn; write "maturity_date = 2027-Q1"`, ""},
		{"maturity_date = 2027-02-30", `"2027-02-30" is not a date: no such day; dates are YYYY, YYYY-Qn, YYYY-MM, YYYY-MM-DD, today, yesterday, this month, last month, this year, last year`, ""},
		{"value > 1k", `"1k" is not money: a scale is not read; write "value_USD > 1000"`, ""},
		{"value > five", `"five" is not money: a number in words is not read; write "value_USD > 5"`, ""},
		{"value > 1000 USD", `"1000 USD" is not money: a unit is not read; amounts are plain numbers in the result's currency; write "value_USD > 1000"`, ""},
		{"quantity > 5%", `"5%" is not a number: a percent sign is read on a _pct column only, whose cells are percentages; write "quantity > 5"`, ""},
		// 6. No value.
		{"value >", `">" needs a value after it, such as "value_USD > 1000"`, ""},
		{"maturity_date >", `">" needs a value after it, such as "maturity_date > 2027"`, ""},
		{"name like '%'", `"like" needs a value after it, such as "name contains x"`, ""},
		{"country in", `"in" needs a comma list after it, such as "country in a, b"`, ""},
		// 7. or.
		{"asset_class = cash or asset_class = fixed_income", `every condition must hold, so "or" cannot join them; for one column write "asset_class in cash, fixed_income"`, ""},
		{"country = US or value > 100", `every condition must hold, so "or" cannot join them; make one call with filter ["country = US"] and one with filter ["value > 100"]`,
			`every condition must hold, so "or" cannot join them; run the command once with -F "country = US" and once with -F "value > 100"`},
		// 8. Parentheses.
		{"(value > 5 or country = US) and name = x", `every condition must hold, so "or" cannot join them; make one call with filter ["value > 5", "name = x"] and one with filter ["country = US", "name = x"]`,
			`every condition must hold, so "or" cannot join them; run the command once with -F "value > 5" -F "name = x" and once with -F "country = US" -F "name = x"`},
		{"(country = US or country = CH) and value > 5", `every condition must hold, so "or" cannot join them; for one column write ["country in US, CH", "value > 5"]`,
			`every condition must hold, so "or" cannot join them; for one column write -F "country in US, CH" -F "value > 5"`},
		{"country = US or value > 5 or name = x", `every condition must hold, so "or" cannot join them; make one call with each of filter ["country = US"], filter ["value > 5"] and filter ["name = x"]`,
			`every condition must hold, so "or" cannot join them; run the command once with each of -F "country = US", -F "value > 5" and -F "name = x"`},
		{"name = Arts or Crafts", `every condition must hold, so "or" cannot join them; for any of the values write "name in Arts, Crafts"; a value that holds "or" goes in quotes: "name = \"Arts or Crafts\""`, ""},
		{"country in ()", `"in" needs a comma list after it, such as "country in a, b"`, ""},
		// Review follow-up: ambiguous lists, between, is alone, quotes, a trailing and.
		{"quantity in 100,200", `"100,200" may be 2 numbers or a number with thousands commas; write "quantity in 100, 200" or "quantity = 100200"`, ""},
		{"quantity in 100,200,5", `"100,200,5" may be 3 numbers or a number with thousands commas; write "quantity in 100, 200, 5" or "quantity in 100200, 5"`, ""},
		{"value between 5", `between takes two values joined by and; write "value between 5 and 10", which reads as "value >= 5" and "value <= 10"`, ""},
		{"value between 5 and", `between takes two values joined by and; write "value between 5 and 10", which reads as "value >= 5" and "value <= 10"`, ""},
		{"value between 50", `between takes two values joined by and; write "value between 50 and 100", which reads as "value >= 50" and "value <= 100"`, ""},
		{"name is", `"is" needs a word after it; write "name is empty", or "name = x" for a value`, ""},
		{"name is not", `"is not" needs a word after it; write "name is not empty", or "name != x" for a value`, ""},
		{"name in 'GOLD, Silver", `the ' before GOLD, never closes; close it: "name in 'GOLD, Silver'"`, ""},
		{"value > 5 and", `nothing follows the "and"; write "value > 5", and each other condition in full as its own item`, ""},
		{"country in (US, CH", `a parenthesis around the list never closes; write "country in US, CH"`, ""},
		{`"`, `the " at the end never closes; write column op value, such as "name = Miller's Pantry": a value needs no quotes`, ""},
		{"value > (5)", "", ""}, // a value in parentheses reads as text and fails as money below
		// 9. An unclosed quote.
		{`name = "Paper Cash`, `the " before Paper never closes; a value needs no quotes: "name = Paper Cash"`, ""},
		// 10. Two columns.
		{"value > cost_basis", `a condition compares a column with a constant, and cost_basis_USD is a column; unrealized_gain_USD holds the comparison: "unrealized_gain_USD > 0"`, ""},
		{"value < cost_basis", `a condition compares a column with a constant, and cost_basis_USD is a column; unrealized_gain_USD holds the comparison: "unrealized_gain_USD < 0"`, ""},
		{"maturity_date > period", `a condition compares a column with a constant, and period is a column; compare with a date, such as "maturity_date > 2027"`, ""},
		{"maturity_date = 31/02/2027", `"31/02/2027" is not a date: no such day; dates are YYYY, YYYY-Qn, YYYY-MM, YYYY-MM-DD, today, yesterday, this month, last month, this year, last year`, ""},
		{"maturity_date = 13/02/2027", `"13/02/2027" is not a date: dates are ISO; write "maturity_date = 2027-02-13"`, ""},
		{"coupon_pct > 50 bps", `"50 bps" is not a number: basis points are not read; the column is in percent; write "coupon_pct > 0.5"`, ""},
		{"month(maturity_date) = 3", `a date compares by period, and a month needs its year; write "maturity_date = YYYY-MM" with the year`, ""},
		// 11. SQL that is no condition.
		{"value > 0 ORDER BY value", `ORDER BY is not a condition; sort orders the rows: sort="-value"`,
			`ORDER BY is not a condition; the CLI prints the report's own order; pipe -f csv into sort`},
		{"value > 0 LIMIT 5", `LIMIT is not a condition; limit and offset page the rows: limit=5`, `LIMIT is not a condition; pipe the output into head`},
		{"value > 0 GROUP BY country", `GROUP BY is not a condition; a view groups: spending categories, income types, a period of monthly, quarterly or annual`, ""},
		{"SELECT name", `SELECT is not a condition; columns picks the columns: columns="+coupon,maturity_date"`, `SELECT is not a condition; -C picks the columns: -C +coupon,maturity_date`},
		{"lower(name) = paper cash", `= ignores case already; write "name = paper cash"`, ""},
		{"year(maturity_date) = 2027", `a date compares by period; write "maturity_date = 2027"`, ""},
		{"abs(value) > 500", `money compares signed: spending lines are negative, so "over 500" is "value <= -500"; a size either way takes one call per sign`, ""},
	}
	for _, cli := range []bool{false, true} {
		tg := bondTarget(testBonds(t, false), cli)
		prefix := `yield: filter `
		if cli {
			prefix = `wealthdb yield positions: -F `
		}
		for _, c := range cases {
			_, err := bindAll(tg, c.written)
			if err == nil {
				t.Errorf("%q read, want an error", c.written)
				continue
			}
			want := c.mcp
			if cli && c.cli != "" {
				want = c.cli
			}
			if want == "" {
				continue
			}
			if want = prefix + strconv.Quote(c.written) + ": " + want; err.Error() != want {
				t.Errorf("cli %v %q:\n got %s\nwant %s", cli, c.written, err, want)
			}
		}
	}
	// 12. A redacted column, on the privacy endpoint and under -p.
	for _, cli := range []bool{false, true} {
		_, err := bindAll(bondTarget(testBonds(t, true), cli), "value > 1000")
		where := "on this endpoint"
		if cli {
			where = "under -p"
		}
		if err == nil || !strings.HasSuffix(err.Error(), `value_USD is redacted `+where+`, so it cannot select rows; a share can: "share_pct > 5"`) {
			t.Errorf("redacted, cli %v: %v", cli, err)
		}
		// A view with no share names its percentage as one.
		rs := testBonds(t, true)
		tg := bondTarget(rs, cli)
		var cols []reportColumn
		for _, c := range tg.lookup.cols {
			if c.header != "share_pct" {
				cols = append(cols, c)
			}
		}
		tg.lookup.cols = cols
		if _, err := bindAll(tg, "value > 1000"); err == nil || !strings.HasSuffix(err.Error(), `a percentage can: "coupon_pct > 5"`) {
			t.Errorf("redacted without a share, cli %v: %v", cli, err)
		}
	}
	// Every bad condition in one reply, one line each.
	_, err := bindAll(bondTarget(testBonds(t, false), false), "value => 1", "country = US", "nosuch = 1", "value >")
	if err == nil || strings.Count(err.Error(), "\n") != 2 {
		t.Errorf("three bad conditions in one reply:\n%v", err)
	}
}

// TestObjectFilterRestated: an object sent as filter, in each dialect a
// model writes, is refused with its conditions as strings.
func TestObjectFilterRestated(t *testing.T) {
	t.Parallel()
	params := []param{{name: "filter", kind: paramStrings}}
	for _, c := range []struct{ raw, want string }{
		{`{"filter": {"value": {"$gte": 100}}}`, `["value >= 100"]`},
		{`{"filter": {"value": {"gte": 100, "lt": 500}}}`, `["value >= 100", "value < 500"]`},
		{`{"filter": {"value": {">=": 4}}}`, `["value >= 4"]`},
		{`{"filter": {"acquisition_date": "< 2020-01-01"}}`, `["acquisition_date < 2020-01-01"]`},
		{`{"filter": {"category": "Rent"}}`, `["category = Rent"]`},
		{`{"filter": {"category": {"$nin": ["Rent", "Groceries"]}}}`, `["category not in Rent, Groceries"]`},
		{`{"filter": [{"rating": {"$exists": false}}]}`, `["rating is empty"]`},
		{`{"filter": ["country = US", {"value": {"$gt": 2}}]}`, `["country = US", "value > 2"]`},
		{`{"filter": [{"column": "value", "op": ">", "value": 100}]}`, `["value > 100"]`},
		{`{"filter": {"category": {"$in": ["a, b", "c"]}}}`, `["category in \"a, b\", c"]`},
		{`{"filter": {"value": {"$ne": null}}}`, `["value is not empty"]`},
		{`{"filter": {"$and": [{"country": "US"}, {"value": {"gt": 5}}]}}`, `["country = US", "value > 5"]`},
	} {
		_, err := parseArgs("yield", params, json.RawMessage(c.raw))
		want := "yield: filter is a list of condition strings, not an object; send " + c.want
		if err == nil || err.Error() != want {
			t.Errorf("%s:\n got %v\nwant %s", c.raw, err, want)
		}
	}
	_, err := parseArgs("yield", params, json.RawMessage(`{"filter": {"$or": [{"country": "US"}, {"country": "CH"}]}}`))
	if want := `yield: filter is a list of condition strings, not an object, and every condition must hold, so $or cannot join them; ` +
		`make one call with filter ["country = US"] and one with filter ["country = CH"]`; err == nil || err.Error() != want {
		t.Errorf("$or:\n got %v\nwant %s", err, want)
	}
	a, err := parseArgs("yield", params, json.RawMessage(`{"filter": "value > 5"}`))
	if err != nil || len(a.strs("filter")) != 1 {
		t.Errorf("one string is a list of one: %v %v", a, err)
	}
}

// TestEmptiedNote names the condition that emptied the rows, the values
// its column takes nearest first, and the mirrored money bound.
func TestEmptiedNote(t *testing.T) {
	t.Parallel()
	rs := testBonds(t, false)
	tg := bondTarget(rs, false)
	all := rs.filter(nil, "")
	for _, c := range []struct {
		conds []string
		want  string
	}{
		{[]string{"asset_class = fixed_income", "country = United States"},
			"of 5 rows, asset_class = fixed_income kept 3, then country = United States kept 0 (country values: US, CH)"},
		{[]string{"value > 1200", "value < 2000"}, "of 5 rows, value_USD > 1200 kept 3, then value_USD < 2000 kept 0"},
		{[]string{"asset_class = cash", "value > 1000"}, "of 5 rows, asset_class = cash kept 1, then value_USD > 1000 kept 0"},
		{[]string{"asset_class = public_equity", "value >= 1200"},
			"of 5 rows, asset_class = public_equity kept 1, then value_USD >= 1200 kept 0 (money is signed; value_USD <= -1200 keeps 1)"},
	} {
		conds, err := bindAll(tg, c.conds...)
		if err != nil {
			t.Fatal(err)
		}
		if got := emptiedNote(rs.cells, all, conds); got != c.want {
			t.Errorf("%v:\n got %s\nwant %s", c.conds, got, c.want)
		}
	}
}

// TestTotals: the total line sums the shown output-currency money
// columns over every matching row; a balance view adds up within one
// period only; a view that never adds up has none.
func TestTotals(t *testing.T) {
	t.Parallel()
	rs := testBonds(t, false)
	cols := []int{colIndex(t, rs, "name"), colIndex(t, rs, "value"), colIndex(t, rs, "coupon")}
	q1 := []int{0, 1}
	both := []int{0, 1, 2}
	got := totals(rs, q1, cols, "USD", balanceTotal)
	if len(got) != 1 || got[0] != (columnTotal{"value_USD", "75210.50"}) {
		t.Errorf("one period's balance = %v", got)
	}
	if got := totals(rs, both, cols, "USD", balanceTotal); got != nil {
		t.Errorf("a balance over two periods totals %v", got)
	}
	if got := totals(rs, both, cols, "USD", flowTotal); len(got) != 1 || got[0].sum != "95080.50" {
		t.Errorf("a flow over two periods = %v", got)
	}
	if got := totals(rs, both, cols, "USD", noTotal); got != nil {
		t.Errorf("no total = %v", got)
	}
	p := page{idx: q1, totals: totals(rs, q1, cols, "USD", balanceTotal)}
	if line := p.totalLine(); line != "total of 2 rows: value_USD 75210.50\n" {
		t.Errorf("total line = %q", line)
	}
}

// TestFilterHelpListsEveryOperator: the help, the schema doc and the
// messages read one table.
func TestFilterHelpListsEveryOperator(t *testing.T) {
	t.Parallel()
	for _, op := range operators {
		for _, text := range []string{filterHelp(false), filterHelp(true), filterInstruction(), instructionsBody, operatorNames()} {
			if !strings.Contains(text, op.name) {
				t.Errorf("%q is missing from:\n%s", op.name, text)
			}
		}
	}
	if !strings.Contains(filterHelp(true), "-F") || strings.Contains(filterHelp(false), "-F ") {
		t.Error("each help names its own front-end's parameter")
	}
}
