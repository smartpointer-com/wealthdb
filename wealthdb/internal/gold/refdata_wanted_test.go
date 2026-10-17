package gold

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

var updateGolden = flag.Bool("update-wanted", false, "rewrite testdata/refdata_wanted.json")

// The wanted list of a small invented household, pinned as JSON: the
// contract the refdata collector reads (docs/adapters/refdata.md §5).
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
	t.Parallel()
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

// A gold the exposure migrations have not reached yet (a read-only open
// of an older file) yields the held funds and securities with nothing
// known.
func TestRefdataWantedBeforeMigration(t *testing.T) {
	t.Parallel()
	db, ctx := openAtVersion(t, 124)
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
// ISIN are asked for once each, under the ISIN. What a provider answered
// under the ticker counts for the ISIN.
func TestRefdataWantedReadsKeysThroughAliases(t *testing.T) {
	t.Parallel()
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
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
		 VALUES ('refdata', 'symbol:STKT', 'sic', ?, 'sec-edgar', '7372')`,
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
	// A lookup's other ticker for the fund: the holding's symbol wins.
	if _, err := db.ExecContext(ctx, `INSERT INTO security_aliases VALUES ('symbol:FUNDA', 'isin:XX0000000011', 'lookup')`); err != nil {
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
		t.Fatalf("securities = %+v", w.Securities)
	}
	if s := w.Securities[0]; s.Lookups["sec-edgar"] != "2026-06-30" || slices.Contains(s.Missing, "sector") {
		t.Errorf("the ISIN's lookups = %v, missing = %v; want the answer under its ticker", s.Lookups, s.Missing)
	}
}

// A security held directly states a US symbol only: its own where the
// holding is quoted in the US, else the one a lookup found (a share held
// abroad that also trades in New York); a bank abroad's local ticker is
// never one.
func TestRefdataWantedStatesUSSymbols(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, currency,
		 first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QUSA', 'public_equity', 'stock', NULL, 'QUSA', 'USD', 0, 0),
		 ('test-src', 'QVIE', 'public_equity', 'stock', 'AT0000000031', 'QVIE', 'EUR', 0, 0),
		 ('test-src', 'QDR', 'public_equity', 'stock', 'US0000000032', NULL, 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES
		 ('test-src', ?, 'A', 'QUSA', 'QUSA', 'public_equity', 'stock', 'USD', 1),
		 ('test-src', ?, 'A', 'QVIE', 'QVIE', 'public_equity', 'stock', 'EUR', 1),
		 ('test-src', ?, 'A', 'QDR', 'QDR', 'public_equity', 'stock', 'USD', 1)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, isin, ticker, exchange)
		 VALUES ('refdata', 'isin:US0000000032', ?, 'openfigi', 'US0000000032', 'QDRY', 'US'),
		        ('refdata', 'isin:AT0000000035', ?, 'openfigi', 'AT0000000035', 'QVNSY', 'US')`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QVNS', 'public_equity', 'stock', 'AT0000000035', 'QVNS', 'EUR', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A', 'QVNS', 'QVNS', 'public_equity', 'stock', 'EUR', 1)`,
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
	got := map[string]string{}
	for _, s := range w.Securities {
		got[s.Key] = "<nil>"
		if s.Symbol != nil {
			got[s.Key] = *s.Symbol
		}
	}
	want := map[string]string{"symbol:QUSA": "QUSA", "isin:AT0000000031": "<nil>", "isin:US0000000032": "QDRY",
		"isin:AT0000000035": "QVNSY"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s symbol = %s, want %s", k, got[k], v)
		}
	}
}

// A held fund's lines that lack a sector or a country are wanted as
// constituents, with their share and asset class and the US symbol a
// fund line or a lookup gives them, never a broker's local ticker; a
// country only an ISIN's prefix implies counts as missing. A line a
// source describes, a cash line, a line naming a fund, a line with no
// identifier and a security held directly are not.
func TestRefdataWantedNamesConstituents(t *testing.T) {
	t.Parallel()
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

// A receipt that took its sector and country from a share matched by name
// is still wanted, so a lookup can tie it to its share.
func TestRefdataWantedKeepsAskingAboutANameMatch(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QFND', 'public_equity', 'etf', 'QFND', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A', 'QFND', 'QFND', 'public_equity', 'etf', 'USD', 100)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
		 VALUES ('test-src', 'symbol:QFND', ?, 'holdings', 'ishares', 2)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QFND', ?, 'holdings', 1, 'isin:US0000000771', 'US0000000771', 'QORVANT AG SPON ADR',
		  'Equity', 50, 'USD', NULL, NULL),
		 ('test-src', 'symbol:QFND', ?, 'holdings', 2, 'isin:DE0000000772', 'DE0000000772', 'QORVANT N AG',
		  'Equity', 50, 'EUR', 'Germany', 'Industrials')`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{day}, strings.Count(q, "?"))...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	var origin sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT sector_origin FROM securities WHERE security_key = 'isin:US0000000771'`).
		Scan(&origin); err != nil || origin.String != "underlying:isin:DE0000000772" {
		t.Fatalf("the receipt is not linked by name: %q, %v", origin.String, err)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(day+86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range w.Securities {
		if s.Key == "isin:US0000000771" {
			if !slices.Contains(s.Missing, "sector") || !slices.Contains(s.Missing, "country") {
				t.Errorf("missing = %v, want sector and country", s.Missing)
			}
			return
		}
	}
	t.Error("the name-matched receipt is not wanted")
}

// A fund a held fund holds is wanted too, from today: each line of the
// held fund's newest composition that is a fund and names it by an
// identifier. A line named by name alone, an older composition's line
// and a swap fund's basket are not.
func TestRefdataWantedNamesTheFundsAHeldFundHolds(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'QFOF', 'multi_asset', 'fund', 'QFOF', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A', 'QFOF', 'QFOF', 'multi_asset', 'fund', 'USD', 100)`,
		// The newest holdings, an index list of the same day, an older
		// list and a newer swap basket.
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count) VALUES
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 'testprov', 7),
		 ('test-src', 'symbol:QFOF', ?, 'index', 'testprov', 1),
		 ('test-src', 'symbol:QFOF', ? - 86400 * 200, 'holdings', 'testprov', 1),
		 ('test-src', 'symbol:QFOF', ? + 86400, 'collateral', 'testprov', 1),
		 ('test-src', 'symbol:QSUB', ? - 86400, 'holdings', 'testprov', 1)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, ticker,
		 exchange, currency, name, asset_type, weight) VALUES
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 1, 'symbol:QSUB', NULL, 'QSUB', NULL, 'USD', 'Qumbra Series Equity Fund', 'Fund', 50),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 2, 'name:q1', NULL, NULL, NULL, 'USD', 'Qumbra Series Bond Fund', 'Fund', 20),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 3, 'symbol:QSTK', NULL, 'QSTK', NULL, 'USD', 'Qorvant Corp', 'Equity', 10),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 4, 'isin:XX0000000401', 'XX0000000401', 'QLOC', 'London Stock Exchange', 'GBP',
		  'Qumbra Qland Fund', 'Fund', 15),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 5, 'symbol:QMMF', NULL, 'QMMF', NULL, 'USD', 'Qumbra Money Market Fund',
		  'Money Market Funds', 5),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 6, 'symbol:QGBP', NULL, 'QGBP', NULL, 'GBP', 'Qumbra Sterling Fund', 'Fund', 0),
		 ('test-src', 'symbol:QFOF', ?, 'holdings', 7, 'symbol:QLSE', NULL, 'QLSE', 'London Stock Exchange', 'USD',
		  'Qumbra Dollar Fund', 'Fund', 0),
		 ('test-src', 'symbol:QFOF', ?, 'index', 1, 'symbol:QIDX', NULL, 'QIDX', NULL, 'USD', 'Qumbra Index Fund', 'Fund', 100),
		 ('test-src', 'symbol:QFOF', ? - 86400 * 200, 'holdings', 1, 'symbol:QOLD', NULL, 'QOLD', NULL, 'USD', 'Qumbra Old Fund', 'Fund', 100),
		 ('test-src', 'symbol:QFOF', ? + 86400, 'collateral', 1, 'symbol:QCOL', NULL, 'QCOL', NULL, 'USD', 'Qumbra Basket Fund', 'Fund', 100),
		 ('test-src', 'symbol:QSUB', ? - 86400, 'holdings', 1, 'symbol:QSTK', NULL, 'QSTK', NULL, 'USD', 'Qorvant Corp', 'Equity', 100)`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{day}, strings.Count(q, "?"))...); err != nil {
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
	got := map[string][]string{}
	for _, f := range w.Funds {
		got[f.Ref] = f.Roles
		switch f.Ref {
		case "symbol:QSUB":
			// Its own key names a US-dollar line: its symbol. Wanted from
			// today, with the composition gold already has.
			if f.Symbol == nil || *f.Symbol != "QSUB" || f.HeldTo != nil || f.HeldFrom == nil ||
				!slices.Equal(f.Compositions, []string{"2026-06-29"}) {
				t.Errorf("QSUB = %+v, want its symbol and composition, wanted from today", f)
			}
		case "isin:XX0000000401":
			// A ticker on a London line is no US symbol.
			if f.ISIN == nil || f.Symbol != nil {
				t.Errorf("the London fund = %+v, want its ISIN and no symbol", f)
			}
		}
	}
	// A company, a money market fund (cash), a name-only line, a fund
	// keyed by a ticker with no US listing, an index list, an older list
	// and a swap basket stay out.
	want := map[string][]string{"symbol:QFOF": {"held"}, "symbol:QSUB": {"underlying"},
		"isin:XX0000000401": {"underlying"}}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("funds = %v, want %v", got, want)
	}
}

// A bond held directly reads its sector from a lookup's type of it, and
// a municipal bond its country too, so neither field is missing and the
// bond's leaf counts as a described security. A fund line's sector wins
// over the type, and a stock's type states nothing. A lookup under
// another key that found nothing does not count for the bond: its own
// identifier may find it. One that found it does.
func TestRefdataWantedReadsABondByItsType(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, currency,
		 first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QMUNI', 'fixed_income', 'bond', 'US00000000A1', '00000000A', 'USD', 0, 0),
		 ('test-src', 'QGOVT', 'fixed_income', 'bond', 'XX0000000041', NULL, 'CHF', 0, 0),
		 ('test-src', 'QNEW', 'fixed_income', 'bond', 'US00000000B9', '00000000B', 'USD', 0, 0),
		 ('test-src', 'QLINE', 'fixed_income', 'bond', 'XX0000000042', NULL, 'EUR', 0, 0),
		 ('test-src', 'QSTK', 'public_equity', 'stock', 'XX0000000043', NULL, 'EUR', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, market_value) VALUES
		 ('test-src', ?, 'A', 'QMUNI', 'QMUNI', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QGOVT', 'QGOVT', 'fixed_income', 'bond', 'CHF', 1),
		 ('test-src', ?, 'A', 'QNEW', 'QNEW', 'fixed_income', 'bond', 'USD', 1),
		 ('test-src', ?, 'A', 'QLINE', 'QLINE', 'fixed_income', 'bond', 'EUR', 1),
		 ('test-src', ?, 'A', 'QSTK', 'QSTK', 'public_equity', 'stock', 'EUR', 1)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload) VALUES
		 ('refdata', 'isin:US00000000A1', ?, 'openfigi', 'Muni', '{"found": true}'),
		 ('refdata', 'symbol:00000000A', ?, 'gleif', NULL, '{"found": true}'),
		 ('refdata', 'isin:XX0000000041', ?, 'openfigi', 'Govt', '{"found": true}'),
		 ('refdata', 'symbol:00000000B', ?, 'openfigi', NULL, '{"found": false}'),
		 ('refdata', 'isin:US00000000B9', ?, 'gleif', NULL, '{"found": false}'),
		 ('refdata', 'isin:XX0000000042', ?, 'openfigi', 'Corp', '{"found": true}'),
		 ('refdata', 'isin:XX0000000043', ?, 'openfigi', 'Common Stock', '{"found": true}')`,
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value) VALUES
		 ('refdata', 'isin:XX0000000041', 'legal_country', ?, 'gleif', 'DE'),
		 ('refdata', 'isin:XX0000000042', 'legal_country', ?, 'gleif', 'DE'),
		 ('refdata', 'isin:XX0000000043', 'legal_country', ?, 'gleif', 'DE')`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count)
		 VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 'testprov', 1)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, sector, weight)
		 VALUES ('test-src', 'symbol:QREF', ?, 'holdings', 1, 'isin:XX0000000042', 'XX0000000042', 'Government', 100)`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{day}, strings.Count(q, "?"))...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"isin:US00000000A1": "municipal type:openfigi US type:openfigi",
		"isin:XX0000000041": "government type:openfigi DE fact:gleif",
		"isin:XX0000000042": "government fund_line:testprov DE fact:gleif",
		"isin:XX0000000043": "- - DE fact:gleif",
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT concat_ws(' ', COALESCE(sector, '-'), COALESCE(sector_origin, '-'),
		  COALESCE(risk_country, domicile), COALESCE(risk_country_origin, domicile_origin))
		  FROM securities WHERE security_key = ?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var how, flags string
	if err := db.QueryRowContext(ctx, `SELECT how, array_to_string(flags, ',') FROM exposure_leaves(?, 'USD')
	  WHERE top_key = 'isin:US00000000A1'`, day+86400).Scan(&how, &flags); err != nil {
		t.Fatal(err)
	}
	if how != "security" || flags != "" {
		t.Errorf("the municipal bond's leaf = %s %v, want a described security with no flag", how, flags)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(day+86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*WantedSecurity{}
	for i := range w.Securities {
		got[w.Securities[i].Key] = &w.Securities[i]
	}
	for key, missing := range map[string][]string{"isin:US00000000A1": {}, "isin:XX0000000041": {},
		"isin:US00000000B9": {"sector", "country"}, "isin:XX0000000043": {"sector", "market_cap"}} {
		if s := got[key]; s == nil || !slices.Equal(s.Missing, missing) {
			t.Errorf("%s = %+v, want missing %v", key, s, missing)
		}
	}
	// The ticker lookup found nothing under the CUSIP; GLEIF's answer
	// under the bond's own key counts, and so does one that found the
	// municipal bond under its CUSIP.
	if s := got["isin:US00000000B9"]; s != nil && (s.Lookups["openfigi"] != "" || s.Lookups["gleif"] != "2026-06-30") {
		t.Errorf("the new bond's lookups = %v, want gleif only", s.Lookups)
	}
	if s := got["isin:US00000000A1"]; s != nil && s.Lookups["gleif"] != "2026-06-30" {
		t.Errorf("the municipal bond's lookups = %v, want gleif's answer under its CUSIP", s.Lookups)
	}
}

// A proxy of a fund that is not held is wanted from today, as a
// reference fund is; a proxy the config names under an alias is wanted
// under the key the alias names.
func TestRefdataWantedProxiesOfFundsNotHeld(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Proxies: []canonical.ExposureProxy{
			{FundRef: "symbol:QNH1", ProxyRef: "symbol:QPX1", Leverage: 1},
			{FundRef: "symbol:QNH2", ProxyRef: "symbol:QPX2", Leverage: 1},
		},
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: canonical.DefaultAgedDays,
			PartialPct: canonical.DefaultPartialPct},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO security_aliases VALUES ('symbol:QPX2', 'isin:XX0000000021', 'lookup')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	w, err := RefdataWanted(ctx, db, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range w.Funds {
		from := ""
		if f.HeldFrom != nil {
			from = *f.HeldFrom
		}
		got = append(got, f.Ref+" "+strings.Join(f.Roles, ",")+" "+from+" "+strconv.FormatBool(f.HeldTo == nil))
	}
	want := []string{"isin:XX0000000021 proxy 2026-06-30 true", "symbol:QPX1 proxy 2026-06-30 true"}
	if !slices.Equal(got, want) {
		t.Errorf("funds = %q, want %q", got, want)
	}
}

// A fund the config names states the CUSIP and US symbol its aliases
// give. A size fund named by its ISIN gains a lookup's ticker and a fund
// line's CUSIP. A proxy named by its ticker reads as its ISIN and keeps
// the ticker. A ticker only a broker states is no US symbol.
func TestRefdataWantedFundsStateTheirAliases(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Proxies:        []canonical.ExposureProxy{{FundRef: "symbol:QNH1", ProxyRef: "symbol:QPX1", Leverage: 1}},
		ReferenceFunds: []string{"isin:XX0000000032"},
		SizeFunds:      []canonical.ExposureSizeFund{{FundRef: "isin:XX0000000031", CoveragePct: 90}},
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: canonical.DefaultAgedDays,
			PartialPct: canonical.DefaultPartialPct},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO security_aliases VALUES
	    ('symbol:QSZ1', 'isin:XX0000000031', 'lookup'), ('cusip:Q0000SZ31', 'isin:XX0000000031', 'fund_line'),
	    ('symbol:QPX1', 'isin:XX0000000021', 'fund_line'), ('symbol:QRF2', 'isin:XX0000000032', 'instrument')`); err != nil {
		t.Fatal(err)
	}
	w, err := RefdataWanted(ctx, db, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range w.Funds {
		got[f.Ref] = strings.Join([]string{show(f.ISIN), show(f.CUSIP), show(f.Symbol)}, " ")
	}
	want := map[string]string{
		"isin:XX0000000021": "XX0000000021 <nil> QPX1",
		"isin:XX0000000031": "XX0000000031 Q0000SZ31 QSZ1",
		"isin:XX0000000032": "XX0000000032 <nil> <nil>",
	}
	if !maps.Equal(got, want) {
		t.Errorf("funds = %q, want %q", got, want)
	}
}
