package gold

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// adapterRefiners is every registered adapter's refiner, as the load
// command hands them to the class pass.
func adapterRefiners(t *testing.T) map[string]ClassRefiner {
	t.Helper()
	out := silver.ClassRefiners()
	if out["schwab"] == nil {
		t.Fatal("schwab registers no class refiner")
	}
	return out
}

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

// The class pass reads OpenFIGI's latest type of a schwab instrument's
// security over the pair the load gave it, on the instrument and on its
// positions: a crypto trust Schwab types as an equity reads as a fund,
// and a bond Schwab types as an equity as a bond. The type is found
// under the US ISIN of the instrument's CUSIP, its CUSIP or its symbol.
// An ETF whose abbreviated name already reads as fixed income keeps its
// pair. A pinned instrument, an instrument OpenFIGI does not type and an
// instrument of a source whose adapter takes no type keep theirs. A
// second pass changes nothing. Every identifier and name is invented.
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
	refiners := adapterRefiners(t)
	pinned := map[string]map[string]bool{"test-src": {"00000000P": true}}
	n, err := RefineClasses(ctx, db, refiners, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if n != (ClassCounts{Instruments: 2, Positions: 3}) {
		t.Errorf("counts %+v, want 2 instruments and 3 positions", n)
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
		"instrument other-src/00000000T":    "public_equity stock",
		"position other-src/00000000T@1000": "public_equity stock",
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
	again, err := RefineClasses(ctx, db, refiners, pinned)
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
	for _, refiners := range []map[string]ClassRefiner{nil, adapterRefiners(t)} {
		n, err := RefineClasses(ctx, db, refiners, nil)
		if err != nil || n != (ClassCounts{}) {
			t.Errorf("%d refiners: %+v, %v", len(refiners), n, err)
		}
	}
	if got := classPairs(t, ctx, db)["instrument test-src/00000000T"]; !strings.HasPrefix(got, "public_equity") {
		t.Errorf("pair %q", got)
	}
}
