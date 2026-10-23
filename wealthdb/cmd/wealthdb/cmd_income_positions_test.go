package main

import (
	"bytes"
	"context"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The positions view and the calendar name a holding by one key, its
// source, account and position key (yield.HoldingKey), and read the same
// amounts. On 2026-11-20, a fund that has paid every month since it was
// bought in June reads monthly: five pay months over five months held
// (docs/YIELD.md §4). So its positions rows and the calendar both expect
// it to pay again in December. Account B's payments name the fund's
// CUSIP, which the matching folds into the ticker its holding has. A
// balance at a stated rate earns interest, a second type. Each type's
// positions rows sum to the projection's, and each holding's expected
// income is its calendar rows'. Every account, holding and figure is
// invented.
func TestIncomePositionsAndTheCalendarAgree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := gold.OpenFresh(filepath.Join(dir, "wealthdb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	day := func(m, d int) string {
		return strconv.FormatInt(time.Date(2026, time.Month(m), d, 0, 0, 0, 0, time.UTC).Unix(), 10)
	}
	var txns, kinds []string
	for m := 7; m <= 11; m++ {
		for _, p := range [][3]string{{"A", "EXIN", "200"}, {"B", "000000002", "100"}} {
			id := "D" + strconv.Itoa(len(txns))
			txns = append(txns, "('brk', '"+id+"', "+day(m, 5)+", '"+p[0]+"', 'dividend', 'USD', "+p[2]+", '"+p[1]+"')")
			kinds = append(kinds, "('brk', '"+id+"', 'sig-exin', 1, 'INCOME_DIVIDENDS', 'rule', 100)")
		}
	}
	rate := `'{"CdtIntrstRate1": "1.5", "CdtLmtAmt1": "0", "CalIntrstBasisDesc": "act/360", "EndOfDurtn": "9999-12-31"}'`
	for _, q := range []string{
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('brk', 'synthetic', '/tmp/brk.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'A', 'brokerage', 'Brokerage A', 'USD', 0, 0),
		 ('brk', 'B', 'brokerage', 'Brokerage B', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol, name,
		 currency, first_seen_at, last_seen_at) VALUES
		 ('brk', 'EXIN', 'fixed_income', 'etf', 'US0000000002', '000000002', 'EXIN', 'EXAMPLE BOND INCOME ETF', 'USD', 0, 0),
		 ('brk', '000000002', 'fixed_income', 'etf', 'US0000000002', '000000002', 'EXIN', 'EXAMPLE BOND INCOME ETF', 'USD',
		  0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('brk', ` + day(6, 15) + `, 'A', 'EXIN', 'EXIN', 'fixed_income', 'etf', 'USD', 1000, 50000),
		 ('brk', ` + day(6, 15) + `, 'B', '000000002', '000000002', 'fixed_income', 'etf', 'USD', 500, 25000),
		 ('brk', ` + day(11, 20) + `, 'A', 'EXIN', 'EXIN', 'fixed_income', 'etf', 'USD', 1000, 50000),
		 ('brk', ` + day(11, 20) + `, 'B', 'EXIN', 'EXIN', 'fixed_income', 'etf', 'USD', 500, 25000)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount,
		 payload) VALUES ('brk', ` + day(6, 15) + `, 'A', 'USD', 'current', 10000, ` + rate + `),
		 ('brk', ` + day(11, 20) + `, 'A', 'USD', 'current', 10000, ` + rate + `)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		 currency, net_amount, instrument_external_id) VALUES ` + strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		 signature_version, income_detailed, provenance, assigned_at) VALUES ` + strings.Join(kinds, ", "),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := gold.RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := gold.RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	view := func(v string) []map[string]string {
		t.Helper()
		req := request{view: v, currency: "USD", from: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
			to: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - 1, asOf: time.Date(2026, 11, 20, 23, 59, 59, 0,
				time.UTC).Unix(), period: "annual", assumptions: yield.DefaultAssumptions}
		rep := incomeForwardReport(req)
		rows, err := rep.fetch(ctx, db)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		var out bytes.Buffer
		if err := writeFormatted(&out, output.FormatCSV, rows.table(nil, rep.allColumns(), false,
			output.FormatCSV)); err != nil {
			t.Fatal(err)
		}
		return csvRows(t, out.String())
	}
	num := func(r map[string]string, col string) float64 {
		v, _ := strconv.ParseFloat(r[col], 64)
		return v
	}
	var total map[string]string
	for _, r := range view("projection") {
		if r["period"] == "total" {
			total = r
		}
	}
	// Each holding's expected income is its calendar rows'.
	calendar := map[string]float64{}
	for _, r := range view("calendar") {
		if r["kind"] == "income" {
			calendar[r["account_id"]+"/"+r["position_key"]] += num(r, "value_USD")
		}
	}
	byType := map[string][2]float64{}
	for _, r := range view("positions") {
		k := r["account_id"] + "/" + r["position_key"]
		typ := "distributions"
		if strings.HasPrefix(r["position_key"], "cash:") {
			typ = "interest"
		}
		b := byType[typ]
		b[0], b[1] = b[0]+num(r, "received_USD"), b[1]+num(r, "expected_USD")
		byType[typ] = b
		if math.Abs(num(r, "expected_USD")-calendar[k]) > 0.006 {
			t.Errorf("%s: expected %s, its calendar rows %.2f", k, r["expected_USD"], calendar[k])
		}
		want := map[string]string{"A/EXIN": "1000.00 200.00 indicated", "B/EXIN": "500.00 100.00 indicated"}[k]
		if got := r["received_USD"] + " " + r["expected_USD"] + " " + r["basis"]; want != "" && got != want {
			t.Errorf("%s: %s, want %s", k, got, want)
		}
		if k == "B/000000002" {
			t.Errorf("a row of the CUSIP: %v", r)
		}
	}
	for _, typ := range []string{"distributions", "interest"} {
		got, want := byType[typ], [2]float64{num(total, typ+"_received_USD"), num(total, typ+"_expected_USD")}
		if math.Abs(got[0]-want[0]) > 0.01 || math.Abs(got[1]-want[1]) > 0.01 {
			t.Errorf("%s: the positions sum to %v, the projection %v", typ, got, want)
		}
	}
	if d, i := byType["distributions"], byType["interest"]; d != [2]float64{1500, 300} || i[1] <= 0 {
		t.Errorf("distributions %v, interest %v", d, i)
	}
}
