package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("holdings", cmdHoldings)
}

// holdingsViews are the point-in-time portfolio views, coarsest first,
// grouped under `wealthdb holdings`. They share one flag idiom (-d / -f
// / -x / -p, plus -C on all but global) and their totals reconcile:
// global == Σ sources == Σ portfolios == Σ accounts == positions
// --with-cash.
var holdingsViews = []string{"global", "sources", "portfolios", "accounts", "positions"}

// holdingsViewUsage is each view's own -h text.
var holdingsViewUsage = map[string]func() string{
	"global":     globalCmdUsage,
	"sources":    sourcesUsage,
	"portfolios": portfoliosUsage,
	"accounts":   accountsUsage,
	"positions":  positionsUsage,
}

// cmdHoldings routes `wealthdb holdings <view> ...` to the view's run.
func cmdHoldings(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("holdings", subargs, holdingsViews, func() string { return holdingsUsage }, stderr)
	if err != nil {
		return err
	}
	return runHoldingsView(ctx, g, view, rest, stdout, stderr)
}

// runHoldingsView parses one view's flags and writes it. Errors name
// the view alone ("positions: ..."), as its usage line does.
func runHoldingsView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asOf := registerAsOf(fs)
	rf := registerReportFlags(fs, view != "global")
	var withCash *bool
	var missing *string
	if view == "positions" {
		withCash = fs.Bool("with-cash", false, "also emit one row per account and currency with non-zero cash")
		missing = registerMissingBasisFlag(fs)
	}
	fs.Usage = func() { fmt.Fprintln(stderr, holdingsViewUsage[view]()) }
	if err := parseFlags(fs, args, view); err != nil {
		return err
	}
	if err := noPositionals(fs, view); err != nil {
		return err
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(errs.ExitUsage, "%s: %s", view, err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, view)
	if err != nil {
		return err
	}
	req := request{view: view, currency: outCcy, asOf: asOfEpoch}
	if view == "positions" {
		req.withCash = *withCash
		if req.missing, err = resolveMissingBasis(*missing, cfg, view); err != nil {
			return err
		}
	}

	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, holdingsReport(req), *rf.cols, view, open, *rf.privacy, fmtChoice, stdout)
}

// holdingsReport is one holdings view as of req.asOf, the runner the
// CLI views and the MCP server share. The global rollup is one fixed
// row, so its default column set is its whole registry.
func holdingsReport(req request) *report {
	ccy, asOf := req.currency, req.asOf
	switch req.view {
	case "global":
		registry := buildGlobalColumnRegistry(ccy)
		return newReport(registry, columnNames(registry), func(ctx context.Context, db *sql.DB) ([]gold.GlobalRow, error) {
			row, err := gold.GlobalAsOf(ctx, db, asOf, ccy)
			if err != nil {
				return nil, err
			}
			return []gold.GlobalRow{row}, nil
		})
	case "sources":
		return newReport(buildSourceColumnRegistry(ccy), defaultSourceColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.SourceRow, error) {
				return gold.SourcesAsOf(ctx, db, asOf, ccy)
			})
	case "portfolios":
		kindOf, loadKinds := sourceKinds()
		return newReport(buildPortfolioColumnRegistry(ccy, kindOf), defaultPortfolioColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.PortfolioRow, error) {
				if err := loadKinds(ctx, db); err != nil {
					return nil, err
				}
				return gold.PortfoliosAsOf(ctx, db, asOf, ccy)
			})
	case "accounts":
		return newReport(buildAccountColumnRegistry(ccy), defaultAccountColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.AccountRow, error) {
				return gold.AccountsAsOf(ctx, db, asOf, ccy)
			})
	default:
		withCash := req.withCash
		return newReport(buildColumnRegistry(ccy), defaultColumns,
			func(ctx context.Context, db *sql.DB) ([]gold.PositionRow, error) {
				rows, err := gold.PositionsAsOf(ctx, db, asOf, ccy, req.missing)
				if err != nil || !withCash {
					return rows, err
				}
				cash, err := gold.CashAsOf(ctx, db, asOf, ccy)
				if err != nil {
					return nil, err
				}
				return mergeSorted(rows, cash), nil
			})
	}
}

// sourceKinds is how a registry reads a source's kind, which only gold
// knows: the portfolio column's redaction class depends on it.
// kindOf answers from what load reads, and the report's fetch calls
// load before any row is rendered.
func sourceKinds() (kindOf func(string) string, load func(context.Context, *sql.DB) error) {
	kinds := map[string]string{}
	return func(id string) string { return kinds[id] },
		func(ctx context.Context, db *sql.DB) error {
			sk, err := gold.SourceKinds(ctx, db)
			for k, v := range sk {
				kinds[k] = v
			}
			return err
		}
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
account contributes its own latest snapshot on or before the as-of
date (-d, default today). An account's last snapshot ends at the
first later run of its source that reports again the accounts it was
reported with; accounts that stopped reporting too do not count. It
also ends once its source has run for a year past it. So an account
reported only in some of a source's runs drops out at the next run
that reports its companions. An account a run leaves out but a later
run reports again keeps its older snapshot across the gap. Totals
reconcile: global == Σ sources == Σ portfolios == Σ accounts ==
positions --with-cash.

There are no row filters: pick the view, then filter the output (grep
on the table, or -f json and jq). Run 'wealthdb holdings <view> -h'
for a view's columns.`
