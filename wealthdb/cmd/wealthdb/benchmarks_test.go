package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupBenchmarkGold is the returns fixture with a benchmark whose fund
// returned 1% a month over the first quarter of 2024, and its look-through
// rows.
func setupBenchmarkGold(t *testing.T) string {
	t.Helper()
	cfgPath := setupReturnsGold(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := gold.SyncBenchmarks(ctx, db, []canonical.Benchmark{{Name: "Test mix",
		Funds: []canonical.BenchmarkFund{{FundRef: "symbol:QIDX", WeightPct: 100}}}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []time.Month{time.January, time.February, time.March} {
		end := time.Date(2024, m+1, 0, 0, 0, 0, 0, time.UTC)
		if _, err := db.ExecContext(ctx, `INSERT INTO fund_returns (silver_source_id, fund_ref, as_of, month, total_return)
VALUES ('schwab', 'symbol:QIDX', ?, ?, 1)`, end.Unix(), end.Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO benchmark_exposure (computed_at, benchmark, dimension, bucket, share_pct)
VALUES (1, 'Test mix', 'asset_class', 'public_equity', 70), (1, 'Test mix', 'asset_class', 'fixed_income', 30)`); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestHelpBenchmarksListsTheSet(t *testing.T) {
	t.Parallel()
	cfg := setupReturnsGold(t)
	so, se, code := runToday(t, "-c", cfg, "help", "benchmarks")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	for _, want := range []string{"Global 60/40", "symbol:ACWI 60, symbol:BND 40", "built-in"} {
		if !strings.Contains(so, want) {
			t.Errorf("help misses %q:\n%s", want, so)
		}
	}
}

func TestReturnsBenchmarkBesideTheGlobalView(t *testing.T) {
	t.Parallel()
	cfg := setupBenchmarkGold(t)
	so, se, code := runToday(t, "-c", cfg, "returns", "global", "--benchmark", "Test mix", "--period", "total",
		"-f", "csv", "-C", "benchmark_twr,compared_to,benchmark_quality")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if !strings.Contains(so, "2.96") || !strings.Contains(so, "2024-03-31") || !strings.Contains(so, "benchmark_end_clipped") {
		t.Errorf("output:\n%s", so)
	}
}

func TestReturnsBenchmarkIsRefusedOffTheGlobalView(t *testing.T) {
	t.Parallel()
	cfg := setupBenchmarkGold(t)
	_, se, code := runToday(t, "-c", cfg, "returns", "sources", "--benchmark", "Test mix")
	if code == 0 || !strings.Contains(se, "global view") {
		t.Errorf("exit %d: %s", code, se)
	}
}

func TestReturnsBenchmarkNamesTheFixForAnUnknownName(t *testing.T) {
	t.Parallel()
	cfg := setupBenchmarkGold(t)
	_, se, code := runToday(t, "-c", cfg, "returns", "global", "--benchmark", "Nope")
	if code == 0 || !strings.Contains(se, "help benchmarks") {
		t.Errorf("exit %d: %s", code, se)
	}
}

func TestExposureBenchmarkWeights(t *testing.T) {
	t.Parallel()
	cfg := setupBenchmarkGold(t)
	so, se, code := runToday(t, "-c", cfg, "exposure", "classes", "--benchmark", "Test mix", "-f", "csv",
		"-C", "asset_class,benchmark,difference")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, se)
	}
	if !strings.Contains(so, "benchmark_pct") || !strings.Contains(so, "difference_pct") {
		t.Errorf("output:\n%s", so)
	}
}

func TestExposureBenchmarkRefusals(t *testing.T) {
	t.Parallel()
	cfg := setupBenchmarkGold(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"exposure", "funds", "--benchmark", "Test mix"}, "belongs to"},
		{[]string{"exposure", "classes", "--benchmark", "Test mix", "--period", "quarterly", "2024"}, "no --period"},
		{[]string{"exposure", "classes", "--benchmark", "Nope"}, "help benchmarks"},
		{[]string{"exposure", "countries", "--benchmark", "Test mix", "--country", "domicile"}, "country of risk"},
		{[]string{"exposure", "sectors", "--benchmark", "Test mix"}, "web refresh"},
	} {
		_, se, code := runToday(t, append([]string{"-c", cfg}, c.args...)...)
		if code == 0 || !strings.Contains(se, c.want) {
			t.Errorf("%v: exit %d: %s", c.args, code, se)
		}
	}
}
