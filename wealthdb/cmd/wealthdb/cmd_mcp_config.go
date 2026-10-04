package main

import (
	"context"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

func init() {
	register("mcp-config", cmdMCPConfig)
}

// cmdMCPConfig prints the resolved `mcp` settings as shell-evalable
// KEY=VALUE lines for the host-side `mcp/mcp` lifecycle script, the
// twin of web-config: the engine is the one component that parses and
// validates wealthdb.cfg, so the host reads the settings back through
// it. Read-only: it loads the config and opens nothing. Hidden from
// `wealthdb help`.
func cmdMCPConfig(_ context.Context, g globalFlags, _ []string, _ io.Reader, stdout, _ io.Writer) error {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	enabled, insecure := "0", "0"
	if cfg.MCP != nil && cfg.MCP.Enabled {
		enabled = "1"
	}
	if cfg.MCP != nil && cfg.MCP.Insecure {
		insecure = "1"
	}
	fmt.Fprintf(stdout, "WEALTHDB_MCP_ENABLED=%s\n", enabled)
	fmt.Fprintf(stdout, "WEALTHDB_MCP_PORT=%d\n", cfg.MCP.EffectivePort())
	// Named apart from WEALTHDB_MCP_AUTH and WEALTHDB_MCP_INSECURE, the
	// env forms that override them: evaluating these lines must not
	// overwrite an override.
	fmt.Fprintf(stdout, "WEALTHDB_MCP_CONFIG_AUTH=%s\n", cfg.MCP.EffectiveAuth())
	fmt.Fprintf(stdout, "WEALTHDB_MCP_CONFIG_INSECURE=%s\n", insecure)
	fmt.Fprintf(stdout, "WEALTHDB_GOLD_DB=%s\n", shellSingleQuote(cfg.GoldDB))
	return nil
}
