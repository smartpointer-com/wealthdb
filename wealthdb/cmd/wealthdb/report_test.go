package main

import (
	"encoding/csv"
	"strings"
	"testing"
)

// TestPseudonymsHoldAcrossColumnsAndConditions: under -p a typed value
// reads the same pseudonym whatever -C shows and whatever rows -F keeps.
// The wallet's account and its transactions' ids share the source kind's
// prefix, so a pseudonym numbered over the printed cells alone moves
// with them.
func TestPseudonymsHoldAcrossColumnsAndConditions(t *testing.T) {
	t.Parallel()
	cfg := setupTypedLabelGold(t)
	// cell is the column col of the row whose date is day, in a csv run.
	cell := func(args []string, day, col string) string {
		t.Helper()
		so, se, code := runToday(t, append([]string{"-c", cfg, "transactions", "2026", "-p", "-f", "csv"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, se)
		}
		recs, err := csv.NewReader(strings.NewReader(so)).ReadAll()
		if err != nil || len(recs) == 0 {
			t.Fatalf("%v: %v\n%s", args, err, so)
		}
		at := map[string]int{}
		for i, h := range recs[0] {
			at[h] = i
		}
		for _, r := range recs[1:] {
			if r[at["date"]] == day && r[at["silver_source"]] == "wallets" {
				return r[at[col]]
			}
		}
		t.Fatalf("%v: no wallets row on %s\n%s", args, day, so)
		return ""
	}
	const buy = "2026-05-10"
	runs := [][]string{
		{"-C", "silver_source,date,tx_id"},
		{"-C", "all"},
		{"-C", "silver_source,date,tx_id", "-F", "kind = buy"},
		{"-C", "silver_source,date,tx_id,account", "-F", "silver_source = wallets"},
	}
	want := cell(runs[1], buy, "tx_id")
	for _, args := range runs {
		if got := cell(args, buy, "tx_id"); got != want {
			t.Errorf("%v: the buy's tx_id reads %s, with -C all %s", args, got, want)
		}
	}
	account := cell(nil, buy, "account")
	for _, args := range append(runs[1:2], runs[3], []string{"-F", "kind = buy"}) {
		if got := cell(args, buy, "account"); got != account {
			t.Errorf("%v: the wallet reads %s, unfiltered %s", args, got, account)
		}
	}
}
