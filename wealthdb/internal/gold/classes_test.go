package gold

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// classPairs reads every instrument's and position row's pair, keyed
// "table source/instrument[@snapshot]".
func classPairs(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := scanAll(ctx, db, `
SELECT 'instrument ' || silver_source_id || '/' || instrument_external_id, asset_class || ' ' || vehicle FROM instruments
UNION ALL
SELECT 'position ' || silver_source_id || '/' || instrument_external_id || '@' || snapshot_at,
       asset_class || ' ' || vehicle FROM positions`, func(rows *sql.Rows) error {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		out[k] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// The class pass reads OpenFIGI's latest type of an instrument's
// security over the pair the load gave it, on the instrument and on its
// positions, whatever its source's kind: a crypto trust an adapter types
// as an equity reads as a fund, in a schwab source and in a ubs one, and
// a bond an adapter types as an equity as a bond. The type is found
// under the US ISIN of the instrument's CUSIP, its CUSIP or its symbol.
// An ETF whose abbreviated name already reads as fixed income keeps its
// pair. A pinned instrument and an instrument OpenFIGI does not type
// keep theirs. A second pass changes nothing. Every identifier and name
// is invented.
func TestTheClassPassReadsTheLookupsType(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	trustISIN := canonical.KeyISIN + canonical.ISINFromCUSIP("00000000T")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at,
		  last_loaded_at) VALUES ('other-src', 'ubs', '/tmp/other.db', -1, 0, 0)`, nil},
		{`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, symbol, name,
		  currency, first_seen_at, last_seen_at) VALUES
		  ('test-src', '00000000T', 'public_equity', 'stock', '00000000T', 'EXTR', 'EXAMPLE BITCOIN TRUST', 'USD', 0, 0),
		  ('test-src', '00000000B', 'fixed_income', 'etf', '00000000B', 'EXBD', 'EXAMPLE TOTAL BD MKT ETF', 'USD', 0, 0),
		  ('test-src', '00000000P', 'public_equity', 'stock', '00000000P', 'EXPN', 'EXAMPLE ETHER TRUST', 'USD', 0, 0),
		  ('test-src', 'EXNOTE', 'public_equity', 'stock', NULL, 'EXNOTE', 'EXAMPLE NOTE', 'USD', 0, 0),
		  ('test-src', '00000000N', 'public_equity', 'stock', '00000000N', 'EXNT', 'EXAMPLE BITCOIN TRUST II', 'USD', 0, 0),
		  ('other-src', '00000000T', 'public_equity', 'stock', '00000000T', 'EXTR', 'EXAMPLE BITCOIN TRUST', 'USD', 0, 0)`, nil},
		{`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		  asset_class, vehicle, currency, market_value) VALUES
		  ('test-src', 1000, 'A', '00000000T', '00000000T', 'public_equity', 'stock', 'USD', 1),
		  ('test-src', 2000, 'A', '00000000T', '00000000T', 'public_equity', 'stock', 'USD', 1),
		  ('test-src', 1000, 'A', '00000000B', '00000000B', 'fixed_income', 'etf', 'USD', 1),
		  ('test-src', 1000, 'A', '00000000P', '00000000P', 'public_equity', 'stock', 'USD', 1),
		  ('test-src', 1000, 'A', 'EXNOTE', 'EXNOTE', 'public_equity', 'stock', 'USD', 1),
		  ('other-src', 1000, 'B', '00000000T', '00000000T', 'public_equity', 'stock', 'USD', 1)`, nil},
		{`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload) VALUES
		  ('refdata', ?, 100, 'openfigi', 'Common Stock', '{"found": true}'),
		  ('refdata', ?, 200, 'openfigi', 'Mutual Fund', '{"found": true}'),
		  ('refdata', 'cusip:00000000B', 200, 'openfigi', 'Mutual Fund', '{"found": true}'),
		  ('refdata', 'cusip:00000000P', 200, 'openfigi', 'Mutual Fund', '{"found": true}'),
		  ('refdata', 'symbol:EXNOTE', 200, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'cusip:00000000N', 200, 'gleif', 'Mutual Fund', '{"found": true}')`,
			[]any{trustISIN, trustISIN}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	pinned := map[string]map[string]ClassPin{"test-src": {"00000000P": {AssetClass: "public_equity", Vehicle: "stock"}}}
	n, err := RefineClasses(ctx, db, silver.RefineClass, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if n != (ClassCounts{Instruments: 3, Positions: 4}) {
		t.Errorf("counts %+v, want 3 instruments and 4 positions", n)
	}
	want := map[string]string{
		"instrument test-src/00000000T":     "crypto fund",
		"position test-src/00000000T@1000":  "crypto fund",
		"position test-src/00000000T@2000":  "crypto fund",
		"instrument test-src/00000000B":     "fixed_income etf",
		"position test-src/00000000B@1000":  "fixed_income etf",
		"instrument test-src/00000000P":     "public_equity stock",
		"position test-src/00000000P@1000":  "public_equity stock",
		"instrument test-src/EXNOTE":        "fixed_income bond",
		"position test-src/EXNOTE@1000":     "fixed_income bond",
		"instrument test-src/00000000N":     "public_equity stock",
		"instrument other-src/00000000T":    "crypto fund",
		"position other-src/00000000T@1000": "crypto fund",
	}
	got := classPairs(t, ctx, db)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("rows %v, want %d", got, len(want))
	}
	again, err := RefineClasses(ctx, db, silver.RefineClass, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if again != (ClassCounts{}) {
		t.Errorf("a second pass rewrote %+v", again)
	}
}

// OpenFIGI's type under an instrument's ISIN, under the US ISIN its CUSIP
// stands for, and under its CUSIP each outranks its type under the
// instrument's ticker, in a source of a kind other than schwab. The
// ticker's type reads where no other key has one. A pin outranks every
// type. Every identifier and name is invented.
func TestTheClassPassReadsTheTickerLast(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	usISIN := canonical.KeyISIN + canonical.ISINFromCUSIP("00000000U")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at,
		  last_loaded_at) VALUES ('other-src', 'ubs', '/tmp/other.db', -1, 0, 0)`, nil},
		{`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol,
		  name, currency, first_seen_at, last_seen_at) VALUES
		  ('other-src', 'EXISIN', 'public_equity', 'stock', 'XX00000000I1', NULL, 'EXIS', 'EXAMPLE 2% 2031', 'USD', 0, 0),
		  ('other-src', 'EXUS', 'public_equity', 'stock', NULL, '00000000U', 'EXUS', 'EXAMPLE 3% 2032', 'USD', 0, 0),
		  ('other-src', 'EXCUSIP', 'public_equity', 'stock', NULL, 'X0000000C', 'EXCU', 'EXAMPLE 4% 2033', 'USD', 0, 0),
		  ('other-src', 'EXTICK', 'public_equity', 'stock', NULL, NULL, 'EXTK', 'EXAMPLE 5% 2034', 'USD', 0, 0),
		  ('other-src', 'EXPIN', 'public_equity', 'stock', 'XX00000000P1', NULL, 'EXPN', 'EXAMPLE NOTE', 'USD', 0, 0)`, nil},
		{`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload) VALUES
		  ('refdata', 'isin:XX00000000I1', 100, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'symbol:EXIS', 100, 'openfigi', 'Common Stock', '{"found": true}'),
		  ('refdata', ?, 100, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'symbol:EXUS', 100, 'openfigi', 'Common Stock', '{"found": true}'),
		  ('refdata', 'cusip:X0000000C', 100, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'symbol:EXCU', 100, 'openfigi', 'Common Stock', '{"found": true}'),
		  ('refdata', 'symbol:EXTK', 100, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'isin:XX00000000P1', 100, 'openfigi', 'Corp', '{"found": true}'),
		  ('refdata', 'symbol:EXPN', 100, 'openfigi', 'Corp', '{"found": true}')`, []any{usISIN}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	pins := map[string]map[string]ClassPin{"other-src": {"EXPIN": {AssetClass: "other", Vehicle: "other"}}}
	if _, err := RefineClasses(ctx, db, silver.RefineClass, pins); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"instrument other-src/EXISIN":  "fixed_income bond",
		"instrument other-src/EXUS":    "fixed_income bond",
		"instrument other-src/EXCUSIP": "fixed_income bond",
		"instrument other-src/EXTICK":  "fixed_income bond",
		"instrument other-src/EXPIN":   "other other",
	}
	got := classPairs(t, ctx, db)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}

// A pin sets its pair on the instrument and on every position row of it,
// over the pair the adapter gave and over OpenFIGI's type, in a source
// of either kind. A row an earlier pass pinned is left as it is, and a
// second pass without a refiner changes nothing. Every identifier and
// name is invented.
func TestTheClassPassSetsThePins(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, q := range []string{
		`INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path, high_watermark, first_loaded_at,
		  last_loaded_at) VALUES ('other-src', 'ubs', '/tmp/other.db', -1, 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, name,
		  currency, first_seen_at, last_seen_at) VALUES
		  ('test-src', '00000000T', 'public_equity', 'stock', '00000000T', 'EXAMPLE METAL TRUST', 'USD', 0, 0),
		  ('other-src', 'EXPIN', 'public_equity', 'etf', NULL, 'EXAMPLE METAL FUND', 'USD', 0, 0),
		  ('other-src', 'EXFREE', 'public_equity', 'etf', NULL, 'EXAMPLE EQUITY FUND', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		  asset_class, vehicle, currency, market_value) VALUES
		  ('test-src', 1000, 'A', '00000000T', '00000000T', 'public_equity', 'stock', 'USD', 1),
		  ('other-src', 1000, 'B', 'EXPIN', 'EXPIN', 'metal', 'etf', 'USD', 1),
		  ('other-src', 2000, 'B', 'EXPIN', 'EXPIN', 'public_equity', 'etf', 'USD', 1),
		  ('other-src', 2000, 'B', 'EXFREE', 'EXFREE', 'public_equity', 'etf', 'USD', 1)`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type, payload)
		  VALUES ('refdata', 'cusip:00000000T', 200, 'openfigi', 'Mutual Fund', '{"found": true}')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	pins := map[string]map[string]ClassPin{
		"test-src":  {"00000000T": {AssetClass: "metal", Vehicle: "fund"}},
		"other-src": {"EXPIN": {AssetClass: "metal", Vehicle: "etf"}},
	}
	n, err := RefineClasses(ctx, db, silver.RefineClass, pins)
	if err != nil {
		t.Fatal(err)
	}
	if n != (ClassCounts{Instruments: 2, Positions: 2}) {
		t.Errorf("counts %+v, want 2 instruments and 2 positions", n)
	}
	want := map[string]string{
		"instrument test-src/00000000T":    "metal fund",
		"position test-src/00000000T@1000": "metal fund",
		"instrument other-src/EXPIN":       "metal etf",
		"position other-src/EXPIN@1000":    "metal etf",
		"position other-src/EXPIN@2000":    "metal etf",
		"instrument other-src/EXFREE":      "public_equity etf",
		"position other-src/EXFREE@2000":   "public_equity etf",
	}
	got := classPairs(t, ctx, db)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("rows %v, want %d", got, len(want))
	}
	again, err := RefineClasses(ctx, db, nil, pins)
	if err != nil {
		t.Fatal(err)
	}
	if again != (ClassCounts{}) {
		t.Errorf("a second pass rewrote %+v", again)
	}
}

// Without a refiner, or without a type, the pass writes nothing.
func TestTheClassPassWithoutTypes(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class,
	  vehicle, cusip, name, currency, first_seen_at, last_seen_at)
	  VALUES ('test-src', '00000000T', 'public_equity', 'stock', '00000000T', 'EXAMPLE BITCOIN TRUST', 'USD', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	for i, refine := range []ClassRefiner{nil, silver.RefineClass} {
		n, err := RefineClasses(ctx, db, refine, nil)
		if err != nil || n != (ClassCounts{}) {
			t.Errorf("refiner %d: %+v, %v", i, n, err)
		}
	}
	if got := classPairs(t, ctx, db)["instrument test-src/00000000T"]; !strings.HasPrefix(got, "public_equity") {
		t.Errorf("pair %q", got)
	}
}
