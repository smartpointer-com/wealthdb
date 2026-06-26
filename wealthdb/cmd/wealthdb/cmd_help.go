package main

import (
	"context"
	"fmt"
	"io"
	"sort"
)

func init() {
	register("help", cmdHelp)
}

// helpText is the per-subcommand short help shown by `wealthdb
// help <subcommand>`. Subcommand handlers register their longer
// usage via flag.Usage; this map is for the brief overview only.
var helpText = map[string]string{
	"config":          "Interactive first-time setup; writes the wealthdb.cfg file.",
	"init":            "Initialise an empty gold DB at the configured gold_db path.",
	"load":            "Merge new silver snapshots into gold (one source, or -a for all).",
	"reset":           "Purge a silver source's data from gold (one source, or -a for all).",
	"positions":       "Print consolidated positions as of a date (-f table|csv|csv_plain|json, -x CCY).",
	"transactions":    "Print transactions over a date range (-r reverses to newest-first).",
	"accounts":        "Print one row per account with derived value aggregates (-x CCY, -d date, -C cols).",
	"portfolios":      "Roll each portfolio's accounts into one row, + a per-source sentinel (-x CCY, -d date, -C cols).",
	"sources":         "Roll each silver source's accounts into one row (-x CCY, -d date, -C cols).",
	"global":          "Roll the whole portfolio into a single total row (-x CCY, -d date).",
	"status":          "Report gold state vs each silver source (-v for taxonomy drift counts).",
	"snapshots":       "List snapshots gold has loaded for a silver source (-a for all).",
	"resolve-symbols": "Back-fill missing instrument ticker symbols via the LLM in config.symbol_resolution.model.",
	"resolutions":     "Dump the symbol_resolutions table (LLM-derived + manual-override tickers).",
	"help":            "Show this help, or detailed help for a subcommand.",
}

// hiddenSubcommands are registered (so they're callable) but omitted
// from the help listing — internal plumbing, not user-facing.
// `web-config` emits resolved web settings for the host-side
// `wealthdb web` wrapper.
var hiddenSubcommands = map[string]bool{
	"web-config": true,
}

func cmdHelp(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, _, stderr io.Writer) error {
	if len(subargs) == 0 {
		printGlobalHelp(stderr)
		return nil
	}
	name := subargs[0]
	if _, ok := subcommands[name]; !ok {
		fmt.Fprintf(stderr, "wealthdb help: unknown subcommand %q\n\n", name)
		printGlobalHelp(stderr)
		return nil
	}
	fmt.Fprintf(stderr, "Use 'wealthdb %s -h' for detailed flags.\n%s\n", name, helpText[name])
	return nil
}

func printGlobalHelp(w io.Writer) {
	fmt.Fprint(w, globalUsage)

	names := make([]string, 0, len(subcommands))
	for n := range subcommands {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Fprintln(w, "Registered subcommands:")
	for _, n := range names {
		if hiddenSubcommands[n] {
			continue
		}
		desc, ok := helpText[n]
		if !ok {
			desc = "(no description)"
		}
		fmt.Fprintf(w, "  %-12s %s\n", n, desc)
	}
}
