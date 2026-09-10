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
	"config":       "Interactive first-time setup; writes the wealthdb.cfg file.",
	"init":         "Initialise an empty gold DB at the configured gold_db path.",
	"load":         "Merge new silver snapshots into gold (one source, or -a for all).",
	"reset":        "Purge a silver source's data from gold (one source, or -a for all).",
	"reload":       "Reset then load (one source, or -a for all; -a builds a fresh, compact gold file and swaps it in).",
	"compact":      "Rewrite the gold DB into a fresh file to reclaim dead space (concurrent readers keep the old file until they close).",
	"holdings":     "Point-in-time portfolio views: positions, accounts, portfolios, sources, global ('wealthdb holdings <view> -h').",
	"returns":      "Time-weighted (TWR) & money-weighted (MWR/XIRR) returns by accounts, portfolios, sources, global ('wealthdb returns <view> -h').",
	"transactions": "Print transactions over a date range (-r reverses to newest-first).",
	"spending":     "What the tracked accounts spent: summary, categories, transactions ('wealthdb spending <view> -h').",
	// The holdings views — addressed as `wealthdb holdings <view>`, but kept
	// here so `wealthdb help <view>` still resolves to a useful blurb.
	"positions":       "Print consolidated positions as of a date (-f table|csv|csv_plain|json, -x CCY).",
	"accounts":        "Print one row per account with derived value aggregates (-x CCY, -d date, -C cols).",
	"portfolios":      "Roll each portfolio's accounts into one row, + a per-source sentinel (-x CCY, -d date, -C cols).",
	"sources":         "Roll each silver source's accounts into one row (-x CCY, -d date, -C cols).",
	"global":          "Roll the whole portfolio into a single total row (-x CCY, -d date).",
	"status":          "Report gold state vs each silver source (-v for taxonomy drift counts).",
	"snapshots":       "List snapshots gold has loaded for a silver source (-a for all, --latest for newest only).",
	"resolve-symbols": "Back-fill missing instrument ticker symbols via the LLM in config.symbol_resolution.model.",
	"resolutions":     "Dump the symbol_resolutions table (LLM-derived + manual-override tickers).",
	"categorize":      "Categorise the merchants the deterministic spending tiers left unplaced, via the LLM in config.spending.categorization.model.",
	"categorizations": "Dump the spend_merchant_categories table (model-derived merchant verdicts); --forget SIG removes one.",
	"version":         "Print the wealthdb version.",
	"help":            "Show this help, or detailed help for a subcommand.",
}

// hiddenSubcommands are registered (so they're callable) but omitted
// from the help listing — internal plumbing, not user-facing.
// `web-config` emits resolved web settings for the host-side
// `wealthdb web` wrapper; `web-materialize` rewrites the
// report_returns table before the wrapper snapshots gold.
var hiddenSubcommands = map[string]bool{
	"web-config":      true,
	"web-materialize": true,
}

func cmdHelp(_ context.Context, _ globalFlags, subargs []string, _ io.Reader, _, stderr io.Writer) error {
	if len(subargs) == 0 {
		printGlobalHelp(stderr)
		return nil
	}
	name := subargs[0]
	// Holdings views are addressed as `wealthdb holdings <view>`; point
	// at that path rather than reporting them as unknown.
	if _, ok := holdingsViews[name]; ok {
		fmt.Fprintf(stderr, "Use 'wealthdb holdings %s -h' for detailed flags.\n%s\n", name, helpText[name])
		return nil
	}
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
