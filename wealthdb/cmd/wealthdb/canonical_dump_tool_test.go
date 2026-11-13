package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalDumpTool writes the canonical dump (silvertest.DumpCanonical,
// since change number 0) of every silver source the config at
// WEALTHDB_CONFIG names, one file per source, <id>.canonical, into the
// directory WEALTHDB_CANONICAL_OUT. Each source opens through the spec the
// loader builds (config.SilverSource.ToSilverOpenSpec), read-only.
//
// It is a tool, not a check: it runs only when WEALTHDB_CANONICAL_OUT is
// set. Run it on one tree, then on another, and compare the directories
// with diff -r; a refactoring of an adapter leaves them equal. Over a real
// config the files hold real data, so the directory must lie outside the
// repository, and the test refuses one inside it.
//
// The tests run in the image (wealthdb-test), which gets no host variable
// and sees the module directory, the Go cache and the directory of a
// WEALTHDB_GOLD_* file, read-only, and no other path. go test's -exec
// hands the variables in, and the output goes under the Go cache, the one
// writable mount outside the repository. The config and every silver file
// it names must lie under a mounted directory, such as a demo root mounted
// through WEALTHDB_GOLD_VERIFY (the demo keeps silver/ under its root). A
// silver file in WAL mode cannot open on the read-only mount. Pass only
// this test's -run:
//
//	out=${XDG_CACHE_HOME:-$HOME/.cache}/wealthdb/go/canonical/before
//	demo=$HOME/wealthdb-demo
//	WEALTHDB_GOLD_VERIFY=$demo/wealthdb.db wealthdb/wealthdb-test -count=1 \
//	    -run '^TestCanonicalDumpTool$' \
//	    -exec "env WEALTHDB_CANONICAL_OUT=$out WEALTHDB_CONFIG=$demo/wealthdb.cfg" \
//	    ./cmd/wealthdb
func TestCanonicalDumpTool(t *testing.T) {
	out := os.Getenv("WEALTHDB_CANONICAL_OUT")
	if out == "" {
		t.Skip("set WEALTHDB_CANONICAL_OUT=<dir> and WEALTHDB_CONFIG=<config> to write the canonical dumps")
	}
	cfgPath := os.Getenv("WEALTHDB_CONFIG")
	if cfgPath == "" {
		t.Fatal("WEALTHDB_CANONICAL_OUT is set but WEALTHDB_CONFIG is not")
	}
	out, err := filepath.Abs(out)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(repo, out); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("WEALTHDB_CANONICAL_OUT %s lies inside the repository %s", out, repo)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, src := range cfg.SilverSources {
		t.Run(src.ID, func(t *testing.T) {
			adapter, err := silver.Get(src.Kind)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := adapter.Open(ctx, src.ToSilverOpenSpec())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			dump, err := silvertest.DumpCanonical(ctx, conn, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, src.ID+".canonical"), dump, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
