package config

import (
	"strings"
	"testing"
)

func TestLoadParsesMCPBlock(t *testing.T) {
	path := writeConfig(t, `{
        "gold_db": "/tmp/x", "default_currency": "USD",
        "silver_sources": [],
        "mcp": {"enabled": true, "port": 3500}
    }`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MCP == nil || !c.MCP.Enabled || c.MCP.EffectivePort() != 3500 || c.MCP.EffectiveAuth() != MCPAuthToken {
		t.Errorf("mcp = %+v, want enabled on 3500 with token auth", c.MCP)
	}
}

// TestMCPDefaults pins what an omitted field means: the default port,
// and the token. A nil block reads the same way.
func TestMCPDefaults(t *testing.T) {
	for _, m := range []*MCPConfig{nil, {Enabled: true}} {
		if got := m.EffectivePort(); got != DefaultMCPPort {
			t.Errorf("%+v: port = %d, want %d", m, got, DefaultMCPPort)
		}
		if got := m.EffectiveAuth(); got != MCPAuthToken {
			t.Errorf("%+v: auth = %q, want %q", m, got, MCPAuthToken)
		}
	}
}

// TestValidateMCP holds the two refusals that keep a config from
// publishing the data by accident — auth none without its insecure
// acknowledgement, and an unknown mode — beside the port checks.
func TestValidateMCP(t *testing.T) {
	cases := []struct {
		name string
		web  *WebConfig
		mcp  MCPConfig
		want string // substring of the error; "" means valid
	}{
		{"defaults", nil, MCPConfig{Enabled: true}, ""},
		{"token named", nil, MCPConfig{Auth: "token"}, ""},
		{"none acknowledged", nil, MCPConfig{Auth: "none", Insecure: true}, ""},
		{"none alone", nil, MCPConfig{Auth: "none"}, `"insecure": true`},
		{"unknown mode", nil, MCPConfig{Auth: "oauth"}, "not a mode"},
		{"port out of range", nil, MCPConfig{Port: 70000}, "mcp.port"},
		{"port of web", &WebConfig{Port: 3300}, MCPConfig{}, "web server's port"},
		{"default port of web", &WebConfig{}, MCPConfig{Port: DefaultWebPort}, "web server's port"},
		{"beside web", &WebConfig{Port: 3000}, MCPConfig{Port: 3300}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.mcp
			c := &Config{GoldDB: "/x", DefaultCurrency: "USD", Web: tc.web, MCP: &m}
			err := c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("err = %v, want valid", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
