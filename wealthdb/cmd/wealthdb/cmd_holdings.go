package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
)

func init() {
	register("holdings", cmdHoldings)
}

// holdingsViews are the point-in-time portfolio views, grouped under
// `wealthdb holdings` so the top-level command surface stays small as
// more report families land (e.g. the sibling `wealthdb returns`). They
// share one flag idiom (-d / -f / -x / --fx-mode / -p, plus -C on all
// but global) and their totals reconcile: global == Σ sources ==
// Σ portfolios == Σ accounts == positions --with-cash.
var holdingsViews = map[string]subcommandHandler{
	"positions":  cmdPositions,
	"accounts":   cmdAccounts,
	"portfolios": cmdPortfolios,
	"sources":    cmdSources,
	"global":     cmdGlobal,
}

// cmdHoldings routes `wealthdb holdings <view> ...` to the matching
// view handler, passing the post-view args through unchanged so each
// view parses its own flags exactly as it did when top-level.
func cmdHoldings(ctx context.Context, g globalFlags, subargs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 {
		fmt.Fprint(stderr, holdingsUsage)
		return errs.Newf(2, "holdings: a view subcommand is required")
	}
	view, rest := subargs[0], subargs[1:]
	switch view {
	case "-h", "--help", "help":
		fmt.Fprint(stderr, holdingsUsage)
		return nil
	}
	h, ok := holdingsViews[view]
	if !ok {
		fmt.Fprint(stderr, holdingsUsage)
		return errs.Newf(2, "holdings: unknown view %q", view)
	}
	return h(ctx, g, rest, stdin, stdout, stderr)
}

const holdingsUsage = `wealthdb holdings — point-in-time portfolio views

usage:
  wealthdb holdings <view> [flags]

views (coarsest → finest aggregation):
  global       roll the whole portfolio into a single total row
  sources      one row per silver source
  portfolios   one row per portfolio (+ a sentinel row per source)
  accounts     one row per account
  positions    one row per individual holding (instrument)

Each view takes a date (-d), output format (-f), currency (-x),
--fx-mode and -p/--privacy; all but global also take -C/--columns.
Totals reconcile: global == Σ sources == Σ portfolios == Σ accounts
== positions --with-cash. Run 'wealthdb holdings <view> -h' for a
view's full flags.
`
