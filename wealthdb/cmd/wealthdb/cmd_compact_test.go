package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bloatGold grows the on-disk gold file by writing then dropping a
// large scratch table. DuckDB frees the dropped blocks to an internal
// free-list but never truncates the file, reproducing the dead-space
// bloat that repeated in-place loads leave behind — the exact
// condition `compact` is meant to reclaim.
func bloatGold(t *testing.T, goldPath string) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath)
	if err != nil {
		t.Fatalf("open gold for bloat: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE _bloat (id BIGINT, pad VARCHAR)`,
		`INSERT INTO _bloat
		   SELECT i, md5(random()::VARCHAR) || md5(random()::VARCHAR) ||
		             md5(random()::VARCHAR) || md5(random()::VARCHAR)
		     FROM range(200000) t(i)`,
		`CHECKPOINT`,
		`DROP TABLE _bloat`,
		`CHECKPOINT`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("bloat step failed: %v", err)
		}
	}
}

// goldStat opens the gold file read-only and returns the row counts of
// the core tables plus the schema version, so a test can assert a
// rewrite preserved them exactly.
func goldStat(t *testing.T, goldPath string) (counts map[string]int64, schemaVersion int64) {
	t.Helper()
	db, err := sql.Open("duckdb", goldPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("open gold read-only: %v", err)
	}
	defer db.Close()
	counts = map[string]int64{}
	for _, tbl := range []string{"positions", "transactions", "fx_rates", "cash_balances", "accounts"} {
		var n int64
		if err := db.QueryRow("SELECT count(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		counts[tbl] = n
	}
	if err := db.QueryRow("SELECT COALESCE(MAX(gold_schema_version), 0) FROM schema_meta").Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return counts, schemaVersion
}

func fileSizeT(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Size()
}

// goldPathFromCfg derives the gold_db path setupCLITest wrote (it
// lives next to the config, named wealthdb.db).
func goldPathFromCfg(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "wealthdb.db")
}

// TestCompactShrinksAndPreservesRows bloats a loaded gold DB, then
// confirms `compact` shrinks the file while every core row count and
// the schema version are byte-for-byte identical.
func TestCompactShrinksAndPreservesRows(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	bloatGold(t, goldPath)

	beforeCounts, beforeVer := goldStat(t, goldPath)
	beforeSize := fileSizeT(t, goldPath)

	so, se, code := run(t, "-c", cfg, "compact")
	if code != 0 {
		t.Fatalf("compact failed: code=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "reclaimed") {
		t.Errorf("compact stdout missing reclaim report: %q", so)
	}

	afterSize := fileSizeT(t, goldPath)
	if afterSize >= beforeSize {
		t.Errorf("compact did not shrink file: before=%d after=%d", beforeSize, afterSize)
	}
	afterCounts, afterVer := goldStat(t, goldPath)
	if afterVer != beforeVer {
		t.Errorf("schema version changed: before=%d after=%d", beforeVer, afterVer)
	}
	for tbl, want := range beforeCounts {
		if afterCounts[tbl] != want {
			t.Errorf("row count for %s changed: before=%d after=%d", tbl, want, afterCounts[tbl])
		}
	}
}

// TestCompactDryRunChangesNothing confirms --dry-run reports sizes but
// leaves the live file untouched.
func TestCompactDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	bloatGold(t, goldPath)

	before, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("read gold: %v", err)
	}

	so, se, code := run(t, "-c", cfg, "compact", "--dry-run")
	if code != 0 {
		t.Fatalf("compact --dry-run failed: code=%d stderr=%s", code, se)
	}
	if !strings.Contains(so, "reclaim") {
		t.Errorf("dry-run stdout missing reclaim report: %q", so)
	}

	after, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("re-read gold: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("dry-run changed the live file (before=%d bytes, after=%d bytes)",
			len(before), len(after))
	}
	// The dry-run temp must not linger next to the live DB.
	matches, _ := filepath.Glob(goldPath + ".compact-dryrun-*")
	if len(matches) != 0 {
		t.Errorf("dry-run left temp files: %v", matches)
	}
}

// TestReloadFreshEqualsInPlace confirms the default `reload -a`
// (build-fresh-and-swap) yields the same gold rows as
// `reload -a --in-place`, and produces a smaller file when the
// in-place DB carries accumulated dead space.
func TestReloadFreshEqualsInPlace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	silverPath := filepath.Join(dir, "schwab.db")
	sdb, err := sql.Open("sqlite", "file:"+silverPath)
	if err != nil {
		t.Fatalf("open silver: %v", err)
	}
	if _, err := sdb.Exec(silverFixture); err != nil {
		t.Fatalf("apply silver fixture: %v", err)
	}
	sdb.Close()

	writeCfg := func(goldPath string) string {
		p := filepath.Join(dir, filepath.Base(goldPath)+".cfg")
		body := fmt.Sprintf(`{
            "gold_db": %q,
            "default_currency": "USD",
            "silver_sources": [{"id":"schwab-test","kind":"schwab","path":%q}]
        }`, goldPath, silverPath)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write cfg: %v", err)
		}
		return p
	}

	goldInPlace := filepath.Join(dir, "inplace.db")
	goldFresh := filepath.Join(dir, "fresh.db")
	cfgInPlace := writeCfg(goldInPlace)
	cfgFresh := writeCfg(goldFresh)

	for _, c := range []string{cfgInPlace, cfgFresh} {
		if _, _, code := run(t, "-c", c, "init"); code != 0 {
			t.Fatalf("init failed for %s", c)
		}
		if _, _, code := run(t, "-c", c, "load", "schwab-test"); code != 0 {
			t.Fatalf("load failed for %s", c)
		}
	}

	// Give the in-place DB accumulated dead space, then reload in place
	// (which never truncates the file).
	bloatGold(t, goldInPlace)
	if _, se, code := run(t, "-c", cfgInPlace, "reload", "-a", "--in-place"); code != 0 {
		t.Fatalf("reload -a --in-place failed: code=%d stderr=%s", code, se)
	}
	// Default fresh reload swaps in a freshly built (compact) file.
	if _, se, code := run(t, "-c", cfgFresh, "reload", "-a"); code != 0 {
		t.Fatalf("reload -a (fresh) failed: code=%d stderr=%s", code, se)
	}

	inPlaceCounts, _ := goldStat(t, goldInPlace)
	freshCounts, _ := goldStat(t, goldFresh)
	for tbl, want := range inPlaceCounts {
		if freshCounts[tbl] != want {
			t.Errorf("row count mismatch for %s: in-place=%d fresh=%d", tbl, want, freshCounts[tbl])
		}
	}

	if fileSizeT(t, goldFresh) >= fileSizeT(t, goldInPlace) {
		t.Errorf("fresh reload should be smaller than bloated in-place: fresh=%d in-place=%d",
			fileSizeT(t, goldFresh), fileSizeT(t, goldInPlace))
	}
}

// TestReloadFreshFailedBuildLeavesLiveUntouched confirms a build that
// errors mid-flight never swaps: the live gold DB keeps its data and
// no stray rebuild file is left behind.
func TestReloadFreshFailedBuildLeavesLiveUntouched(t *testing.T) {
	t.Parallel()
	cfg := setupCLITest(t)
	if _, _, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := run(t, "-c", cfg, "load", "schwab-test"); code != 0 {
		t.Fatal("load failed")
	}
	goldPath := goldPathFromCfg(cfg)
	beforeCounts, _ := goldStat(t, goldPath)
	if beforeCounts["positions"] == 0 {
		t.Fatal("fixture should have loaded at least one position")
	}

	// Corrupt the backing silver so the fresh build's Load fails.
	silverPath := filepath.Join(filepath.Dir(cfg), "schwab.db")
	if err := os.WriteFile(silverPath, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("corrupt silver: %v", err)
	}

	_, se, code := run(t, "-c", cfg, "reload", "-a")
	if code == 0 {
		t.Errorf("reload -a should fail on corrupt silver; stderr=%s", se)
	}

	// Live DB unchanged: same row counts, no leftover rebuild temp.
	afterCounts, _ := goldStat(t, goldPath)
	for tbl, want := range beforeCounts {
		if afterCounts[tbl] != want {
			t.Errorf("live gold changed after failed build for %s: before=%d after=%d", tbl, want, afterCounts[tbl])
		}
	}
	matches, _ := filepath.Glob(goldPath + ".rebuild-*")
	if len(matches) != 0 {
		t.Errorf("failed build left rebuild temp files: %v", matches)
	}
}
