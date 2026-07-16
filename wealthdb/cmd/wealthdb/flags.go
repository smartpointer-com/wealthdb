package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// globalFlags collects the parsed top-level (pre-subcommand)
// flags. Subcommand handlers consult these to know which config
// to load and whether to force read-only mode.
type globalFlags struct {
	ConfigPath    string
	ForceReadOnly bool
}

// defaultConfigPath returns wealthdb.cfg under the XDG config dir
// ($XDG_CONFIG_HOME, falling back to ~/.config), resolved at parse
// time so help text shows the concrete value.
func defaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "wealthdb.cfg")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No $HOME → fall back to the unexpanded literal. The
		// user will get a clear error on Load.
		return "$HOME/.config/wealthdb.cfg"
	}
	return filepath.Join(home, ".config", "wealthdb.cfg")
}

// parseGlobal extracts top-level flags from the leading slice of
// args. Returns the consumed flags, the remaining args (starting
// with the subcommand name), and a usage-writer error if parsing
// failed.
func parseGlobal(args []string, errOut io.Writer) (globalFlags, []string, error) {
	fs := flag.NewFlagSet("wealthdb", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprint(errOut, globalUsage)
	}

	g := globalFlags{ConfigPath: defaultConfigPath()}
	fs.StringVar(&g.ConfigPath, "c", g.ConfigPath, "path to wealthdb config file")
	fs.StringVar(&g.ConfigPath, "config", g.ConfigPath, "path to wealthdb config file")
	fs.BoolVar(&g.ForceReadOnly, "r", false, "force read-only access to the gold DB")
	fs.BoolVar(&g.ForceReadOnly, "read-only", false, "force read-only access to the gold DB")

	if err := fs.Parse(args); err != nil {
		return g, nil, err
	}
	return g, fs.Args(), nil
}

const globalUsage = `wealthdb — gold-layer portfolio CLI

usage:
  wealthdb [global flags] <subcommand> [subcommand flags]

global flags:
  -c, --config <path>   config file (default ${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg)
  -r, --read-only       force read-only access to the gold DB

subcommands:
  config                interactive first-time setup wizard
  init                  initialise an empty gold DB at the configured gold_db path
  load <id> | -a        merge new silver snapshots into gold
  reset <id> | -a       purge a silver source's data from gold
  reload <id> | -a      reset then load (use after upgrading wealthdb; -a builds a fresh, compact file)
  compact [--dry-run]   rewrite the gold DB into a fresh file to reclaim dead space
  holdings <view>       point-in-time portfolio views: positions, accounts, portfolios, sources, global
  returns <view>        TWR / MWR returns: accounts, portfolios, sources, global ('wealthdb returns <view> -h')
  transactions [flags]  print transactions over a date range (default past 30 days, oldest first)
  status [<id>] [-v]    report gold state vs each silver source
  snapshots <id> | -a   list snapshots gold has loaded for a silver
  web {start|stop|status}  manage the optional Metabase BI server (host-side; see web/README.md)
  help [<subcommand>]   help for a subcommand
`
