package gold

import (
	"database/sql"
	"testing"
)

// seedFX inserts a single fx_rates row attributed to the default
// 'test-src' source. snap is an epoch-seconds timestamp; its UTC-day
// bucket (snap // 86400) is what the fx_daily view keys on, so tests
// that care about at-or-before selection must space their snaps into
// distinct days.
func seedFX(t *testing.T, db *sql.DB, snap int64, base, quote, mid string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
        VALUES (?, ?, ?, ?, CAST(? AS DECIMAL(20,10)))
    `, "test-src", snap, base, quote, mid); err != nil {
		t.Fatalf("seed fx (%d, %s/%s, %s): %v", snap, base, quote, mid, err)
	}
}

// fxConvert reproduces the report macros' currency conversion against
// the fx_daily view for one (day, amount, from, to): identity when
// from == to, otherwise the first of the direct / CHF-triangulated /
// USD-triangulated paths that resolves, each via an at-or-before ASOF
// match on the day bucket. It is the exact COALESCE/ASOF shape the
// report_* macros embed (migration 0021), isolated so the FX layer
// can be asserted without seeding whole accounts. Returns the
// DECIMAL(28,4) string the macros render, or ok=false when no path
// resolves (the macros' NULL). `day` is a UTC-day bucket (epoch //
// 86400), matching fx_daily.day.
func fxConvert(t *testing.T, db *sql.DB, day int64, amount float64, from, to string) (string, bool) {
	t.Helper()
	const q = `
        SELECT CAST(CAST(COALESCE(
                 CASE WHEN base.fromc = base.toc THEN base.amt END,
                 base.amt * direct.rate,
                 base.amt * chf1.rate * chf2.rate,
                 base.amt * usd1.rate * usd2.rate
               ) AS DECIMAL(28,4)) AS VARCHAR)
          FROM (SELECT CAST(? AS BIGINT) AS d, CAST(? AS DOUBLE) AS amt,
                       CAST(? AS VARCHAR) AS fromc, CAST(? AS VARCHAR) AS toc) base
          ASOF LEFT JOIN fx_daily direct
            ON direct.from_ccy = base.fromc AND direct.to_ccy = base.toc AND direct.day <= base.d
          ASOF LEFT JOIN fx_daily chf1
            ON chf1.from_ccy = base.fromc AND chf1.to_ccy = 'CHF' AND chf1.day <= base.d
          ASOF LEFT JOIN fx_daily chf2
            ON chf2.from_ccy = 'CHF' AND chf2.to_ccy = base.toc AND chf2.day <= base.d
          ASOF LEFT JOIN fx_daily usd1
            ON usd1.from_ccy = base.fromc AND usd1.to_ccy = 'USD' AND usd1.day <= base.d
          ASOF LEFT JOIN fx_daily usd2
            ON usd2.from_ccy = 'USD' AND usd2.to_ccy = base.toc AND usd2.day <= base.d`
	var got sql.NullString
	if err := db.QueryRow(q, day, amount, from, to).Scan(&got); err != nil {
		t.Fatalf("fxConvert(day=%d, %v %s→%s): %v", day, amount, from, to, err)
	}
	return got.String, got.Valid
}

// An fx_rates row (base=X, quote=Y, mid=R) means "1 Y = R X", so
// fx_norm yields a direct Y→X rate of R and a reciprocal X→Y rate of
// 1/R. The (CHF, USD, 0.8) rows below therefore give USD→CHF = ×0.8
// and CHF→USD = ×1.25.

func TestFxIdentity(t *testing.T) {
	db, _ := openMigrated(t)
	if v, ok := fxConvert(t, db, 0, 42, "USD", "USD"); !ok || v != "42.0000" {
		t.Errorf("USD→USD identity = %q (ok=%v), want 42.0000", v, ok)
	}
}

func TestFxDirect(t *testing.T) {
	db, _ := openMigrated(t)
	seedFX(t, db, 0, "CHF", "USD", "0.8")
	if v, ok := fxConvert(t, db, 0, 100, "USD", "CHF"); !ok || v != "80.0000" {
		t.Errorf("100 USD→CHF = %q (ok=%v), want 80.0000", v, ok)
	}
}

func TestFxReciprocal(t *testing.T) {
	db, _ := openMigrated(t)
	// Only one direction stored (1 USD = 0.8 CHF); CHF→USD must use 1/0.8.
	seedFX(t, db, 0, "CHF", "USD", "0.8")
	if v, ok := fxConvert(t, db, 0, 80, "CHF", "USD"); !ok || v != "100.0000" {
		t.Errorf("80 CHF→USD = %q (ok=%v), want 100.0000", v, ok)
	}
}

func TestFxNoRate(t *testing.T) {
	db, _ := openMigrated(t)
	if v, ok := fxConvert(t, db, 0, 100, "CHF", "USD"); ok {
		t.Errorf("CHF→USD with no rates = %q (ok=%v), want no result", v, ok)
	}
}

// TestFxTriangulatesViaCHF exercises the CHF-pivot fallback used when
// neither direct nor reciprocal exists for the pair — the UBS /
// Swissquote feed shape (only CHF→X pairs published).
func TestFxTriangulatesViaCHF(t *testing.T) {
	db, _ := openMigrated(t)
	// 1 EUR = 1.06 CHF, 1 USD = 0.80 CHF.
	seedFX(t, db, 0, "CHF", "EUR", "1.06")
	seedFX(t, db, 0, "CHF", "USD", "0.80")

	// 100 EUR → 106 CHF → 132.5 USD.
	if v, ok := fxConvert(t, db, 0, 100, "EUR", "USD"); !ok || v != "132.5000" {
		t.Errorf("100 EUR→USD = %q (ok=%v), want 132.5000", v, ok)
	}
	// 100 USD → 80 CHF → 75.4716...EUR (DECIMAL(28,4) → 75.4717).
	if v, ok := fxConvert(t, db, 0, 100, "USD", "EUR"); !ok || v != "75.4717" {
		t.Errorf("100 USD→EUR = %q (ok=%v), want 75.4717", v, ok)
	}
}

// TestFxTriangulationMissingLeg confirms a half-present CHF pivot
// resolves to no result rather than a wrong one.
func TestFxTriangulationMissingLeg(t *testing.T) {
	db, _ := openMigrated(t)
	seedFX(t, db, 0, "CHF", "EUR", "1.06") // USD leg absent
	if v, ok := fxConvert(t, db, 0, 100, "EUR", "USD"); ok {
		t.Errorf("EUR→USD with only the EUR leg = %q (ok=%v), want no result", v, ok)
	}
}

// TestFxFlatAtOrBefore pins the no-interpolation contract: between two
// dated rates the earlier one is carried forward flat (not blended),
// and a date before the first rate yields nothing (no backward
// extrapolation). Days 10 and 12 are distinct UTC-day buckets.
func TestFxFlatAtOrBefore(t *testing.T) {
	db, _ := openMigrated(t)
	const day = int64(86400)
	seedFX(t, db, 10*day, "CHF", "USD", "0.8")
	seedFX(t, db, 12*day, "CHF", "USD", "0.9")

	// Between the two: flat-carry the day-10 rate (NOT interpolated 0.85).
	if v, ok := fxConvert(t, db, 11, 100, "USD", "CHF"); !ok || v != "80.0000" {
		t.Errorf("day 11 (flat) = %q (ok=%v), want 80.0000", v, ok)
	}
	// After the last: carry the day-12 rate.
	if v, ok := fxConvert(t, db, 20, 100, "USD", "CHF"); !ok || v != "90.0000" {
		t.Errorf("day 20 = %q (ok=%v), want 90.0000", v, ok)
	}
	// Before the first: nothing at-or-before → no result.
	if v, ok := fxConvert(t, db, 9, 100, "USD", "CHF"); ok {
		t.Errorf("day 9 (pre-first) = %q (ok=%v), want no result", v, ok)
	}
}

// TestFxLatestSnapshotWinsWithinDay confirms that, absent a priority
// tiebreak, the latest snapshot in a UTC day supplies that day's rate.
func TestFxLatestSnapshotWinsWithinDay(t *testing.T) {
	db, _ := openMigrated(t)
	seedFX(t, db, 10, "CHF", "USD", "0.8") // same day (bucket 0)...
	seedFX(t, db, 20, "CHF", "USD", "0.9") // ...later stamp wins
	if v, ok := fxConvert(t, db, 0, 100, "USD", "CHF"); !ok || v != "90.0000" {
		t.Errorf("within-day latest = %q (ok=%v), want 90.0000", v, ok)
	}
}

// TestFxSourcePrecedence asserts that silver_sources.fx_priority (set
// by SetFxPriorities) overrides the within-day latest-snapshot
// tiebreak, and that reversing the order flips the winner.
func TestFxSourcePrecedence(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, src := range []string{"ubs", "fred"} {
		if _, err := db.Exec(`
            INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
                high_watermark, first_loaded_at, last_loaded_at)
            VALUES (?, 'ubs', '/tmp/x.db', -1, 0, 0)`, src); err != nil {
			t.Fatalf("seed source %s: %v", src, err)
		}
	}
	ins := func(src string, snap int64, mid string) {
		t.Helper()
		if _, err := db.Exec(`
            INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
            VALUES (?, ?, 'CHF', 'USD', CAST(? AS DECIMAL(20,10)))`, src, snap, mid); err != nil {
			t.Fatalf("ins %s: %v", src, err)
		}
	}
	const day = int64(100)
	// Same UTC day; ubs stamped earlier than fred, so date-only would
	// pick fred. Priority must override.
	ins("ubs", day*86400+10000, "0.8000")
	ins("fred", day*86400+60000, "0.7900")

	// No priority: both NULL → date-only → fred's later same-day stamp.
	if v, ok := fxConvert(t, db, day, 100, "USD", "CHF"); !ok || v != "79.0000" {
		t.Fatalf("no-priority USD→CHF = %q (ok=%v), want 79.0000 (latest same-day)", v, ok)
	}
	// ubs preferred → its 0.80 wins despite the earlier stamp.
	if err := SetFxPriorities(ctx, db, []string{"ubs", "fred"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := fxConvert(t, db, day, 100, "USD", "CHF"); !ok || v != "80.0000" {
		t.Fatalf("ubs-preferred = %q, want 80.0000", v)
	}
	// Reversing the order flips the winner back to fred — confirms it's
	// priority-driven, not the timestamp coincidence.
	if err := SetFxPriorities(ctx, db, []string{"fred", "ubs"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := fxConvert(t, db, day, 100, "USD", "CHF"); !ok || v != "79.0000" {
		t.Fatalf("fred-preferred = %q, want 79.0000", v)
	}
}
