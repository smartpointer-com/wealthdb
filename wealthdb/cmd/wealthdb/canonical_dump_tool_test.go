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
			spec, err := src.ToSilverOpenSpec()
			if err != nil {
				t.Fatal(err)
			}
			conn, err := adapter.Open(ctx, spec)
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
