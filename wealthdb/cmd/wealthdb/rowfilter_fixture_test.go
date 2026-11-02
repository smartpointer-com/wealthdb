package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// bondRow is a synthetic row for the condition tests: a text, a money,
// a percentage, a number, a date and a period column.
type bondRow struct {
	name, category, country, maturity, period string
	value, cost                               *float64
	coupon, quantity                          *float64
}

func fp(f float64) *float64 { return &f }

// testBonds is a result set over bondRows, as a view of a tool prints it.
func testBonds(t *testing.T, privacy bool) *resultSet {
	t.Helper()
	rows := []bondRow{
		{name: "Treasury 4.25% 2027", category: "fixed_income", country: "US", maturity: "2027-03-15", period: "2026-Q1",
			value: fp(50210), cost: fp(49000), coupon: fp(4.25), quantity: fp(50)},
		{name: "Northgate Utilities 5.10%", category: "fixed_income", country: "US", maturity: "2027-06-01", period: "2026-Q1",
			value: fp(25000.5), cost: fp(26000), coupon: fp(5.1), quantity: fp(25)},
		{name: "Arden County GO", category: "fixed_income", country: "CH", maturity: "2031-11-01", period: "2026-Q2",
			value: fp(19870), cost: fp(20500), coupon: fp(3), quantity: fp(20)},
		{name: "Corner Market Fund", category: "public_equity", country: "DE", period: "2026-Q2",
			value: fp(-1200), quantity: fp(10)},
		{name: "Paper Cash", category: "cash", period: "2026-Q2"},
	}
	reg := []columnSpec[bondRow]{
		{Name: "period", Align: output.AlignLeft, Date: true, Extract: func(r bondRow) string { return r.period }},
		strCol("name", func(r bondRow) string { return r.name }),
		strCol("asset_class", func(r bondRow) string { return r.category }),
		strCol("country", func(r bondRow) string { return r.country }),
		floatMoneyCol("value", "USD", func(r bondRow) *float64 { return r.value }),
		floatMoneyCol("cost_basis", "USD", func(r bondRow) *float64 { return r.cost }),
		floatMoneyCol("unrealized_gain", "USD", func(r bondRow) *float64 {
			if r.value == nil || r.cost == nil {
				return nil
			}
			return fp(*r.value - *r.cost)
		}),
		pctCol("coupon", func(r bondRow) *float64 { return r.coupon }),
		pctCol("share", func(r bondRow) *float64 { return r.coupon }),
		decimalCol("quantity", func(r bondRow) *float64 { return r.quantity }),
		asDate(strCol("maturity_date", func(r bondRow) string { return r.maturity })),
	}
	rep := newReport(reg, columnNames(reg), func(context.Context, *sql.DB) ([]bondRow, error) { return rows, nil })
	fetched, err := rep.fetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return newResultSet(rep, fetched, privacy)
}

// bondTarget binds conditions against the bonds view on a front-end.
func bondTarget(rs *resultSet, cli bool) filterTarget {
	front := filterFront{prefix: "yield:"}
	if cli {
		front = filterFront{prefix: "wealthdb yield positions:", cli: true}
	}
	return filterTarget{view: "positions", lookup: columnLookup{cols: rs.rep.columns, currency: "USD", principal: "value"},
		privacy: rs.privacy, now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), front: front,
		elsewhere: func(col string) string {
			if col == "rating" {
				return "rating is printed by view=securities"
			}
			return ""
		}}
}

// bindAll parses and binds conditions; the error is every message.
func bindAll(t filterTarget, conds ...string) ([]condition, error) {
	raws, errs := parseConditions(conds, t.front)
	return t.bind(raws, errs)
}
