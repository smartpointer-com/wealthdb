package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func webTestCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wealthdb.cfg")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWebConfigEmitsShellEnv(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{
		"gold_db": "/Users/Shared/x/wealthdb.db",
		"default_currency": "GBP",
		"silver_sources": [],
		"web": {"enabled": true, "port": 4444},
		"lots": {"missing_basis": "zero"}
	}`)

	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "web-config"}, nil, &out, &errb); code != 0 {
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

func TestWebConfigDefaultsWhenOmitted(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{
		"gold_db": "/tmp/g.db",
		"default_currency": "USD",
		"silver_sources": []
	}`)

	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "web-config"}, nil, &out, &errb); code != 0 {
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

func TestWebConfigQuotesGoldPath(t *testing.T) {
	t.Parallel()
	// A path containing a single quote must survive `eval` in the
	// host wrapper.
	cfg := webTestCfg(t, `{
		"gold_db": "/tmp/o'brien/wealthdb.db",
		"default_currency": "USD",
		"silver_sources": []
	}`)
	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "web-config"}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), `WEALTHDB_GOLD_DB='/tmp/o'\''brien/wealthdb.db'`) {
		t.Errorf("single-quote not escaped for eval:\n%s", out.String())
	}
}
