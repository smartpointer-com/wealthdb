package gold

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

// seedFX inserts a single fx_rates row.
func seedFX(t *testing.T, db *sql.DB, snap int64, base, quote, mid string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
        VALUES (?, ?, ?, ?, CAST(? AS DECIMAL(20,10)))
    `, "test-src", snap, base, quote, mid); err != nil {
		t.Fatalf("seed fx (%d, %s/%s, %s): %v", snap, base, quote, mid, err)
	}
}

func TestLookupRateBaseEqualsQuote(t *testing.T) {
	db, ctx := openMigrated(t)
	rate, err := LookupRate(ctx, db, 1000, "USD", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "1" {
		t.Errorf("rate(USD,USD) = %s, want 1", rate)
	}
}

func TestLookupRateNoData(t *testing.T) {
	db, ctx := openMigrated(t)
	_, err := LookupRate(ctx, db, 1000, "CHF", "USD", canonical.FxModeHistoric)
	if !errors.Is(err, ErrNoRate) {
		t.Errorf("err = %v, want ErrNoRate", err)
	}
}

func TestLookupRateExactMatch(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, 1000, "CHF", "USD", "0.8000")
	seedFX(t, db, 2000, "CHF", "USD", "0.9000")
	rate, err := LookupRate(ctx, db, 1000, "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "0.8" {
		t.Errorf("exact-match rate = %s, want 0.8", rate)
	}
}

func TestLookupRateInterpolation(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, 1000, "CHF", "USD", "0.8000000000")
	seedFX(t, db, 2000, "CHF", "USD", "0.9000000000")
	// At asOf=1500 (midpoint), expect midway rate.
	rate, err := LookupRate(ctx, db, 1500, "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	// 0.8 + (0.9-0.8) * (1500-1000)/(2000-1000) = 0.85
	if rate.String() != "0.85" {
		t.Errorf("interpolated rate = %s, want 0.85", rate)
	}

	// asOf=1750 → 0.8 + 0.1 * 750/1000 = 0.875
	rate, err = LookupRate(ctx, db, 1750, "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "0.875" {
		t.Errorf("interpolated rate(1750) = %s, want 0.875", rate)
	}
}

func TestLookupRateFlatExtrapolation(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, 1000, "CHF", "USD", "0.8")
	seedFX(t, db, 2000, "CHF", "USD", "0.9")

	// asOf before first → flat-extrapolate back (return below=above row)
	rate, err := LookupRate(ctx, db, 500, "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "0.8" {
		t.Errorf("pre-first rate = %s, want 0.8 (flat back)", rate)
	}

	// asOf after last → flat-extrapolate forward
	rate, err = LookupRate(ctx, db, 3000, "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "0.9" {
		t.Errorf("post-last rate = %s, want 0.9 (flat forward)", rate)
	}
}

func TestLookupRateCurrentMode(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, 1000, "CHF", "USD", "0.8")
	seedFX(t, db, 2000, "CHF", "USD", "0.9")
	// FxModeCurrent ignores asOf, returns the latest available.
	rate, err := LookupRate(ctx, db, 500, "CHF", "USD", canonical.FxModeCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if rate.String() != "0.9" {
		t.Errorf("current-mode rate = %s, want 0.9", rate)
	}
}

func TestLookupRateSourcePrecedence(t *testing.T) {
	db, ctx := openMigrated(t)
	ins := func(src string, snap int64, mid string) {
		t.Helper()
		if _, err := db.Exec(`
            INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
            VALUES (?, ?, 'CHF', 'USD', CAST(? AS DECIMAL(20,10)))`,
			src, snap, mid); err != nil {
			t.Fatalf("ins %s @ %d: %v", src, snap, err)
		}
	}
	const day = 86400
	// Same UTC day, but ubs is stamped EARLIER than fred — so a date-only
	// sort would pick fred's rate. Source priority must override that.
	ins("ubs", 100*day+10000, "0.8000")
	ins("fred", 100*day+60000, "0.7900")
	// A historic day only fred covers.
	ins("fred", 90*day, "0.7000")
	asOf := int64(100*day + 86399) // end of the shared day
	hist := int64(90 * day)

	// No precedence configured: date-only → fred's later same-day stamp wins.
	SetFxSourceOrder(nil)
	if r, err := LookupRate(ctx, db, asOf, "CHF", "USD", canonical.FxModeHistoric); err != nil || r.String() != "0.79" {
		t.Fatalf("no-precedence rate = %v (err %v), want 0.79 (latest same-day)", r, err)
	}

	// ubs preferred: the within-day tiebreak picks ubs over fred...
	SetFxSourceOrder([]string{"ubs", "fred"})
	t.Cleanup(func() { SetFxSourceOrder(nil) })
	if r, err := LookupRate(ctx, db, asOf, "CHF", "USD", canonical.FxModeHistoric); err != nil || r.String() != "0.8" {
		t.Fatalf("ubs-preferred rate = %v (err %v), want 0.8", r, err)
	}
	// ...while fred still fills the historic day ubs never covered.
	if r, err := LookupRate(ctx, db, hist, "CHF", "USD", canonical.FxModeHistoric); err != nil || r.String() != "0.7" {
		t.Fatalf("historic rate = %v (err %v), want 0.7 (fred fallback)", r, err)
	}

	// Reversing the order flips the same-day winner — confirms it's
	// config-driven, not the timestamp coincidence.
	SetFxSourceOrder([]string{"fred", "ubs"})
	if r, err := LookupRate(ctx, db, asOf, "CHF", "USD", canonical.FxModeHistoric); err != nil || r.String() != "0.79" {
		t.Fatalf("fred-preferred rate = %v (err %v), want 0.79", r, err)
	}
}

func TestConvertValueDirect(t *testing.T) {
	db, ctx := openMigrated(t)
	// 1 USD = 0.8 CHF
	seedFX(t, db, 1000, "CHF", "USD", "0.8")

	// USD → CHF: amount_chf = amount_usd * 0.8
	out, err := ConvertValue(ctx, db, 1000, canonical.NewDecimalFromInt(100), "USD", "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "80" {
		t.Errorf("100 USD → CHF = %s, want 80", out)
	}
}

func TestConvertValueReciprocal(t *testing.T) {
	db, ctx := openMigrated(t)
	// Only one direction stored: 1 USD = 0.8 CHF.
	seedFX(t, db, 1000, "CHF", "USD", "0.8")

	// CHF → USD requires reciprocal: amount_usd = amount_chf / 0.8 = 1.25 * amount_chf
	out, err := ConvertValue(ctx, db, 1000, canonical.NewDecimalFromInt(80), "CHF", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "100" {
		t.Errorf("80 CHF → USD = %s, want 100", out)
	}
}

func TestConvertValueSameCurrency(t *testing.T) {
	db, ctx := openMigrated(t)
	v := canonical.NewDecimalFromInt(42)
	out, err := ConvertValue(ctx, db, 0, v, "USD", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Equal(v) {
		t.Errorf("same-currency convert changed value: %s → %s", v, out)
	}
}

func TestConvertValueNoRate(t *testing.T) {
	db, ctx := openMigrated(t)
	_, err := ConvertValue(ctx, db, 0, canonical.NewDecimalFromInt(100), "JPY", "BRL", canonical.FxModeHistoric)
	if !errors.Is(err, ErrNoRate) {
		t.Errorf("err = %v, want ErrNoRate", err)
	}
}

// TestConvertValueTriangulatesViaCHF exercises the CHF-pivot
// fallback used when neither direct nor reciprocal exists for the
// requested pair. Mirrors the UBS / Swissquote feed shape (only
// CHF→X pairs are published).
func TestConvertValueTriangulatesViaCHF(t *testing.T) {
	db, ctx := openMigrated(t)
	// 1 EUR = 1.06 CHF, 1 USD = 0.80 CHF (typical UBS feed).
	seedFX(t, db, 1000, "CHF", "EUR", "1.06")
	seedFX(t, db, 1000, "CHF", "USD", "0.80")

	// 100 EUR → USD: should triangulate 100 EUR → 106 CHF → 132.5 USD.
	out, err := ConvertValue(ctx, db, 1000, canonical.NewDecimalFromInt(100), "EUR", "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatalf("triangulate EUR→USD: %v", err)
	}
	if out.StringFixed(2) != "132.50" {
		t.Errorf("100 EUR → USD = %s, want 132.50", out)
	}

	// USD → EUR: triangulate 100 USD → 80 CHF → 75.4716...EUR
	out, err = ConvertValue(ctx, db, 1000, canonical.NewDecimalFromInt(100), "USD", "EUR", canonical.FxModeHistoric)
	if err != nil {
		t.Fatalf("triangulate USD→EUR: %v", err)
	}
	if got := out.StringFixed(4); got != "75.4717" {
		t.Errorf("100 USD → EUR = %s, want 75.4717", got)
	}
}

// TestConvertValueTriangulationMissingLeg confirms that when one
// of the two legs through CHF is absent we surface ErrNoRate (and
// the message mentions which leg failed).
func TestConvertValueTriangulationMissingLeg(t *testing.T) {
	db, ctx := openMigrated(t)
	// Only the EUR leg exists. USD leg missing.
	seedFX(t, db, 1000, "CHF", "EUR", "1.06")

	_, err := ConvertValue(ctx, db, 1000, canonical.NewDecimalFromInt(100), "EUR", "USD", canonical.FxModeHistoric)
	if !errors.Is(err, ErrNoRate) {
		t.Errorf("err = %v, want ErrNoRate", err)
	}
}
