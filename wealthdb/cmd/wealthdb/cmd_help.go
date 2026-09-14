package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

func init() {
	register("help", cmdHelp)
}

// commandHelp describes one subcommand at the two grains the CLI
// prints it: `args` and `short` compose its line in the usage
// listing, and `long` is what `wealthdb help <name>` shows where the
// listing's one line cannot carry it. `hostSide` marks a command the
// `wealthdb` wrapper serves rather than this binary, so the listing
// can name it without the dispatch table holding an entry to match.
type commandHelp struct {
	name     string
	args     string
	short    string
	long     string
	hostSide bool
}

// usage is the command's left column: its name and argument shape.
func (c commandHelp) usage() string {
	if c.args == "" {
		return c.name
	}
	return c.name + " " + c.args
}

// detail is what `wealthdb help <name>` prints — the long form where
// one exists, and the listing's own line where it says enough.
func (c commandHelp) detail() string {
	if c.long != "" {
		return c.long
	}
	return c.short
}

// commandHelps is the one place a subcommand is described, and the
// usage listing is generated from it — there is no second copy to
// drift. The order is the one a reader meets the commands in (set up,
// load, query, enrich, inspect, then the meta commands) rather than
// the alphabetical order, which files a first-run wizard between two
// diagnostics.
//
// TestUsageListsEverySubcommand holds this against the dispatch
// table, so a subcommand that registers without an entry here fails
// the suite rather than going quietly unlisted.
var commandHelps = []commandHelp{
	{name: "config", short: "interactive first-time setup wizard",
		long: "Interactive first-time setup; writes the wealthdb.cfg file."},
	{name: "init", short: "initialise an empty gold DB at the configured gold_db path"},
	{name: "load", args: "<id> | -a", short: "merge new silver snapshots into gold"},
	{name: "reset", args: "<id> | -a", short: "purge a silver source's data from gold"},
	{name: "reload", args: "<id> | -a", short: "reset then load (use after upgrading wealthdb)",
		long: "Reset then load (one source, or -a for all; -a builds a fresh, compact gold file and swaps it in)."},
	{name: "compact", args: "[--dry-run]", short: "rewrite the gold DB into a fresh file to reclaim dead space",
		long: "Rewrite the gold DB into a fresh file to reclaim dead space (concurrent readers keep the old file until they close)."},

	{name: "holdings", args: "<view>", short: "point-in-time views: positions, accounts, portfolios, sources, global",
		long: "Point-in-time portfolio views: positions, accounts, portfolios, sources, global ('wealthdb holdings <view> -h')."},
	{name: "returns", args: "<view>", short: "TWR / MWR returns: accounts, portfolios, sources, global",
		long: "Time-weighted (TWR) & money-weighted (MWR/XIRR) returns by accounts, portfolios, sources, global ('wealthdb returns <view> -h')."},
	{name: "transactions", args: "[flags]", short: "print transactions over a date range (default past 30 days)",
		long: "Print transactions over a date range (-r reverses to newest-first)."},
	{name: "spending", args: "<view>", short: "spending reports: summary, categories, transactions",
		long: "What the tracked accounts spent: summary, categories, transactions ('wealthdb spending <view> -h')."},
	{name: "income", args: "<view>", short: "income reports: summary, types, transactions",
		long: "What the tracked accounts received: summary, types, transactions ('wealthdb income <view> -h')."},

	{name: "categorize", args: "[flags]", short: "categorise the merchants no deterministic tier placed",
		long: "Categorise the merchants the deterministic spending tiers left unplaced, via the LLM in config.spending.categorization.model."},
	{name: "categorizations", short: "list the stored merchant verdicts, or forget one",
		long: "Dump the spend_merchant_categories table (model-derived merchant verdicts); --forget SIG removes one."},
	{name: "resolve-symbols", short: "back-fill missing instrument ticker symbols",
		long: "Back-fill missing instrument ticker symbols via the LLM in config.symbol_resolution.model."},
	{name: "resolutions", args: "[-s <id>]", short: "list the resolved instrument ticker symbols",
		long: "Dump the symbol_resolutions table (LLM-derived + manual-override tickers)."},

	{name: "status", args: "[<id>] [-v]", short: "report gold state vs each silver source",
		long: "Report gold state vs each silver source (-v for taxonomy drift counts)."},
	{name: "snapshots", args: "<id> | -a", short: "list snapshots gold has loaded for a silver source",
		long: "List snapshots gold has loaded for a silver source (-a for all, --latest for newest only)."},

	{name: "web", args: "<verb>", hostSide: true,
		short: "manage the optional Metabase BI server (host-side wrapper)",
		long:  "Manage the optional Metabase BI server: start, stop, status, restart, refresh, logs. Served by the host-side `wealthdb` wrapper rather than this binary — see web/README.md."},
	{name: "version", short: "print the wealthdb version"},
	{name: "help", args: "[<subcommand>]", short: "help for a subcommand",
		long: "Show the top-level usage, or detailed help for a subcommand."},
}

// hostSideCommand reports whether name is a command the listing
// names but this binary does not serve, so the dispatcher can say
// which one runs it instead of calling it unknown.
func hostSideCommand(name string) (commandHelp, bool) {
	for _, c := range commandHelps {
		if c.name == name && c.hostSide {
			return c, true
		}
	}
	return commandHelp{}, false
}

// viewHelp blurbs the grains addressed as `wealthdb holdings <view>`.
// They are not subcommands, but `wealthdb help <view>` is what a
// reader tries first, so each resolves to its own line and a pointer
// at the path that runs it.
var viewHelp = map[string]string{
	"positions":  "Print consolidated positions as of a date (-f table|csv|csv_plain|json, -x CCY).",
	"accounts":   "Print one row per account with derived value aggregates (-x CCY, -d date, -C cols).",
	"portfolios": "Roll each portfolio's accounts into one row, + a per-source sentinel (-x CCY, -d date, -C cols).",
	"sources":    "Roll each silver source's accounts into one row (-x CCY, -d date, -C cols).",
	"global":     "Roll the whole portfolio into a single total row (-x CCY, -d date).",
}

// hiddenSubcommands are registered (so they're callable) but omitted
// from the usage listing — internal plumbing, not user-facing.
// `web-config` emits resolved web settings for the host-side
// `wealthdb web` wrapper; `web-materialize` rewrites the
// report_returns table before the wrapper snapshots gold.
var hiddenSubcommands = map[string]bool{
	"web-config":      true,
	"web-materialize": true,
}

func cmdHelp(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, _, stderr io.Writer) error {
	if len(subargs) == 0 {
		printGlobalUsage(stderr)
		return nil
	}
	name := subargs[0]
	// A holdings view is addressed as `wealthdb holdings <view>`;
	// point at that path rather than reporting it as unknown.
	if blurb, ok := viewHelp[name]; ok {
		fmt.Fprintf(stderr, "Use 'wealthdb holdings %s -h' for detailed flags.\n%s\n", name, blurb)
		return nil
	}
	for _, c := range commandHelps {
		if c.name != name {
			continue
		}
		if c.hostSide {
			fmt.Fprintf(stderr, "Use 'wealthdb %s help' for detailed usage.\n%s\n", name, c.detail())
			return nil
		}
		fmt.Fprintf(stderr, "Use 'wealthdb %s -h' for detailed flags.\n%s\n", name, c.detail())
		return nil
	}
	fmt.Fprintf(stderr, "wealthdb help: unknown subcommand %q\n\n", name)
	printGlobalUsage(stderr)
	return nil
}

// printGlobalUsage writes the top-level usage: the header and global
// flags, then one generated line per command. This is the only
// listing the CLI prints — `wealthdb`, `wealthdb --help`, `wealthdb
// help` and an unknown subcommand all land here.
func printGlobalUsage(w io.Writer) {
	fmt.Fprint(w, globalUsageHeader)

	// The left column is measured rather than fixed, so adding a
	// command with a longer name cannot silently break alignment.
	width := 0
	for _, c := range commandHelps {
		if n := len(c.usage()); n > width {
			width = n
		}
	}

	fmt.Fprintln(w, "subcommands:")
	for _, c := range commandHelps {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.usage(), c.short)
	}
	fmt.Fprint(w, globalUsageFooter)
}

const globalUsageHeader = `wealthdb — gold-layer portfolio CLI

usage:
  wealthdb [global flags] <subcommand> [subcommand flags]

global flags:
  -c, --config <path>   config file (default ${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg)
  -r, --read-only       force read-only access to the gold DB

`

const globalUsageFooter = `
Run 'wealthdb <subcommand> -h' for a subcommand's own flags, or
'wealthdb help <subcommand>' for what it does.
`

// usageString renders the listing to a string, for the callers that
// interpolate it into a message rather than writing it directly.
func usageString() string {
	var b strings.Builder
	printGlobalUsage(&b)
	return b.String()
}
