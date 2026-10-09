package main

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// TestPositionCostBasisColumns pins the cost basis columns of
// `holdings positions`: opt-in, reader names over gold's (cost_basis
// for book_value), money redacted and the ratio and stamp legible
// under -p, and blank on a row without a basis, which is every cash
// row.
func TestPositionCostBasisColumns(t *testing.T) {
	t.Parallel()
	names := []string{"cost_basis", "cost_basis_ccy", "unrealized_gain", "unrealized",
		"unrealized_pct", "basis_stamp", "acquisition_date", "accrued_interest", "clean_value"}
	for _, n := range names {
		if slices.Contains(defaultColumns, n) {
			t.Errorf("%q is in the default column set; the cost basis columns are opt-in", n)
		}
	}
	registry := buildColumnRegistry("CHF")
	byName := map[string]columnSpec[gold.PositionRow]{}
	for _, c := range registry {
		byName[c.Name] = c
	}

	s := func(v string) *string { return &v }
	ratio := 0.5
	held := gold.PositionRow{
		BookValue: s("600"), BookValueOutCcy: s("300"), UnrealizedGain: s("300"),
		UnrealizedOutCcy: s("150"), UnrealizedRatio: &ratio, BasisStamp: s("stated/lots/included"),
		AcquisitionDate: s("2021-03-04"), AccruedInterest: s("12"), CleanValue: s("900"),
	}
	for name, want := range map[string]string{
		"cost_basis": "600.00", "cost_basis_ccy": "300.00", "unrealized_gain": "300.00",
		"unrealized": "150.00", "unrealized_pct": "50.00", "basis_stamp": "stated/lots/included",
		"acquisition_date": "2021-03-04", "accrued_interest": "12.00", "clean_value": "900.00",
	} {
		c := byName[name]
		if got := c.Extract(held); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if got := c.Extract(gold.PositionRow{}); got != "" {
			t.Errorf("%s on a row without a basis = %q, want empty", name, got)
		}
	}
	for name, header := range map[string]string{"cost_basis_ccy": "cost_basis_CHF", "unrealized": "unrealized_CHF"} {
		if got := byName[name].header(); got != header {
			t.Errorf("%s header = %q, want %q", name, got, header)
		}
	}
	for _, name := range []string{"cost_basis", "cost_basis_ccy", "unrealized_gain", "unrealized", "accrued_interest", "clean_value"} {
		if byName[name].Privacy != PrivacyMoney {
			t.Errorf("%s privacy = %v, want money", name, byName[name].Privacy)
		}
	}
	for _, name := range []string{"unrealized_pct", "basis_stamp", "acquisition_date"} {
		if byName[name].Privacy != PrivacyNone {
			t.Errorf("%s privacy = %v, want legible", name, byName[name].Privacy)
		}
	}
}

// TestReturnsGainColumn pins the gain column: the money the percentages
// describe, end − start − net flow, on by default, blank where a
// boundary value is missing.
func TestReturnsGainColumn(t *testing.T) {
	t.Parallel()
	if !slices.Contains(defaultReturnColumnsFor("twr"), "gain") {
		t.Error("gain is not in the default returns columns")
	}
	var gain columnSpec[gold.ReturnRow]
	for _, c := range buildReturnColumnRegistry("USD") {
		if c.Name == "gain" {
			gain = c
		}
	}
	if gain.header() != "gain_USD" || gain.Privacy != PrivacyMoney {
		t.Errorf("gain header %q privacy %v", gain.header(), gain.Privacy)
	}
	s := func(v string) *string { return &v }
	if got := gain.Extract(gold.ReturnRow{StartValue: s("1000.00"), EndValue: s("1500.00"), NetFlow: s("300.00")}); got != "200.00" {
		t.Errorf("gain = %q, want 200.00", got)
	}
	if got := gain.Extract(gold.ReturnRow{StartValue: s("1000.00"), EndValue: s("1500.00")}); got != "" {
		t.Errorf("gain without a net flow = %q, want empty", got)
	}
}
