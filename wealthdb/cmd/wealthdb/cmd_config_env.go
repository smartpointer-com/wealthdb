package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

func init() {
	register("config-env", cmdConfigEnv)
}

// configEnvBlocks are the blocks config-env prints, by name. Each writes
// the resolved settings of one part of wealthdb.cfg as shell-evalable
// KEY=VALUE lines. `collectors` is the exception: one name per line, no
// config read.
var configEnvBlocks = map[string]func(cfg *config.Config, stdout io.Writer){
	"web":     webEnv,
	"mcp":     mcpEnv,
	"sources": sourcesEnv,
	"refresh": refreshEnv,
	// collectors reads no config: it is the catalogue.
	"collectors": nil,
}

// cmdConfigEnv prints one block of the resolved config as shell-evalable
// KEY=VALUE lines, for the host-side scripts: Docker is not reachable
// from inside the engine container, so the web and mcp servers are
// orchestrated on the host, and the host reads the settings back through
// the engine, the one component that parses and validates wealthdb.cfg.
// Read-only: it loads the config and opens nothing. `collectors` is the
// exception: one name per line, no config read. Hidden from
// `wealthdb help`; it is plumbing, not a user-facing command.
func cmdConfigEnv(_ context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	names := strings.Join(slices.Sorted(maps.Keys(configEnvBlocks)), " | ")
	fs := flag.NewFlagSet("wealthdb config-env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb config-env <block>

Print one block of wealthdb.cfg, resolved, as shell KEY=VALUE lines for
a host script to eval. The blocks:
  web      the web server, the gold path, the default currency and the
           default reading of a missing cost basis
  mcp      the MCP server and the gold path
  sources  one line per silver file: the source's id and kind, the
           subsource's kind (empty for a source of one file) and the path
  refresh  one line per collector whose silver the config reads, in
           config order: its name, the verb that starts it, and the
           logins of a collector with one silver per login
  collectors
           the name of each collector the setup offers, one per line;
           reads no config`)
	}
	if err := parseFlags(fs, subargs, "config-env"); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "config-env: name one block (%s)", names)
	}
	emit, ok := configEnvBlocks[fs.Arg(0)]
	if !ok {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "config-env: unknown block %q (want %s)", fs.Arg(0), names)
	}
	if emit == nil {
		for _, c := range silver.Catalogue() {
			fmt.Fprintln(stdout, c.Name)
		}
		return nil
	}
	return printConfigEnv(g, stdout, emit)
}

// printConfigEnv loads the config, through its validation, and prints
// one block.
func printConfigEnv(g globalFlags, stdout io.Writer, emit func(*config.Config, io.Writer)) error {
	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	emit(cfg, stdout)
	return nil
}

// webEnv is the `web` block, for web/web.
func webEnv(cfg *config.Config, stdout io.Writer) {
	enabled := "0"
	if cfg.Web != nil && cfg.Web.Enabled {
		enabled = "1"
	}
	fmt.Fprintf(stdout, "WEALTHDB_WEB_ENABLED=%s\n", enabled)
	fmt.Fprintf(stdout, "WEALTHDB_WEB_PORT=%d\n", cfg.Web.EffectivePort())
	fmt.Fprintf(stdout, "WEALTHDB_GOLD_DB=%s\n", shellSingleQuote(cfg.GoldDB))
	// Passed to provision.py's --default-currency: the currency every
	// dashboard's Currency picker opens on, when it is one of the
	// reporting currencies.
	fmt.Fprintf(stdout, "WEALTHDB_DEFAULT_CURRENCY=%s\n", shellSingleQuote(cfg.DefaultCurrency))
	// Passed to provision.py's --missing-basis: the reading the Gains
	// dashboard's Missing cost basis picker opens on.
	fmt.Fprintf(stdout, "WEALTHDB_MISSING_BASIS=%s\n", shellSingleQuote(string(cfg.MissingBasis())))
}

// mcpEnv is the `mcp` block, for mcp/mcp.
func mcpEnv(cfg *config.Config, stdout io.Writer) {
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
}

// sourcesEnv is the silver files, one line per file in config order,
// each line its own set of assignments, so a script evals one line at a
// time.
func sourcesEnv(cfg *config.Config, stdout io.Writer) {
	line := func(s config.SilverSource, kind, path string) {
		fmt.Fprintf(stdout, "WEALTHDB_SOURCE_ID=%s WEALTHDB_SOURCE_KIND=%s WEALTHDB_SILVER_KIND=%s WEALTHDB_SILVER_PATH=%s\n",
			shellSingleQuote(s.ID), shellSingleQuote(s.Kind), shellSingleQuote(kind), shellSingleQuote(path))
	}
	for _, s := range cfg.SilverSources {
		if s.Path != "" {
			line(s, "", s.Path)
		}
		for _, sub := range s.Subsources {
			line(s, sub.Kind, sub.Path)
		}
	}
}

// refreshEnv prints, for `wealthdb refresh`, one line per collector whose
// silver the config reads, the first time the config names it: the
// collector, the start the catalogue states for it (`login` when it
// states none) and, for a collector with one silver per linked login,
// those logins, a blank apart. A source whose kind no collector writes
// (a file edited by hand) is left out.
func refreshEnv(cfg *config.Config, stdout io.Writer) {
	var order []string
	items := map[string][]string{}
	for _, s := range cfg.SilverSources {
		for _, f := range s.Feeds() {
			if f.Collector == "" {
				continue
			}
			if _, seen := items[f.Collector]; !seen {
				order = append(order, f.Collector)
				items[f.Collector] = nil
			}
			if f.Item != "" && !slices.Contains(items[f.Collector], f.Item) {
				items[f.Collector] = append(items[f.Collector], f.Item)
			}
		}
	}
	for _, name := range order {
		c, _ := silver.CollectorNamed(name)
		fmt.Fprintf(stdout, "WEALTHDB_COLLECTOR=%s WEALTHDB_START=%s WEALTHDB_ITEMS=%s\n",
			shellSingleQuote(name), shellSingleQuote(cmp.Or(c.Start, "login")),
			shellSingleQuote(strings.Join(items[name], " ")))
	}
}

// shellSingleQuote wraps s in single quotes for safe `eval` in a
// POSIX shell, escaping embedded single quotes the standard way
// (close the quote, add a backslash-escaped literal ', reopen).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
