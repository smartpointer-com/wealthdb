package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// setupYieldGold seeds a gold with a bond its description gives terms,
// a cash balance whose payload states a tiered rate, a stock, a bond
// fund its page describes, and a dollar curve, and returns a config path
// for it. Every account, holding and figure is invented.
func setupYieldGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('brk', 'synthetic', '/tmp/brk.db', -1, 0, 0), ('rates', 'synthetic', '/tmp/rates.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'ACCT7781', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, cusip, name,
		 currency, first_seen_at, last_seen_at) VALUES
		 ('brk', 'QBND', 'fixed_income', 'bond', NULL, '000000AB1', 'QORVANT CORP NOTE 4.00000% 06/30/2031', 'USD', 0, 0),
		 ('brk', 'QSTK', 'public_equity', 'stock', 'QSTK', NULL, 'Qorvant Corp', 'USD', 0, 0),
		 ('brk', 'QBF', 'fixed_income', 'etf', 'QBF', NULL, 'Example Bond ETF', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('brk', ?, 'ACCT7781', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 10000, 10000),
		 ('brk', ?, 'ACCT7781', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 2000),
		 ('brk', ?, 'ACCT7781', 'QBF', 'QBF', 'fixed_income', 'etf', 'USD', 30, 3000)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		 ('ref', 'symbol:QBF', ?, 'inbox', 'yield_to_maturity', '', 4.0),
		 ('ref', 'symbol:QBF', ?, 'inbox', 'effective_duration', '', 6.0),
		 ('ref', 'symbol:QBF', ?, 'inbox', 'distribution_yield', '', 3.6)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount, payload)
		 VALUES ('brk', ?, 'ACCT7781', 'USD', 'current', 5000,
		  '{"CdtIntrstRate1": "1.5", "CdtLmtAmt1": "0", "CalIntrstBasisDesc": "act/360", "EndOfDurtn": "9999-12-31"}')`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES
		 ('rates', 'USD-3M', ?, 4.0), ('rates', 'USD-1Y', ?, 4.0), ('rates', 'USD-10Y', ?, 4.0)`,
		`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
		 ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '1Y', 12, 'rates', 'USD-1Y'), ('USD', '10Y', 120, 'rates', 'USD-10Y')`,
	} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = snap
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := gold.RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := gold.RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfg, []byte(`{"gold_db": "`+goldPath+`", "default_currency": "USD", "silver_sources": []}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// addPrivateFund adds a fund no public key names to the gold of the
// config at cfg: a holding of the fixture's account, invented.
func addPrivateFund(t *testing.T, cfg string) {
	t.Helper()
	changeYieldGold(t, cfg,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'QPF', 'fixed_income', 'fund', 'Example Private Credit', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('brk', ?, 'ACCT7781', 'QPF', 'QPF', 'fixed_income', 'fund', 'USD', 1, 1500)`)
}

// changeYieldGold runs stmts on the gold of the config at cfg, each `?`
// the fixture's snapshot, then rebuilds the exposure and yield passes.
func changeYieldGold(t *testing.T, cfg string, stmts ...string) {
	t.Helper()
	db, err := gold.Open(filepath.Join(filepath.Dir(cfg), "wealthdb.db"), gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range stmts {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = snap
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := gold.RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := gold.RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func runYield(t *testing.T, cfg string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"-c", cfg, "yield"}, args...), nil, &out, &errb)
	return out.String(), errb.String(), code
}

// csvRows reads a csv report into header-keyed rows.
func csvRows(t *testing.T, out string) []map[string]string {
	t.Helper()
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(recs) == 0 {
		t.Fatalf("csv: %v\n%s", err, out)
	}
	var rows []map[string]string
	for _, r := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = r[i]
		}
		rows = append(rows, m)
	}
	return rows
}

func TestYieldGlobalAndConverted(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	out, errb, code := runYield(t, cfg, "global", "-d", "2026-06-30", "-f", "csv", "-C", "all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	rows := csvRows(t, out)
	if len(rows) != 2 || rows[0]["currency"] != "USD" || rows[1]["currency"] != "total" {
		t.Fatalf("rows %v", rows)
	}
	usd := rows[0]
	// The bond at par yields its coupon; the cash its 1.5%; the stock
	// paid nothing in the window it was held and implies none. Weighted
	// by value over the holdings that state one.
	if usd["value_USD"] != "20000.00" || usd["benchmark_pct"] != "4.00" || usd["implied_yield_pct"] != "3.31" {
		t.Errorf("USD row %v", usd)
	}
	out, _, code = runYield(t, cfg, "global", "-d", "2026-06-30", "--converted", "-f", "csv")
	if code != 0 || len(csvRows(t, out)) != 1 || !strings.Contains(out, "20000.00") {
		t.Errorf("converted: %d\n%s", code, out)
	}
}

func TestYieldPositionsReadTheTerms(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	out, errb, code := runYield(t, cfg, "positions", "-d", "2026-06-30", "-f", "csv", "-C",
		"+coupon,maturity_date,price,coupon_kind")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var bond map[string]string
	for _, r := range csvRows(t, out) {
		if r["kind"] == "bond" {
			bond = r
		}
	}
	if bond == nil || bond["coupon_pct"] != "4.00" || bond["maturity_date"] != "2031-06-30" ||
		bond["implied_yield_pct"] != "4.00" || bond["coupon_kind"] != "fixed" || bond["price"] != "100.0000" {
		t.Errorf("bond %v", bond)
	}
}

func TestYieldCashCurveAndCoverage(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	out, errb, code := runYield(t, cfg, "cash", "-d", "2026-06-30", "-f", "csv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	cash := csvRows(t, out)
	if len(cash) != 1 || cash[0]["rate_pct"] != "1.50" || cash[0]["gap_pct"] != "-2.50" {
		t.Errorf("cash %v", cash)
	}
	out, _, code = runYield(t, cfg, "curve", "-d", "2026-06-30", "-f", "csv")
	rows := csvRows(t, out)
	if code != 0 || len(rows) != 5 || rows[0]["tenor"] != "3M" {
		t.Errorf("curve %d %v", code, rows)
	}
	out, _, code = runYield(t, cfg, "coverage", "-d", "2026-06-30", "-f", "ndjson")
	if code != 0 || !strings.Contains(out, `{"silver_source_id":"brk","instrument":"QBND","rating":{},`) {
		t.Errorf("skeletons %d\n%s", code, out)
	}
	// A note prints as the name reads, as exposure's skeletons do.
	changeYieldGold(t, cfg, `UPDATE instruments SET name = 'QORVANT <A&B> CORP NOTE 4.00000% 06/30/2031'
	    WHERE instrument_external_id = 'QBND'`)
	out, _, code = runYield(t, cfg, "coverage", "-d", "2026-06-30", "-f", "ndjson")
	if code != 0 || !strings.Contains(out, `"note":"QORVANT <A&B> CORP NOTE 4.00000% 06/30/2031"`) {
		t.Errorf("a note with <, > and & %d\n%s", code, out)
	}
}

// The coverage view's record_day lines count the dividends the readings
// rate by where their record day comes from: QSTK's, from the dividend
// calendar, is stated. Its count stays legible under -p.
func TestYieldCoverageCountsTheRecordDays(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	changeYieldGold(t, cfg,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		 currency, net_amount, instrument_external_id, quantity) VALUES ('brk', 'D1', ? - 86400 * 10, 'ACCT7781',
		 'dividend', 'USD', 5, 'QSTK', 0)`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		 signature_version, income_detailed, provenance, assigned_at)
		 VALUES ('brk', 'D1', 'sig-QSTK', 1, 'INCOME_DIVIDENDS', 'rule', 100)`,
		`INSERT INTO dividend_calendar (silver_source_id, ref, provider, event_id, as_of, kind, record_day, pay_day)
		 VALUES ('ref', 'symbol:QSTK', 'sec-edgar', 'e1', ?, 'dividend', '2026-06-05', '2026-06-20')`)
	for _, privacy := range []bool{false, true} {
		args := []string{"coverage", "-d", "2026-06-30", "-f", "csv"}
		if privacy {
			args = append(args, "-p")
		}
		out, errb, code := runYield(t, cfg, args...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		var got []string
		for _, r := range csvRows(t, out) {
			if r["section"] == "record_day" {
				got = append(got, r["reason"]+" "+r["payments"]+" "+r["holdings"])
			}
		}
		if strings.Join(got, "|") != "stated 1 1" {
			t.Errorf("-p %v: record_day lines %q\n%s", privacy, got, out)
		}
	}
}

func TestYieldPrivacyAndFlags(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	addPrivateFund(t, cfg)
	out, _, code := runYield(t, cfg, "positions", "-d", "2026-06-30", "-p", "-f", "csv", "-C", "+account_id")
	if code != 0 || strings.Contains(out, "ACCT7781") || !strings.Contains(out, "4.00") {
		t.Errorf("privacy: %d\n%s", code, out)
	}
	// A fund no public key names is listed by a key of the holding's own,
	// hidden whole.
	out, _, code = runYield(t, cfg, "funds", "-d", "2026-06-30", "-f", "csv")
	if code != 0 || !strings.Contains(out, gold.HoldingKeyPrefix+"brk:QPF") {
		t.Errorf("a holding's own key %d\n%s", code, out)
	}
	for _, view := range []string{"funds", "securities"} {
		out, _, code = runYield(t, cfg, view, "-d", "2026-06-30", "-p", "-f", "csv")
		if code != 0 || strings.Contains(out, "QPF") {
			t.Errorf("%s -p shows the holding's key %d\n%s", view, code, out)
		}
	}
	// No amount, quantity, price, DV01 or income prints under -p; a rate
	// and the public keys do.
	for _, view := range []string{"positions", "securities", "funds", "coverage"} {
		out, _, code := runYield(t, cfg, view, "-d", "2026-06-30", "-p", "-f", "csv")
		for _, amount := range []string{"10000.00", "5000.00", "3000.00", "2000.00", "100.0000", "200.0000"} {
			if code != 0 || strings.Contains(out, amount) {
				t.Errorf("%s -p shows %s: %d\n%s", view, amount, code, out)
			}
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"positions", "--converted"}, "--converted belongs to the global, sources, portfolios and accounts views"},
		{[]string{"positions", "--period", "quarterly", "2026"}, "--period belongs to the global, sources"},
		{[]string{"global", "--period", "weekly"}, "weekly"},
		{[]string{"global", "-f", "ndjson"}, "-f ndjson belongs to the coverage view"},
		{[]string{"coverage", "-f", "ndjson", "-C", "section"}, "it takes no -C"},
		{[]string{"coverage", "-f", "ndjson", "--period", "quarterly"}, "it takes no --period"},
		{[]string{"global", "--period", "quarterly", "-d", "2026-06-30"}, "-d names one day"},
		{[]string{"coverage", "-f", "ndjson", "-p"}, "which -p hides"},
	} {
		if _, errb, code := runYield(t, cfg, c.args...); code != 2 || !strings.Contains(errb, c.want) {
			t.Errorf("%v: exit %d, want 2 and %q:\n%s", c.args, code, c.want, errb)
		}
	}
	out, errb, code := runYield(t, cfg, "global", "--period", "quarterly", "2026-04-01", "2026-06-30", "-f", "csv")
	if code != 0 || !strings.Contains(out, "period,currency") || !strings.Contains(out, "2026-Q2") {
		t.Errorf("series %d %s\n%s", code, errb, out)
	}
	// The curve's series: the period before each currency and tenor; May,
	// before the first snapshot, has no rows.
	out, errb, code = runYield(t, cfg, "curve", "--period", "monthly", "2026-05-01", "2026-06-30", "-f", "csv")
	if rows := csvRows(t, out); code != 0 || len(rows) != 5 || rows[0]["period"] != "2026-06" || rows[0]["tenor"] != "3M" {
		t.Errorf("curve series %d %s\n%s", code, errb, out)
	}
}

// Each view's default column set resolves against its registry, and the
// usage lists every view with its columns and defaults, and names the
// views --period and --converted apply to, every flag line within the
// usage's width.
func TestYieldUsageListsEveryView(t *testing.T) {
	t.Parallel()
	usage := yieldUsage()
	_, columns, _ := strings.Cut(usage, "Available columns (per view):")
	_, defaults, _ := strings.Cut(columns, "Default column sets:")
	for _, view := range yieldViews {
		if _, err := yieldReport(request{view: view, currency: "USD"}).pick("default"); err != nil {
			t.Errorf("%s defaults: %v", view, err)
		}
		for what, text := range map[string]string{"views": usage, "columns": columns, "defaults": defaults} {
			if !strings.Contains(text, "\n  "+view+" ") {
				t.Errorf("usage does not list the %s view under its %s", view, what)
			}
		}
	}
	flat := strings.Join(strings.Fields(usage), " ")
	for _, want := range []string{"a period column first; on " + andList(yieldSeriesViews),
		"currencies; on " + andList(yieldAggregateViews)} {
		if !strings.Contains(flat, want) {
			t.Errorf("usage does not say %q", want)
		}
	}
	for _, line := range strings.Split(usage, "\n") {
		if (strings.HasPrefix(line, "  --") || strings.HasPrefix(line, strings.Repeat(" ", 21))) && len(line) > 74 {
			t.Errorf("flag line is %d columns wide: %q", len(line), line)
		}
	}
}

// Every yield tag, mixed among them, reaches the CLI usage, wrapped to
// the usage's width, and the MCP quality help.
func TestYieldQualityTagsReachBothFrontEnds(t *testing.T) {
	t.Parallel()
	usage := yieldUsage()
	var listed []string
	for _, q := range yieldQualityTags {
		listed = append(listed, q.tag)
		if !strings.Contains(usage, "\n    "+q.tag+" ") {
			t.Errorf("usage does not list the %s tag", q.tag)
		}
		if !strings.Contains(qualityHelp, "\n- "+q.tag+": "+q.meaning+".") {
			t.Errorf("describe topic=quality does not list the %s tag", q.tag)
		}
	}
	var want []string
	for _, tag := range append(slices.Clone(yield.Tags), yield.TagMixed) {
		want = append(want, string(tag))
	}
	if !slices.Equal(listed, want) {
		t.Errorf("tags %v, want yield.Tags and mixed %v", listed, want)
	}
	for _, line := range strings.Split(tagNotes(yieldQualityTags), "\n") {
		if len(line) > 72 {
			t.Errorf("quality line is %d columns wide: %q", len(line), line)
		}
	}
}
