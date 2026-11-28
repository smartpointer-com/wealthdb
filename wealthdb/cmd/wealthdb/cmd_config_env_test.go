package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		if code := Run(argv, nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "mcp | refresh | sources | web") {
			t.Errorf("%v: exit %d, stderr=%s", args, code, errb.String())
		}
	}
}

// webTestCfg writes a config file of body and returns its path.
func webTestCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigEnvWebEmitsShellEnv(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{
		"gold_db": "/Users/Shared/x/wealthdb.db",
		"default_currency": "GBP",
		"silver_sources": [],
		"web": {"enabled": true, "port": 4444},
		"lots": {"missing_basis": "zero"}
	}`)

	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "config-env", "web"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"WEALTHDB_WEB_ENABLED=1",
		"WEALTHDB_WEB_PORT=4444",
		`WEALTHDB_GOLD_DB='/Users/Shared/x/wealthdb.db'`,
		`WEALTHDB_DEFAULT_CURRENCY='GBP'`,
		`WEALTHDB_MISSING_BASIS='zero'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestConfigEnvWebDefaultsWhenOmitted(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{
		"gold_db": "/tmp/g.db",
		"default_currency": "USD",
		"silver_sources": []
	}`)

	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "config-env", "web"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, `WEALTHDB_MISSING_BASIS='ignore'`) {
		t.Errorf("the missing basis defaults to ignore:\n%s", got)
	}
	if !strings.Contains(got, "WEALTHDB_WEB_ENABLED=0") {
		t.Errorf("want disabled, got:\n%s", got)
	}
	if !strings.Contains(got, "WEALTHDB_WEB_PORT=3000") {
		t.Errorf("want default port 3000, got:\n%s", got)
	}
}

func TestConfigEnvWebQuotesGoldPath(t *testing.T) {
	t.Parallel()
	// A path containing a single quote must survive `eval` in the
	// host wrapper.
	cfg := webTestCfg(t, `{
		"gold_db": "/tmp/o'brien/wealthdb.db",
		"default_currency": "USD",
		"silver_sources": []
	}`)
	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "config-env", "web"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), `WEALTHDB_GOLD_DB='/tmp/o'\''brien/wealthdb.db'`) {
		t.Errorf("single-quote not escaped for eval:\n%s", out.String())
	}
}

func TestConfigEnvMCPEmitsShellEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body string
		want []string
	}{
		{`{"gold_db": "/tmp/g.db", "default_currency": "USD", "silver_sources": [],
		   "mcp": {"enabled": true, "port": 3500, "auth": "none", "insecure": true}}`,
			[]string{"WEALTHDB_MCP_ENABLED=1", "WEALTHDB_MCP_PORT=3500", "WEALTHDB_MCP_CONFIG_AUTH=none",
				"WEALTHDB_MCP_CONFIG_INSECURE=1", "WEALTHDB_GOLD_DB='/tmp/g.db'"}},
		// An omitted block is a disabled server on the default port,
		// with the token.
		{`{"gold_db": "/tmp/g.db", "default_currency": "USD", "silver_sources": []}`,
			[]string{"WEALTHDB_MCP_ENABLED=0", "WEALTHDB_MCP_PORT=3300", "WEALTHDB_MCP_CONFIG_AUTH=token", "WEALTHDB_MCP_CONFIG_INSECURE=0"}},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run([]string{"-c", webTestCfg(t, c.body), "config-env", "mcp"}, nil, &out, &errb); code != 0 {
			t.Fatalf("exit %d, stderr=%s", code, errb.String())
		}
		for _, want := range c.want {
			if !strings.Contains(out.String(), want+"\n") {
				t.Errorf("missing %q in:\n%s", want, out.String())
			}
		}
	}
}

// TestConfigEnvMCPRefusesAuthNoneAlone: the emitter reads the config through
// its validation, so the lifecycle never sees a lone auth none.
func TestConfigEnvMCPRefusesAuthNoneAlone(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{"gold_db": "/tmp/g.db", "default_currency": "USD", "silver_sources": [],
		"mcp": {"enabled": true, "auth": "none"}}`)
	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "config-env", "mcp"}, nil, &out, &errb); code == 0 || !strings.Contains(errb.String(), "insecure") {
		t.Errorf("exit %d, stderr=%s; want a refusal naming insecure", code, errb.String())
	}
}
