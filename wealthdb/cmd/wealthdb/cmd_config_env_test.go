package main

import (
	"bytes"
	"strings"
	"testing"
)

// The lifecycle scripts' names print what config-env prints for their
// block, byte for byte.
func TestConfigEnvAliases(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{"gold_db": "/tmp/it's.db", "default_currency": "CHF", "silver_sources": [],
		"web": {"enabled": true, "port": 3100}, "mcp": {"enabled": true, "port": 3500}}`)
	for alias, block := range map[string]string{"web-config": "web", "mcp-config": "mcp"} {
		var aliased, direct, errb bytes.Buffer
		if code := Run([]string{"-c", cfg, alias}, nil, &aliased, &errb); code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", alias, code, errb.String())
		}
		if code := Run([]string{"-c", cfg, "config-env", block}, nil, &direct, &errb); code != 0 {
			t.Fatalf("config-env %s: exit %d, stderr=%s", block, code, errb.String())
		}
		if aliased.String() != direct.String() || aliased.Len() == 0 {
			t.Errorf("%s printed:\n%s\nconfig-env %s printed:\n%s", alias, aliased.String(), block, direct.String())
		}
	}
}

// The sources block is one line per silver file, in config order, every
// value quoted for eval.
func TestConfigEnvSources(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{"gold_db": "/tmp/g.db", "default_currency": "USD", "silver_sources": [
		{"id": "ct", "kind": "cointracking", "path": "/data/ct silver.db"},
		{"id": "bank", "kind": "ubs", "subsources": [{"kind": "web", "path": "/data/web.db"},
			{"kind": "psn", "path": "/data/psn.db"}]}]}`)
	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "config-env", "sources"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	want := `WEALTHDB_SOURCE_ID='ct' WEALTHDB_SOURCE_KIND='cointracking' WEALTHDB_SILVER_KIND='' WEALTHDB_SILVER_PATH='/data/ct silver.db'
WEALTHDB_SOURCE_ID='bank' WEALTHDB_SOURCE_KIND='ubs' WEALTHDB_SILVER_KIND='web' WEALTHDB_SILVER_PATH='/data/web.db'
WEALTHDB_SOURCE_ID='bank' WEALTHDB_SOURCE_KIND='ubs' WEALTHDB_SILVER_KIND='psn' WEALTHDB_SILVER_PATH='/data/psn.db'
`
	if out.String() != want {
		t.Errorf("sources:\n%s\nwant\n%s", out.String(), want)
	}
}

// A missing or unknown block is a usage error that names the blocks,
// before the config is read.
func TestConfigEnvRefusesABadBlock(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"nope"}, {"web", "mcp"}} {
		var out, errb bytes.Buffer
		argv := append([]string{"-c", "/nonexistent/wealthdb.cfg", "config-env"}, args...)
		if code := Run(argv, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "mcp | sources | web") {
			t.Errorf("%v: exit %d, stderr=%s", args, code, errb.String())
		}
	}
}
