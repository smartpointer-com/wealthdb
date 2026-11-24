package config

import (
	"fmt"
)

// DefaultWebPort is the host loopback port the Metabase server is
// published on when `web.port` is omitted. 3000 is Metabase's own
// default.
const DefaultWebPort = 3000

// WebConfig is the optional `web` block of wealthdb.cfg, driving the
// dockerized Metabase server managed by `wealthdb web`. The lifecycle
// runs host-side; the host wrapper reads these settings back as shell
// lines through `wealthdb config-env web`, the engine being the one
// component that parses this JSON. Keep the lines in sync with web/web.
type WebConfig struct {
	// Enabled gates `wealthdb web start`. False (or an omitted block)
	// means the server is not configured.
	Enabled bool `json:"enabled"`
	// Port is the loopback port Metabase is published on
	// (127.0.0.1:Port and [::1]:Port → container :3000). Zero/omitted
	// → DefaultWebPort.
	Port int `json:"port,omitempty"`
}

// EffectivePort is the configured port, or DefaultWebPort.
func (w *WebConfig) EffectivePort() int {
	if w == nil || w.Port == 0 {
		return DefaultWebPort
	}
	return w.Port
}

// DefaultMCPPort is the host loopback port the MCP server is published
// on when `mcp.port` is omitted.
const DefaultMCPPort = 3300

// MCP auth modes. MCPAuthToken requires a bearer token on every HTTP
// request; MCPAuthNone serves any local process and is honoured only
// with Insecure set as well.
const (
	MCPAuthToken = "token"
	MCPAuthNone  = "none"
)

// MCPConfig is the optional `mcp` block of wealthdb.cfg, driving the MCP
// server managed by `wealthdb mcp`. The lifecycle runs host-side; the
// host wrapper reads these settings back as shell lines through
// `wealthdb config-env mcp`. Keep the lines in sync with mcp/mcp.
type MCPConfig struct {
	// Enabled gates `wealthdb mcp start` and `wealthdb mcp stdio`.
	Enabled bool `json:"enabled"`
	// Port is the loopback port the server is published on. Zero or
	// omitted means DefaultMCPPort.
	Port int `json:"port,omitempty"`
	// Auth is MCPAuthToken (the default when empty) or MCPAuthNone.
	Auth string `json:"auth,omitempty"`
	// Insecure acknowledges Auth none. Without it, validation refuses
	// a config that turns the token off: the data is a household's
	// finances, and one mistyped field must not publish them.
	Insecure bool `json:"insecure,omitempty"`
}

// EffectivePort is the configured port, or DefaultMCPPort.
func (m *MCPConfig) EffectivePort() int {
	if m == nil || m.Port == 0 {
		return DefaultMCPPort
	}
	return m.Port
}

// EffectiveAuth is the configured auth mode, or MCPAuthToken.
func (m *MCPConfig) EffectiveAuth() string {
	if m == nil || m.Auth == "" {
		return MCPAuthToken
	}
	return m.Auth
}

// SymbolResolutionConfig is the `symbol_resolution` block of
// wealthdb.cfg. Groups everything specific to the resolve-symbols
// subcommand so the top-level config file doesn't sprout one
// field per concern.
type SymbolResolutionConfig struct {
	// Model configures the LLM endpoint used to back-fill missing
	// instrument tickers. Required for normal `resolve-symbols`
	// runs; can be omitted when only --overrides-only is used.
	Model *ModelConfig `json:"model,omitempty"`
	// Overrides are the configured ticker-mapping overrides.
	// Each entry replaces (or suppresses) a row in
	// symbol_resolutions under model_name='manual-override'.
	// Applied at the start of every `wealthdb resolve-symbols`
	// invocation (including --overrides-only). Use cases:
	// correcting an LLM resolution that was wrong, or seeding
	// tickers the LLM can't infer (e.g. private-fund proxies).
	Overrides []SymbolOverride `json:"overrides,omitempty"`
}

// SymbolOverride is one entry under `symbol_resolution.overrides`.
// Mirrors the symbol_resolutions PK + value columns. Two modes:
//
//   - Correction: set `symbol` to the right ticker. The sync UPSERTs
//     this row into symbol_resolutions, winning over any LLM result.
//   - Suppression: set `delete: true` (and omit `symbol`). The sync
//     DELETEs any row with this PK from symbol_resolutions. Use for
//     descriptions where no real ticker exists (US Treasury CUSIPs,
//     private structured products, currency-line placeholders) so
//     the LLM's wrong guess stops surfacing.
type SymbolOverride struct {
	SilverSourceID string `json:"silver_source_id"`
	LookupKind     string `json:"lookup_kind"` // 'instrument_external_id' or 'name'
	LookupValue    string `json:"lookup_value"`
	Symbol         string `json:"symbol,omitempty"`
	Delete         bool   `json:"delete,omitempty"`
}

// ModelConfig is the `symbol_resolution.model` block of
// wealthdb.cfg, and the identically shaped
// `spending.categorization.model` block. The only API shape
// supported today is the OpenAI-compatible Chat Completions
// endpoint (`api: "openai-completions"`). Stripping R1-style
// `<think>` blocks off a response is unconditional hygiene in
// both pipelines, not something a field turns on.
type ModelConfig struct {
	BaseURL string `json:"baseUrl"`
	API     string `json:"api"`
	APIKey  string `json:"apiKey,omitempty"`
	Name    string `json:"name"`
	// ThinkingFormat is accepted and ignored so an older config
	// still parses; `<think>` stripping runs for every model.
	ThinkingFormat string `json:"thinkingFormat,omitempty"`
}

// Check says what a command that asks the model would refuse in m:
// a missing baseUrl or name, or an api other than the one supported.
// key is the member that holds the block, which every message names.
func (m *ModelConfig) Check(key string) error {
	switch {
	case m.BaseURL == "":
		return fmt.Errorf("%s.baseUrl is required", key)
	case m.Name == "":
		return fmt.Errorf("%s.name is required", key)
	case m.API != "" && m.API != "openai-completions":
		return fmt.Errorf("%s.api %q not supported; only 'openai-completions' is wired today", key, m.API)
	}
	return nil
}

// validateSymbolResolution checks symbol_resolution.overrides: every source
// must be declared, every kind must be one of the two discriminator values
// used by the symbol_resolutions table, every lookup_value must be
// non-empty, every symbol must look ticker-shaped (unless the entry is a
// `delete: true` suppression). Also reject duplicate (source, kind, value)
// tuples, so the UPSERT loop never resolves a duplicate by last write.
func (c *Config) validateSymbolResolution(seenIDs map[string]bool) error {
	if c.SymbolResolution != nil {
		seenOverrideKey := map[string]bool{}
		for i, o := range c.SymbolResolution.Overrides {
			if !seenIDs[o.SilverSourceID] {
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: no silver_sources[].id matches %q", i, o.SilverSourceID)
			}
			if o.LookupKind != "instrument_external_id" && o.LookupKind != "name" {
				return fmt.Errorf("config: symbol_resolution.overrides[%d].lookup_kind %q must be 'instrument_external_id' or 'name'", i, o.LookupKind)
			}
			if o.LookupValue == "" {
				return fmt.Errorf("config: symbol_resolution.overrides[%d].lookup_value is required", i)
			}
			switch {
			case o.Delete && o.Symbol != "":
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: `delete: true` is mutually exclusive with `symbol`", i)
			case o.Delete:
				// Deletion entry — nothing else to validate.
			default:
				if !TickerShape.MatchString(o.Symbol) {
					return fmt.Errorf("config: symbol_resolution.overrides[%d].symbol %q must be 1-12 chars of uppercase letters/digits/dots/hyphens", i, o.Symbol)
				}
			}
			k := o.SilverSourceID + "\x00" + o.LookupKind + "\x00" + o.LookupValue
			if seenOverrideKey[k] {
				return fmt.Errorf("config: symbol_resolution.overrides[%d]: duplicate (silver_source_id, lookup_kind, lookup_value) tuple", i)
			}
			seenOverrideKey[k] = true
		}
	}
	return nil
}

// validateWeb checks web: optional dockerized BI server. Only the port needs
// a shape check; an absent block or zero port means "use the default"
// (DefaultWebPort), resolved at read time.
func (c *Config) validateWeb() error {
	if c.Web != nil && c.Web.Port != 0 && (c.Web.Port < 1 || c.Web.Port > 65535) {
		return fmt.Errorf("config: web.port %d is out of range (1-65535)", c.Web.Port)
	}
	return nil
}

// validateMCP checks mcp: the optional MCP server. The port must be in
// range and must not collide with the web server's; turning the token
// off takes two settings, auth none and insecure, so that neither one
// alone can expose the data.
func (c *Config) validateMCP() error {
	m := c.MCP
	if m == nil {
		return nil
	}
	if m.Port != 0 && (m.Port < 1 || m.Port > 65535) {
		return fmt.Errorf("config: mcp.port %d is out of range (1-65535)", m.Port)
	}
	if c.Web != nil {
		if webPort := c.Web.EffectivePort(); m.EffectivePort() == webPort {
			return fmt.Errorf("config: mcp.port %d is also the web server's port; pick another", webPort)
		}
	}
	switch m.Auth {
	case "", MCPAuthToken:
	case MCPAuthNone:
		if !m.Insecure {
			return fmt.Errorf(`config: mcp.auth "none" serves the data to any process on this machine; ` +
				`it needs "insecure": true beside it to take effect`)
		}
	default:
		return fmt.Errorf("config: mcp.auth %q is not a mode (want token | none)", m.Auth)
	}
	return nil
}
