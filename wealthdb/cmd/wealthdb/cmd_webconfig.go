package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

func init() {
	register("web-config", cmdWebConfig)
}

// cmdWebConfig prints the resolved `web` settings as shell-evalable
// KEY=VALUE lines for the host-side `web/web` lifecycle script.
// Docker isn't reachable from inside the engine container, so the
// Metabase server is orchestrated on the host; the host reads these
// settings back through this subcommand — the one component that
// already parses (and validates) wealthdb.cfg. Read-only: it loads
// the config and opens nothing. Hidden from `wealthdb help`; it's
// plumbing for `wealthdb web`, not a user-facing command.
func cmdWebConfig(_ context.Context, g globalFlags, _ []string, _ io.Reader, stdout, _ io.Writer) error {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	enabled := "0"
	if cfg.Web != nil && cfg.Web.Enabled {
		enabled = "1"
	}
	port := config.DefaultWebPort
	if cfg.Web != nil && cfg.Web.Port != 0 {
		port = cfg.Web.Port
	}

	fmt.Fprintf(stdout, "WEALTHDB_WEB_ENABLED=%s\n", enabled)
	fmt.Fprintf(stdout, "WEALTHDB_WEB_PORT=%d\n", port)
	fmt.Fprintf(stdout, "WEALTHDB_GOLD_DB=%s\n", shellSingleQuote(cfg.GoldDB))
	// Passed to provision.py's --default-currency, which accepts it
	// for compatibility only and does not use it — the report models
	// expose per-currency (_<CCY>) column sets rather than binding a
	// single currency.
	fmt.Fprintf(stdout, "WEALTHDB_DEFAULT_CURRENCY=%s\n", shellSingleQuote(cfg.DefaultCurrency))
	return nil
}

// shellSingleQuote wraps s in single quotes for safe `eval` in a
// POSIX shell, escaping embedded single quotes the standard way
// (close the quote, add a backslash-escaped literal ', reopen).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
