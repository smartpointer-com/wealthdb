package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMCPConfigEmitsShellEnv(t *testing.T) {
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
		if code := Run([]string{"-c", webTestCfg(t, c.body), "mcp-config"}, nil, &out, &errb); code != 0 {
			t.Fatalf("exit %d, stderr=%s", code, errb.String())
		}
		for _, want := range c.want {
			if !strings.Contains(out.String(), want+"\n") {
				t.Errorf("missing %q in:\n%s", want, out.String())
			}
		}
	}
}

// TestMCPConfigRefusesAuthNoneAlone: the emitter reads the config through
// its validation, so the lifecycle never sees a lone auth none.
func TestMCPConfigRefusesAuthNoneAlone(t *testing.T) {
	t.Parallel()
	cfg := webTestCfg(t, `{"gold_db": "/tmp/g.db", "default_currency": "USD", "silver_sources": [],
		"mcp": {"enabled": true, "auth": "none"}}`)
	var out, errb bytes.Buffer
	if code := Run([]string{"-c", cfg, "mcp-config"}, nil, &out, &errb); code == 0 || !strings.Contains(errb.String(), "insecure") {
		t.Errorf("exit %d, stderr=%s; want a refusal naming insecure", code, errb.String())
	}
}
