package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupExposureGold seeds a gold with one fund looked through to two
// invented stocks and some cash, plus any extra rows, and returns a
// config path for it. Every ? in a row binds the snapshot instant.
func setupExposureGold(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")
	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range append([]string{
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
		 VALUES ('brk', 'synthetic', '/tmp/brk.db', -1, 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
		 VALUES ('brk', 'ACCT-7781', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency, first_seen_at, last_seen_at)
		 VALUES ('brk', 'IDXF', 'public_equity', 'etf', 'IDXF', 'Index Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'IDXF', 'IDXF', 'public_equity', 'etf', 'USD', 10, 1000)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		 VALUES ('brk', ?, 'ACCT-7781', 'USD', 'current', 250)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
		 VALUES ('brk', 'symbol:IDXF', ?, 'holdings', 'inbox', 2)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, ticker, name, asset_type, weight, currency, country, sector)
		 VALUES ('brk', 'symbol:IDXF', ?, 'holdings', 1, 'symbol:AAAA', 'AAAA', 'Alpha Corp', 'Equity', 70, 'USD', 'United States', 'Technology'),
		        ('brk', 'symbol:IDXF', ?, 'holdings', 2, 'symbol:BBBB', 'BBBB', 'Beta Corp', 'Equity', 30, 'EUR', 'France', 'Energy')`,
	}, extra...) {
		args := []any{}
		for i := 0; i < strings.Count(q, "?"); i++ {
			args = append(args, snap)
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := gold.RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	cfg := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(cfg, []byte(`{"gold_db": "`+goldPath+`", "default_currency": "USD", "silver_sources": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runExposure(t *testing.T, cfg string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"-c", cfg, "exposure"}, args...), nil, &out, &errb)
	return out.String(), errb.String(), code
}

func TestExposureClassesAndSectors(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t)
	out, errb, code := runExposure(t, cfg, "sectors", "-d", "2026-06-30", "-f", "csv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{
		"asset_class,sector,value_USD,share_pct,class_share_pct,quality",
		"public_equity,technology,700.00,56.00,70.00,",
		"public_equity,energy,300.00,24.00,30.00,",
		"cash,cash,250.00,20.00,100.00,",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sectors lacks %q:\n%s", want, out)
		}
	}
	// -C names a share as the other reports do; it prints as share_pct.
	out, errb, code = runExposure(t, cfg, "sectors", "-d", "2026-06-30", "-f", "csv", "-C", "sector,share,class_share")
	if code != 0 || !strings.HasPrefix(out, "sector,share_pct,class_share_pct\n") {
		t.Errorf("-C sector,share,class_share: exit %d %s\n%s", code, errb, out)
	}
}

// Each view's default column set resolves against its registry, and the
// usage lists every view.
func TestExposureViewsResolveTheirDefaults(t *testing.T) {
	t.Parallel()
	usage := exposureUsage()
	for _, view := range exposureViews {
		if _, err := exposureReport(request{view: view, currency: "USD"}).pick("default"); err != nil {
			t.Errorf("%s defaults: %v", view, err)
		}
		if !strings.Contains(usage, "\n  "+view+" ") {
			t.Errorf("usage does not list the %s view", view)
		}
	}
}

// Every quality tag and situs basis reaches the CLI usage, wrapped to
// the usage's width, and the MCP quality help.
func TestExposureQualityTagsReachBothFrontEnds(t *testing.T) {
	t.Parallel()
	usage := exposureUsage()
	for _, tags := range [][]struct{ tag, meaning string }{exposureQualityTags, exposureSitusBases} {
		for _, q := range tags {
			if !strings.Contains(usage, "\n    "+q.tag+" ") {
				t.Errorf("usage does not list the %s tag", q.tag)
			}
			if !strings.Contains(qualityHelp, "\n- "+q.tag+": "+q.meaning+".") {
				t.Errorf("describe topic=quality does not list the %s tag", q.tag)
			}
		}
		for _, line := range strings.Split(exposureNotes(tags), "\n") {
			if len(line) > 72 {
				t.Errorf("quality line is %d columns wide: %q", len(line), line)
			}
		}
	}
	if got := wrapWords("aa bb cc averyveryverylongword", 5); strings.Join(got, "|") != "aa bb|cc|averyveryverylongword" {
		t.Errorf("wrapWords = %q", got)
	}
}

// -p hides every amount and every account or instrument id on every
// view, and keeps the shares, the labels and the public keys; flags may
// follow the view. The opaque holding has no security key, so the
// securities, funds and coverage views name it by its instrument id.
func TestExposurePrivacyHidesAmounts(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'PRIV-5521', 'PRIV-5521', 'private_equity', 'spv', 'USD', 1, 500)`)
	kept := map[string][]string{
		"classes":    {"public_equity", "57.14"},
		"sectors":    {"technology", "40.00"},
		"sizes":      {"public_equity", "unknown"},
		"countries":  {"US", "40.00"},
		"currencies": {"USD", "fiat"},
		"situs":      {"unknown", "100.00"},
		"securities": {"symbol:AAAA", "Alpha Corp", "40.00", "technology"},
		"funds":      {"symbol:IDXF", "57.14"},
		"coverage":   {"looked_through", "undeclared", "brk"},
	}
	for _, v := range exposureViews {
		out, errb, code := runExposure(t, cfg, v, "-d", "2026-06-30", "-p", "-f", "csv", "-C", "all")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", v, code, errb)
		}
		for _, leak := range []string{"700.00", "300.00", "250.00", "500.00", "1000.00", "1750.00", "ACCT-7781", "PRIV-5521"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s -p shows %q:\n%s", v, leak, out)
			}
		}
		for _, k := range kept[v] {
			if !strings.Contains(out, k) {
				t.Errorf("%s -p lost %q:\n%s", v, k, out)
			}
		}
	}
}

// coverage -f ndjson prints a skeleton record per holding undeclared,
// with its economic dimensions empty, and a situs-only one per holding
// of unknown situs, a holding with a record among them; each note
// is the holding's name, and nothing else is printed. Cash names no
// instrument and takes none. The other views, a series, a column set and
// -p refuse it.
func TestExposureCoverageSkeletons(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'PRIV-5521', 'private_equity', 'spv', 'Example SPV <A&B>', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'PRIV-5521', 'PRIV-5521',
		 'private_equity', 'spv', 'USD', 1, 500)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'PRIV-0', 'PRIV-0',
		 'private_equity', 'spv', 'USD', 1, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'PRIV-7', 'private_equity', 'fund', 'Example Fund Seven', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'PRIV-7', 'PRIV-7',
		 'private_equity', 'fund', 'USD', 1, 70)`,
		`INSERT INTO exposure_declarations (line_no, silver_source_id, instrument_external_id, dimensions)
		 VALUES (3, 'brk', 'PRIV-7', 'region')`,
		`INSERT INTO exposure_declaration_leaves (line_no, leaf_no, share, region) VALUES (3, 1, 1, 'europe')`)
	out, errb, code := runExposure(t, cfg, "coverage", "-d", "2026-06-30", "-f", "ndjson")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if errb != "" {
		t.Errorf("stderr = %q", errb)
	}
	want := `{"silver_source_id":"brk","instrument":"PRIV-5521","country":{},"currency":{},"sector":{},` +
		`"note":"Example SPV <A&B>"}` + "\n" +
		`{"silver_source_id":"brk","instrument":"IDXF","situs":{},"note":"Index Fund"}` + "\n" +
		`{"silver_source_id":"brk","instrument":"PRIV-5521","situs":{},"note":"Example SPV <A&B>"}` + "\n" +
		`{"silver_source_id":"brk","instrument":"PRIV-7","situs":{},"note":"Example Fund Seven"}` + "\n"
	if out != want {
		t.Errorf("skeletons:\n%s\nwant\n%s", out, want)
	}
	for _, args := range [][]string{
		{"classes", "-f", "ndjson"},
		{"coverage", "-f", "ndjson", "-p"},
		{"coverage", "-f", "ndjson", "-C", "reason"},
		{"coverage", "-f", "ndjson", "--period", "monthly"},
	} {
		if _, errb, code := runExposure(t, cfg, args...); code != 2 || !strings.Contains(errb, "ndjson") {
			t.Errorf("%v: exit %d, %s", args, code, errb)
		}
	}
	if _, errb, code := runExposure(t, cfg, "funds", "-f", "jsonl"); code != 2 {
		t.Errorf("an unknown format: exit %d, %s", code, errb)
	}
}

// funds shows what a declaration states of a fund held directly: via
// declared, the record's date and its dimensions.
func TestExposureFundsShowADeclaration(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		 first_seen_at, last_seen_at) VALUES ('brk', 'PRIVF', 'private_equity', 'fund', 'Example Private Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES ('brk', ?, 'ACCT-7781', 'PRIVF', 'PRIVF',
		 'private_equity', 'fund', 'USD', 1, 400)`,
		`INSERT INTO exposure_declarations (line_no, silver_source_id, instrument_external_id, as_of, dimensions)
		 VALUES (1, 'brk', 'PRIVF', 1719705600, 'region,asset_class')`,
		`INSERT INTO exposure_declaration_leaves (line_no, leaf_no, share, region, asset_class)
		 VALUES (1, 1, 0.45, 'europe', 'private_equity'), (1, 2, 0.55, 'other', 'private_debt')`)
	out, errb, code := runExposure(t, cfg, "funds", "-d", "2026-06-30", "-f", "csv", "-C",
		"fund,via,declaration_date,declared,quality")
	if code != 0 || !strings.Contains(out, `,declared,2024-06-30,"region,asset_class",declared`+"\n") {
		t.Errorf("exit %d %s\n%s", code, errb, out)
	}
}

// situs is one row per legal situs, a fund whole at its own: its
// domicile here, the cash at its custodian's country. funds shows the
// situs beside the domicile; a series takes it; --country does not.
func TestExposureSitusView(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t,
		`INSERT INTO fund_attributes (silver_source_id, fund_ref, as_of, domicile) VALUES ('brk', 'symbol:IDXF', ?, 'IE')`,
		`INSERT INTO exposure_custody (silver_source_id, custodian, country) VALUES ('brk', 'Example Bank', 'US')`)
	for args, want := range map[string]string{
		"situs -d 2026-06-30 -f csv": "situs,country_name,region,value_USD,share_pct,funds_pct,quality\n" +
			"IE,Ireland,europe,1000.00,80.00,100.00,domicile=100%\n" +
			"US,United States,north_america,250.00,20.00,0.00,custody=100%\n",
		"situs -d 2026-06-30 -f csv -p": "situs,country_name,region,value_USD,share_pct,funds_pct,quality\n" +
			"IE,Ireland,europe,,80.00,100.00,domicile=100%\nUS,United States,north_america,,20.00,0.00,custody=100%\n",
		"funds -d 2026-06-30 -f csv -C fund,domicile,situs,situs_basis": "fund,domicile,situs,situs_basis\n" +
			"symbol:IDXF,IE,IE,domicile\n(other holdings),,,\n",
	} {
		out, errb, code := runExposure(t, cfg, strings.Fields(args)...)
		if code != 0 || out != want {
			t.Errorf("%s: exit %d %s\n%s\nwant\n%s", args, code, errb, out, want)
		}
	}
	out, errb, code := runExposure(t, cfg, "situs", "--period", "monthly", "2026-06", "2026-06", "-f", "csv")
	if code != 0 || !strings.Contains(out, "2026-06,IE,Ireland,europe,1000.00,80.00,100.00,domicile=100%\n") {
		t.Errorf("situs series: exit %d %s\n%s", code, errb, out)
	}
	if _, errb, code := runExposure(t, cfg, "situs", "--country", "domicile"); code != 2 || !strings.Contains(errb, "--country") {
		t.Errorf("situs --country: exit %d, %s", code, errb)
	}
}

func TestExposureCountryFlagBelongsToTwoViews(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t)
	if _, errb, code := runExposure(t, cfg, "classes", "--country", "domicile"); code != 2 || !strings.Contains(errb, "--country") {
		t.Errorf("classes --country: exit %d, %s", code, errb)
	}
	if _, errb, code := runExposure(t, cfg, "countries", "--country", "home"); code != 2 {
		t.Errorf("countries --country home: exit %d, %s", code, errb)
	}
	out, errb, code := runExposure(t, cfg, "countries", "-d", "2026-06-30", "--country", "domicile", "-f", "csv")
	if code != 0 || !strings.Contains(out, "FR,France,europe,300.00") {
		t.Errorf("countries --country domicile: exit %d %s\n%s", code, errb, out)
	}
}

// --period repeats a view at the end of every period of a window; the
// last period ends at the window's end.
func TestExposureSeries(t *testing.T) {
	t.Parallel()
	cfg := setupExposureGold(t)
	out, errb, code := runExposure(t, cfg, "classes", "--period", "monthly", "2026-05", "2026-06", "-f", "csv")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{
		"period,asset_class,value_USD,share_pct,direct_pct,via_funds_pct,quality",
		"2026-06,public_equity,1000.00,80.00,0.00,80.00,",
		"2026-06,cash,250.00,20.00,20.00,0.00,",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("series lacks %q:\n%s", want, out)
		}
	}
	// Nothing was held at the end of May.
	if strings.Contains(out, "2026-05,") {
		t.Errorf("May shows rows:\n%s", out)
	}
	// A window from the start of time begins at the first holding; one
	// past today ends today.
	out, errb, code = runExposure(t, cfg, "classes", "--period", "annual", "-", "2099-12-31", "-f", "csv")
	if code != 0 {
		t.Fatalf("open window: exit %d: %s", code, errb)
	}
	thisYear := time.Now().UTC().Format("2006")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		if y := strings.SplitN(line, ",", 2)[0]; y < "2026" || y > thisYear {
			t.Errorf("open window has the period %s:\n%s", y, out)
		}
	}
	// -p hides the values and keeps the period.
	out, _, _ = runExposure(t, cfg, "classes", "--period", "monthly", "2026-06", "2026-06", "-p", "-f", "csv")
	if !strings.Contains(out, "2026-06,public_equity,,80.00,") || strings.Contains(out, "1000.00") {
		t.Errorf("privacy series:\n%s", out)
	}
	for _, args := range [][]string{
		{"securities", "--period", "monthly"},
		{"classes", "--period", "daily"},
		{"classes", "--period", "monthly", "-d", "2026-06-30"},
		{"classes", "2026-06"},
		{"classes", "--period", "monthly", "2099"},
	} {
		if _, _, code := runExposure(t, cfg, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// Each point takes its period's label and end; the last ends at the
// window's end.
func TestSeriesPointsEndEveryPeriod(t *testing.T) {
	t.Parallel()
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	to := day(2026, 8, 15) + 86399
	got := seriesPoints(day(2026, 2, 10), to, "quarterly")
	want := []seriesPoint{{"2026-Q1", day(2026, 4, 1) - 1}, {"2026-Q2", day(2026, 7, 1) - 1}, {"2026-Q3", to}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("point %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got := seriesPoints(day(2025, 3, 1), to, "annual"); len(got) != 2 || got[0].label != "2025" || got[1].at != to {
		t.Errorf("annual = %v", got)
	}
	if got := seriesPoints(day(2026, 8, 20), to, "monthly"); got != nil {
		t.Errorf("a window that starts after its end = %v", got)
	}
}
