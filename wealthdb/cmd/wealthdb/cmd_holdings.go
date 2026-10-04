package main

import (
	"context"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

func init() {
	register("holdings", cmdHoldings)
}

// holdingsViews are the point-in-time portfolio views, grouped under
// `wealthdb holdings` so the top-level command surface stays small as
// more report families land (e.g. the sibling `wealthdb returns`). They
// share one flag idiom (-d / -f / -x / -p, plus -C on all
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

const holdingsUsage = `wealthdb holdings — what is held, where, and what it is worth, as of a date

usage:
  wealthdb holdings <view> [-d DATE] [-f FORMAT] [-x CCY] [-C COLS] [-p]

views (coarsest → finest):
  global       one row: cash, positions value and total value of everything (net worth)
  sources      one row per source (institution)
  portfolios   one row per portfolio, plus one row per source for its ungrouped accounts
  accounts     one row per account, with its kind, tax wrapper and value
  positions    one row per individual holding; --with-cash adds the cash lines

Holdings cover every collected account: bank, card, brokerage, pension
and crypto accounts, and the property, loans and private holdings
recorded by hand. A loan or a card balance is a negative value. Each
source contributes its latest snapshot on or before the as-of date
(-d, default today). Totals reconcile: global == Σ sources ==
Σ portfolios == Σ accounts == positions --with-cash.

There are no row filters: pick the view, then filter the output (grep
on the table, or -f json and jq). Run 'wealthdb holdings <view> -h'
for a view's columns.
`
