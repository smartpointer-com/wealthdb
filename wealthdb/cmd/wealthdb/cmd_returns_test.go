package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/gold"
)

// setupReturnsGold seeds a synthetic gold DB file (a flow-complete brokerage, a
// mortgage liability, a NAV-only holding, plus a USD→CHF rate) and returns a
// config path pointing at it. Drives the returns command end-to-end via Run().
func setupReturnsGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")

	db, err := gold.Open(goldPath, gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, s := range [][2]string{{"schwab", "schwab"}, {"manualre", "manual"}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
				high_watermark, first_loaded_at, last_loaded_at)
			VALUES (?, ?, ?, -1, 0, 0)`, s[0], s[1], "/tmp/"+s[0]+".db"); err != nil {
			t.Fatalf("seed source %s: %v", s[0], err)
		}
	}

	day := func(y int, m time.Month, dd int) int64 {
		return time.Date(y, m, dd, 12, 0, 0, 0, time.UTC).Unix()
	}
	t0, tMid, t1 := day(2024, time.January, 2), day(2024, time.April, 1), day(2024, time.July, 2)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		VALUES ('schwab', ?, 'USD', 'CHF', CAST('1.1' AS DECIMAL(20,10)))`, day(2024, time.January, 1)); err != nil {
		t.Fatalf("seed fx: %v", err)
	}

	usd := "USD"
	d := func(n int64) *canonical.Decimal { v := canonical.NewDecimalFromInt(n); return &v }
	seed := func(fn func(*gold.Writer) error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := fn(gold.NewWriter(tx)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	seed(func(w *gold.Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "schwab", AccountExternalID: "BROK1", AccountKind: canonical.AccountKindBrokerage,
				DisplayName: strp("Brokerage"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
			{SilverSourceID: "schwab", AccountExternalID: "MORT1", AccountKind: canonical.AccountKindMortgage,
				DisplayName: strp("Mortgage"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
			{SilverSourceID: "manualre", AccountExternalID: "RE1", AccountKind: canonical.AccountKindOther,
				DisplayName: strp("Real estate"), BaseCurrency: &usd, FirstSeenAt: t0, LastSeenAt: t1},
		})
	})
	seed(func(w *gold.Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(1000)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "BROK1", PositionKey: "AAA",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(1200)},
			{SilverSourceID: "schwab", SnapshotAt: t0, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(-500)},
			{SilverSourceID: "schwab", SnapshotAt: t1, AccountExternalID: "MORT1", PositionKey: "M",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(-480)},
			{SilverSourceID: "manualre", SnapshotAt: t0, AccountExternalID: "RE1", PositionKey: "RE",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(2000)},
			{SilverSourceID: "manualre", SnapshotAt: t1, AccountExternalID: "RE1", PositionKey: "RE",
				AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: d(2100)},
		}); err != nil {
			return err
		}
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: "schwab", TransactionExternalID: "TX1", OccurredAt: tMid,
			AccountExternalID: "BROK1", Kind: canonical.TxKindDeposit, Currency: "USD", NetAmount: d(100),
		}})
	})
	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}

	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": []}`, goldPath)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfg
}

func strp(s string) *string { return &s }

func TestReturnsCLIEndToEnd(t *testing.T) {
	cfg := setupReturnsGold(t)

	t.Run("accounts both total", func(t *testing.T) {
		so, se, code := run(t, "-c", cfg, "returns", "accounts", "-x", "USD", "--method", "both", "--period", "total")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, want := range []string{"twr_%", "mwr_%", "Brokerage", "nonpositive_base", "nav_only"} {
			if !strings.Contains(so, want) {
				t.Errorf("stdout missing %q:\n%s", want, so)
			}
		}
	})

	t.Run("global in CHF resolves via FX", func(t *testing.T) {
		so, se, code := run(t, "-c", cfg, "returns", "global", "-x", "CHF")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "start_CHF") {
			t.Errorf("CHF header missing:\n%s", so)
		}
	})

	t.Run("period variants + sources", func(t *testing.T) {
		for _, p := range []string{"monthly", "quarterly", "annual", "total"} {
			if _, se, code := run(t, "-c", cfg, "returns", "sources", "--period", p); code != 0 {
				t.Errorf("--period %s exit=%d stderr=%s", p, code, se)
			}
		}
	})

	t.Run("json and csv", func(t *testing.T) {
		so, se, code := run(t, "-c", cfg, "returns", "accounts", "-f", "json", "--method", "both")
		if code != 0 {
			t.Fatalf("json exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "\"twr\"") && !strings.Contains(so, "twr") {
			t.Errorf("json missing twr field:\n%s", so)
		}
		if _, _, code := run(t, "-c", cfg, "returns", "accounts", "-f", "csv"); code != 0 {
			t.Errorf("csv exit=%d", code)
		}
	})

	t.Run("privacy redacts amounts but keeps returns", func(t *testing.T) {
		so, _, code := run(t, "-c", cfg, "returns", "accounts", "-p", "--method", "both")
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.Contains(so, "1200.00") {
			t.Errorf("privacy leaked a money amount:\n%s", so)
		}
	})

	t.Run("unknown view errors", func(t *testing.T) {
		_, se, code := run(t, "-c", cfg, "returns", "nope")
		if code != 2 {
			t.Errorf("exit=%d, want 2", code)
		}
		if !strings.Contains(se, "unknown view") {
			t.Errorf("stderr missing 'unknown view': %s", se)
		}
	})

	t.Run("bad flag errors", func(t *testing.T) {
		if _, _, code := run(t, "-c", cfg, "returns", "accounts", "--method", "bogus"); code != 2 {
			t.Errorf("invalid --method exit=%d, want 2", code)
		}
	})
}
