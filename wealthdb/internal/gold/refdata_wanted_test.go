package gold

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

var updateGolden = flag.Bool("update-wanted", false, "rewrite testdata/refdata_wanted.json")

// The wanted list of a small invented household, pinned as JSON: the
// contract the refdata collector reads (docs/adapters/refdata.md §3).
//
//   - FUNDA is held at one source under a symbol and at another under a
//     CUSIP; nothing ties the two keys yet, so they are two funds, each
//     with its own window. The second source has not been observed for
//     months, so its holding ends at its last snapshot.
//   - FUNDB was sold, so its window is closed.
//   - IDX1 is FUNDB's proxy; IDX9 a reference fund.
//   - STK1 is a stock with a sector from a fund line and a lookup day.
//   - NOTE1, a note on a basket, is wanted like a fund.
//   - A private fund and a coin are not wanted.
func TestRefdataWantedPinned(t *testing.T) {
	db, ctx := openMigrated(t)
	day := func(m time.Month, d int) int64 { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at)
	    VALUES ('second-src', 'synthetic', '/tmp/s.db', -1, 0, 0)`)
	mustExec(`UPDATE silver_sources SET observed_snapshots_through = ? WHERE silver_source_id = 'test-src'`, day(10, 1))
	mustExec(`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol, name, currency, first_seen_at, last_seen_at) VALUES
	    ('test-src',   'FUNDA', 'public_equity', 'etf',   NULL, NULL,        'FUNDA', 'Fund A ETF', 'USD', 0, 0),
	    ('second-src', 'C1',    'public_equity', 'etf',   NULL, '00000AA01', NULL,    'Fund A',     'USD', 0, 0),
	    ('test-src',   'FUNDB', 'fixed_income',  'fund',  NULL, NULL,        'FUNDB', 'Fund B',     'USD', 0, 0),
	    ('test-src',   'STK1',  'public_equity', 'stock', 'XX0000000009', NULL, 'STK1', 'Stock One', 'EUR', 0, 0),
	    ('test-src',   'PE1',   'private_equity','fund',  NULL, NULL,        'PE1',   'Private Fund','USD', 0, 0),
	    ('test-src',   'BTC',   'crypto',        'physical', NULL, NULL,     'BTC',   'Bitcoin',    'USD', 0, 0),
	    ('test-src',   'NOTE1', 'public_equity', 'structured_product', 'XX0000000017', NULL, NULL, 'Basket Note', 'USD', 0, 0)`)
	pos := func(src string, at int64, key string, ac, veh string, mv int) {
		mustExec(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES (?, ?, 'A1', ?, ?, ?, ?, 'USD', 1, ?)`,
			src, at, key, key, ac, veh, mv)
	}
	pos("test-src", day(1, 31), "FUNDA", "public_equity", "etf", 100)
	pos("test-src", day(1, 31), "FUNDB", "fixed_income", "fund", 100)
	pos("test-src", day(1, 31), "PE1", "private_equity", "fund", 100)
	pos("test-src", day(6, 30), "FUNDA", "public_equity", "etf", 110)
	pos("test-src", day(6, 30), "STK1", "public_equity", "stock", 50)
	pos("test-src", day(6, 30), "BTC", "crypto", "physical", 50)
	pos("test-src", day(6, 30), "NOTE1", "public_equity", "structured_product", 40)
	pos("second-src", day(3, 31), "C1", "public_equity", "etf", 70)
	mustExec(`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count) VALUES
	    ('test-src', 'symbol:FUNDA', ?, 'holdings', 'inbox', 1), ('test-src', 'symbol:FUNDA', ?, 'index', 'inbox', 1)`,
		day(3, 31), day(3, 31))
	mustExec(`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, sector) VALUES
	    ('test-src', 'symbol:FUNDA', ?, 'holdings', 1, 'isin:XX0000000009', 'Technology')`, day(3, 31))
	mustExec(`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value) VALUES
	    ('test-src', 'isin:XX0000000009', 'legal_country', ?, 'gleif', 'DE')`, day(5, 1))
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Proxies:        []canonical.ExposureProxy{{FundRef: "symbol:FUNDB", ProxyRef: "symbol:IDX1", Leverage: 1}},
		ReferenceFunds: []string{"symbol:IDX9"},
		Settings:       canonical.ExposureSettings{MaxGapDays: 400, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}

	w, err := RefdataWanted(ctx, db, time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const golden = "testdata/refdata_wanted.json"
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("wanted list differs from %s:\n%s", golden, got)
	}
}

// A gold the exposure migration has not reached yet (a read-only open of
// an older file) yields the held funds and securities with nothing known.
func TestRefdataWantedBeforeMigration(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, tbl := range []string{"exposure_settings", "exposure_proxies", "exposure_reference_funds"} {
		if _, err := db.ExecContext(ctx, `DROP TABLE `+tbl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `
	    INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, first_seen_at, last_seen_at)
	        VALUES ('test-src', 'S', 'public_equity', 'stock', 'S', 0, 0);
	    INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	        asset_class, vehicle, currency, market_value) VALUES ('test-src', 86400, 'A', 'S', 'S', 'public_equity', 'stock', 'USD', 1)`); err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(86400*10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Securities) != 1 || len(w.Securities[0].Missing) != 3 || w.Settings.MaxGapDays != 400 {
		t.Errorf("wanted = %+v", w)
	}
}

// Once a source ties a key to an ISIN, the wanted list names the ISIN: a
// fund held under its ticker and a stock a fund line names beside its
// ISIN are asked for once each, under the ISIN.
func TestRefdataWantedReadsKeysThroughAliases(t *testing.T) {
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, first_seen_at, last_seen_at)
		 VALUES ('test-src', 'FUNDT', 'public_equity', 'etf', 'FUNDT', 0, 0), ('test-src', 'STKT', 'public_equity', 'stock', 'STKT', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES
		 ('test-src', ?, 'A', 'FUNDT', 'FUNDT', 'public_equity', 'etf', 'USD', 1),
		 ('test-src', ?, 'A', 'STKT', 'STKT', 'public_equity', 'stock', 'USD', 1)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
		 VALUES ('test-src', 'symbol:OTHER', ?, 'holdings', 'inbox', 2)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, ticker, exchange, weight) VALUES
		 ('test-src', 'symbol:OTHER', ?, 'holdings', 1, 'isin:XX0000000011', 'XX0000000011', 'FUNDT', 'NYSE Arca', 50),
		 ('test-src', 'symbol:OTHER', ?, 'holdings', 2, 'isin:XX0000000012', 'XX0000000012', 'STKT', 'NASDAQ', 50)`,
	} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = day
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(day+86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Funds) != 1 || w.Funds[0].Ref != "isin:XX0000000011" || w.Funds[0].Symbol == nil || *w.Funds[0].Symbol != "FUNDT" {
		t.Errorf("funds = %+v", w.Funds)
	}
	if len(w.Securities) != 1 || w.Securities[0].Key != "isin:XX0000000012" {
		t.Errorf("securities = %+v", w.Securities)
	}
}

// A held fund's lines that lack a sector or a country are wanted as
// constituents, with their share and asset class and the US symbol a
// fund line or a lookup gives them, never a broker's local ticker; a
// country only an ISIN's prefix implies counts as missing. A line a
// source describes, a cash line, a line naming a fund, a line with no
// identifier and a security held directly are not.
func TestRefdataWantedNamesConstituents(t *testing.T) {
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
		 VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, isin, first_seen_at, last_seen_at)
		 VALUES ('test-src', 'FUNDC', 'public_equity', 'etf', 'FUNDC', NULL, 0, 0),
		        ('test-src', 'STKH', 'public_equity', 'stock', 'STKH', 'XX0000000063', 0, 0),
		        ('test-src', 'OLDL', 'public_equity', 'stock', 'QLOCAL', 'XX0000000062', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES
		 ('test-src', ?, 'A', 'FUNDC', 'FUNDC', 'public_equity', 'etf', 'USD', 1000),
		 ('test-src', ?, 'A', 'STKH', 'STKH', 'public_equity', 'stock', 'USD', 100)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
		 VALUES ('test-src', 'symbol:FUNDC', ?, 'holdings', 'sec-nport', 9),
		        ('test-src', 'symbol:SUBF', ?, 'holdings', 'sec-nport', 1)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, ticker, name, asset_type, weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 1, 'isin:XX0000000061', 'XX0000000061', 'QBIG', 'Big Co', 'EC', 60, 'USD', 'US', NULL),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 2, 'isin:XX0000000062', 'XX0000000062', NULL, 'Small Co', 'EC', 10, 'EUR', NULL, NULL),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 3, 'isin:XX0000000064', 'XX0000000064', NULL, 'Known Co', 'EC', 15, 'USD', 'US', 'Energy'),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 4, 'isin:XX0000000063', 'XX0000000063', NULL, 'Held Co', 'EC', 10, 'USD', NULL, NULL),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 5, NULL, NULL, NULL, 'Cash', 'Cash', 1, 'USD', NULL, NULL),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 6, 'isin:DE0000000065', 'DE0000000065', NULL, 'Prefix Co', 'EC', 1, 'EUR', NULL, 'Energy'),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 7, 'isin:XX0000000066', 'XX0000000066', NULL, 'A Bond', 'DBT', 1, 'USD', NULL, 'Corporate'),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 8, 'symbol:SUBF', NULL, 'SUBF', 'Sub Fund', 'EC', 1, 'USD', NULL, NULL),
		 ('test-src', 'symbol:FUNDC', ?, 'holdings', 9, 'name:000000000067', NULL, NULL, 'Unnamed Co', 'EC', 1, 'USD', NULL, NULL),
		 ('test-src', 'symbol:SUBF', ?, 'holdings', 1, 'isin:XX0000000068', 'XX0000000068', NULL, 'Deep Co', 'EC', 100, 'USD', 'US', 'Energy')`,
	} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = day
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(ctx, db, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]WantedSecurity{}
	for _, s := range w.Securities {
		got[s.Key] = s
	}
	big, small := got["isin:XX0000000061"], got["isin:XX0000000062"]
	if big.Scope != "constituent" || big.SharePct == nil || *big.SharePct < 54 || *big.SharePct > 55 ||
		strings.Join(big.Missing, ",") != "sector" || big.Symbol == nil || *big.Symbol != "QBIG" {
		t.Errorf("big = %+v", big)
	}
	// QLOCAL is a broker's ticker for it: no symbol for a lookup by ticker.
	if small.Scope != "constituent" || strings.Join(small.Missing, ",") != "sector,country" || small.Symbol != nil ||
		small.AssetClass == nil || *small.AssetClass != "public_equity" {
		t.Errorf("small = %+v", small)
	}
	if p := got["isin:DE0000000065"]; strings.Join(p.Missing, ",") != "country" {
		t.Errorf("a country only the ISIN prefix gives = %+v", p)
	}
	if b := got["isin:XX0000000066"]; b.AssetClass == nil || *b.AssetClass != "fixed_income" ||
		strings.Join(b.Missing, ",") != "country" {
		t.Errorf("the bond = %+v", b)
	}
	for _, key := range []string{"symbol:SUBF", "name:000000000067"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s is wanted", key)
		}
	}
	if _, ok := got["isin:XX0000000064"]; ok {
		t.Error("a described line is wanted")
	}
	if held := got["isin:XX0000000063"]; held.Scope != "direct" || held.SharePct != nil {
		t.Errorf("the held stock = %+v", held)
	}
}
