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
	"init":      "Initialise an empty gold DB at the configured gold_db path.",
	"load":      "Merge new silver snapshots into gold (one source, or -a for all).",
	"positions": "Print consolidated positions as of a date (table format).",
	"help":      "Show this help, or detailed help for a subcommand.",
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
		desc, ok := helpText[n]
		if !ok {
			desc = "(no description)"
		}
		fmt.Fprintf(w, "  %-12s %s\n", n, desc)
	}
}
