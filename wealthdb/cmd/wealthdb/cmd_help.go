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

// commandHelp describes one subcommand for the usage listing: `args`
// and `short` compose its line. `wealthdb help <name>` prints the
// command's own -h text, so the listing line is all a command served
// by this binary needs. `hostSide` marks a command the `wealthdb`
// wrapper serves instead, so the listing can name it without the
// dispatch table holding an entry to match; having no -h here, it
// carries a `long` description for `wealthdb help <name>`.
type commandHelp struct {
	name     string
	args     string
	short    string
	long     string
	hostSide bool
	// group is the heading the listing files the command under; the
	// headings and their order are helpGroups.
	group string
}

// usage is the command's left column: its name and argument shape.
func (c commandHelp) usage() string {
	if c.args == "" {
		return c.name
	}
	return c.name + " " + c.args
}

// detail is a host-side command's description — the long form where
// one exists, and the listing's own line where it says enough.
func (c commandHelp) detail() string {
	if c.long != "" {
		return c.long
	}
	return c.short
}

// helpGroups are the listing's headings, in print order. A reader of
// the help is far more often asking a question than loading data, so
// the read-only reports come first and the write commands follow,
// labelled as such.
var helpGroups = []struct{ key, title string }{
	{"report", "reports (read-only):"},
	{"load", "set up and load (write):"},
	{"enrich", "enrich (write; ask the configured model):"},
	{"other", "other:"},
}

// commandHelps is the one place a subcommand is described, and the
// usage listing is generated from it — there is no second copy to
// drift. Within a group the order is the one a reader meets the
// commands in, not the alphabetical order.
//
// TestUsageListsEverySubcommand holds this against the dispatch
// table, so a subcommand that registers without an entry here fails
// the suite rather than going quietly unlisted.
var commandHelps = []commandHelp{
	{name: "holdings", args: "<view>", group: "report",
		short: "what is held and what it is worth, as of a date: global, sources, portfolios, accounts, positions"},
	{name: "returns", args: "<view>", group: "report",
		short: "how it performed over a window, as TWR / MWR %: global, sources, portfolios, accounts"},
	{name: "gains", args: "<view>", group: "report",
		short: "what was gained or lost (P&L), realized and unrealized, over a window: summary, sources, portfolios, accounts, positions, realized, lots, coverage"},
	{name: "transactions", args: "[FROM [TO]]", group: "report",
		short: "the individual booked lines over a window; totals by kind are the three reports below"},
	{name: "spending", args: "<view>", group: "report",
		short: "what was spent, on what, as totals by category: summary, categories, transactions"},
	{name: "income", args: "<view>", group: "report",
		short: "what was received (salary, dividends, interest, rent), as totals by type: summary, types, transactions"},
	{name: "cashflow", args: "<view>", group: "report",
		short: "where the cash came from and went, mortgage and plan contributions included: summary, flows, sankey, transactions, coverage"},
	{name: "status", args: "[<id>] [-v]", group: "report",
		short: "is the data current, and what each source holds"},
	{name: "snapshots", args: "<id> | -a", group: "report",
		short: "the dates a source has data for"},

	{name: "config", group: "load", short: "interactive first-time setup wizard; writes wealthdb.cfg"},
	{name: "init", group: "load", short: "create the empty database at the configured path"},
	{name: "load", args: "<id> | -a", group: "load", short: "load a source's new data (or every source's) into the database"},
	{name: "reset", args: "<id> | -a", group: "load", short: "remove a source's data from the database"},
	{name: "reload", args: "<id> | -a", group: "load", short: "reset then load (use after upgrading wealthdb)"},
	{name: "compact", args: "[--dry-run]", group: "load", short: "rewrite the database file to reclaim dead space"},

	{name: "categorize", args: "[spending|income]", group: "enrich",
		short: "categorise the merchants and payers no rule could place"},
	{name: "categorizations", args: "[spending|income]", group: "enrich",
		short: "list the stored merchant and payer verdicts, or forget one"},
	{name: "resolve-symbols", group: "enrich", short: "back-fill missing instrument ticker symbols"},
	{name: "resolutions", args: "[-s <id>]", group: "enrich", short: "list the resolved instrument ticker symbols"},

	{name: "web", args: "<verb>", hostSide: true, group: "other",
		short: "manage the optional Metabase dashboards (host-side wrapper)",
		long:  "Manage the optional Metabase dashboards: start, stop, status, restart, refresh, logs. Served by the host-side `wealthdb` wrapper rather than this binary — see web/README.md."},
	{name: "mcp", args: "<verb>", hostSide: true, group: "other",
		short: "manage the optional MCP server that serves the reports to AI agents (host-side wrapper)",
		long:  "Manage the optional MCP server that serves the read-only reports to AI agents: start, stop, status, restart, logs, stdio, url. Served by the host-side `wealthdb` wrapper rather than this binary — see mcp/README.md."},
	{name: "mcp-serve", args: "--stdio | --http ADDR", group: "other",
		short: "the MCP server itself; 'wealthdb mcp' runs and manages it"},
	{name: "version", group: "other", short: "print the wealthdb version"},
	{name: "help", args: "[<subcommand>]", group: "other", short: "this listing, or a subcommand's full help"},
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

// hiddenSubcommands are registered (so they're callable) but omitted
// from the usage listing — internal plumbing, not user-facing.
// `web-config` and `mcp-config` emit resolved settings for the
// host-side `wealthdb web` and `wealthdb mcp` wrappers;
// `web-materialize` rewrites the report_returns table before the
// wrapper snapshots gold.
var hiddenSubcommands = map[string]bool{
	"web-config":      true,
	"web-materialize": true,
	"mcp-config":      true,
}

func cmdHelp(ctx context.Context, g globalFlags, subargs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		printGlobalUsage(stderr)
		return nil
	}
	name := subargs[0]
	// A holdings view is addressed as `wealthdb holdings <view>`; its
	// help is the view's own, reached the way the command reaches it.
	if _, ok := holdingsViews[name]; ok {
		return cmdHoldings(ctx, g, []string{name, "-h"}, stdin, stdout, stderr)
	}
	for _, c := range commandHelps {
		if c.name != name {
			continue
		}
		switch {
		case c.hostSide:
			fmt.Fprintf(stderr, "Use 'wealthdb %s help' for detailed usage.\n%s\n", name, c.detail())
		case name == "help":
			printGlobalUsage(stderr)
		case name == "version":
			fmt.Fprintf(stderr, "wealthdb version — %s\n", c.detail())
		default:
			// The command's own -h is its full help; printing it here
			// keeps one text for the two ways of asking.
			return subcommands[name](ctx, g, []string{"-h"}, stdin, stdout, stderr)
		}
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

	for _, grp := range helpGroups {
		fmt.Fprintln(w, grp.title)
		for _, c := range commandHelps {
			if c.group == grp.key {
				fmt.Fprintf(w, "  %-*s  %s\n", width, c.usage(), c.short)
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, globalUsageFooter)
}

const globalUsageHeader = `wealthdb — a household's complete financial picture, from one local database

The database merges every collected account: banks, cards, brokerages,
pensions and crypto, plus property, loans and private holdings recorded
by hand. Every report reads it; nothing below changes an account.

usage:
  wealthdb [global flags] <subcommand> [subcommand flags]

global flags:
  -c, --config <path>   config file (default ${XDG_CONFIG_HOME:-~/.config}/wealthdb.cfg)
  -r, --read-only       force read-only access to the database

`

const globalUsageFooter = `Windows are positional: 2025 is a year, 2025-06 a month, 2025-06-15 a
day, FROM TO a range ('-' leaves an end open); holdings take an as-of
date (-d) instead. One figure for a whole window: --period total.
Every report prints a table; -f json for machines, -x CCY for another
currency, -C for columns, -p to redact amounts. There are no row
filters: pick the view, then filter the output.

Run 'wealthdb help <subcommand>' or 'wealthdb <subcommand> -h' for its full help.
`

// usageString renders the listing to a string, for the callers that
// interpolate it into a message rather than writing it directly.
func usageString() string {
	var b strings.Builder
	printGlobalUsage(&b)
	return b.String()
}
