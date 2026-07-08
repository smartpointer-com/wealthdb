package gold

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildFreshAndSwapAbortsOnBadBuild is the load-bearing safety
// check: if build() produces a file that fails verification (here, a
// valid DuckDB that lacks the gold schema), BuildFreshAndSwap must
// return an error, leave the live gold DB byte-for-byte untouched, and
// remove the temp — it must NEVER swap an unverified file into place.
func TestBuildFreshAndSwapAbortsOnBadBuild(t *testing.T) {
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "gold.db")

	// A real, migrated live gold DB (has positions/accounts).
	db, err := Open(goldPath, ModeReadWrite)
	if err != nil {
		t.Fatalf("open live gold: %v", err)
	}
	db.Close()
	liveBefore, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("read live gold: %v", err)
	}

	// build() writes a valid DuckDB file with none of the gold tables,
	// so verifyGold's smoke query fails.
	_, err = BuildFreshAndSwap(context.Background(), goldPath, func(tmp string) error {
		d, e := sql.Open("duckdb", tmp)
		if e != nil {
			return e
		}
		defer d.Close()
		_, e = d.Exec("CREATE TABLE unrelated (x INTEGER)")
		return e
	})
	if err == nil {
		t.Fatal("expected BuildFreshAndSwap to fail verification and abort the swap")
	}

	liveAfter, err := os.ReadFile(goldPath)
	if err != nil {
		t.Fatalf("re-read live gold: %v", err)
	}
	if !bytes.Equal(liveBefore, liveAfter) {
		t.Errorf("live gold changed after an aborted swap (before=%d, after=%d bytes)",
			len(liveBefore), len(liveAfter))
	}
	if matches, _ := filepath.Glob(goldPath + ".rebuild-*"); len(matches) != 0 {
		t.Errorf("aborted swap left temp files: %v", matches)
	}
}
