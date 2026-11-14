package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func init() {
	register("holdings", cmdHoldings)
}

// holdingsFamily are the point-in-time portfolio views, coarsest
// first, grouped under `wealthdb holdings`. They share one flag idiom
// (-d / -f / -x / -p, plus -C and -F on all but global) and their totals
// reconcile: global == Σ sources == Σ portfolios == Σ accounts ==
// positions --with-cash. Each view has its own -h text.
var holdingsFamily = &family{
	name: "holdings",
	views: []view{
		{
			name: "global", usage: globalCmdUsage, total: balanceTotal, principal: fixedColumn("total_value_outccy"),
			// The global rollup is one fixed row, so its default column
			// set is its whole registry.
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				registry := buildGlobalColumnRegistry(ccy)
				return newReport(registry, columnNames(registry), func(ctx context.Context, db *sql.DB) ([]gold.GlobalRow, error) {
					row, err := gold.GlobalAsOf(ctx, db, asOf, ccy)
					if err != nil {
						return nil, err
					}
					return []gold.GlobalRow{row}, nil
				})
			},
		},
		{
			name: "sources", usage: sourcesUsage, total: balanceTotal, principal: fixedColumn("total_value_outccy"),
			// A source's or a portfolio's tax_wrapper is its accounts'
			// common one, blank where they differ: a filter there would
			// drop the rest.
			excludes: []string{"tax_wrapper"},
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				return newReport(buildSourceColumnRegistry(ccy), defaultSourceColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.SourceRow, error) {
						return gold.SourcesAsOf(ctx, db, asOf, ccy)
					})
			},
		},
		{
			name: "portfolios", usage: portfoliosUsage, total: balanceTotal, principal: fixedColumn("total_value_outccy"),
			excludes: []string{"tax_wrapper"},
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				kindOf, loadKinds := sourceKinds()
				return withLoads(newReport(buildPortfolioColumnRegistry(ccy, kindOf), defaultPortfolioColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.PortfolioRow, error) {
						return gold.PortfoliosAsOf(ctx, db, asOf, ccy)
					}), loadKinds)
			},
		},
		{
			name: "accounts", usage: accountsUsage, total: balanceTotal, principal: fixedColumn("total_value_outccy"),
			build: func(_ *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				kindOf, loadKinds := sourceKinds()
				return withLoads(newReport(buildAccountColumnRegistry(ccy, kindOf, req.categories), defaultAccountColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.AccountRow, error) {
						return gold.AccountsAsOf(ctx, db, asOf, ccy)
					}), loadKinds)
			},
		},
		{
			name: "positions", usage: positionsUsage, total: balanceTotal, principal: fixedColumn("value"),
			build: func(cfg *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				missing, withCash := missingOr(req.missing, cfg), req.withCash
				kindOf, loadKinds := sourceKinds()
				return withLoads(newReport(buildColumnRegistry(ccy, kindOf, req.categories), defaultColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.PositionRow, error) {
						rows, err := gold.PositionsAsOf(ctx, db, asOf, ccy, missing)
						if err != nil || !withCash {
							return rows, err
						}
						cash, err := gold.CashAsOf(ctx, db, asOf, ccy)
						if err != nil {
							return nil, err
						}
						return mergeSorted(rows, cash), nil
					}), loadKinds)
			},
		},
	},
	window:   asOfDate,
	currency: true,
	narrow:   "a filter",
	prepare: func(_ *family, a *toolArgs, req *request, notes *[]string) error {
		if req.view != "positions" && a.has("missing_basis") {
			return fmt.Errorf("holdings: missing_basis belongs to view=positions")
		}
		var err error
		if req.missing, err = missingBasisArg(a); err != nil {
			return fmt.Errorf("holdings: %w", err)
		}
		req.withCash = req.view == "positions" && a.flag("with_cash", false)
		// Cash is a position only when asked for; a filter for the cash
		// class has asked.
		if req.view == "positions" && !req.withCash && normalizeCell(a.str("asset_class")) == "cash" {
			req.withCash = true
			*notes = append(*notes, "with_cash on for the cash filter")
		}
		return nil
	},
	header: func(req request) []string {
		var h []string
		if req.withCash {
			h = append(h, "with cash")
		}
		if req.missing == lots.MissingZero {
			h = append(h, "missing_basis=zero")
		}
		return h
	},
}

// holdingsViews are the holdings views' names, coarsest first.
var holdingsViews = holdingsFamily.names()

// cmdHoldings routes `wealthdb holdings <view> ...` to the view's run.
func cmdHoldings(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("holdings", subargs, holdingsFamily.names(), func() string { return holdingsUsage }, stderr)
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
	v := holdingsFamily.view(view)
	var withCash *bool
	var missing *string
	if view == "positions" {
		withCash = fs.Bool("with-cash", false, "also emit one row per account and currency with non-zero cash")
		missing = registerMissingBasisFlag(fs)
	}
	fs.Usage = func() { fmt.Fprintln(stderr, v.usage()) }
	if err := parseFlags(fs, args, view); err != nil {
		return err
	}
	if err := noPositionals(fs, view); err != nil {
		return err
	}

	asOfEpoch, err := parseAsOf(*asOf, g.clock())
	if err != nil {
		return errs.Newf(errs.ExitUsage, "%s: %s", view, err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, view)
	if err != nil {
		return err
	}
	req := request{view: view, currency: outCcy, asOf: asOfEpoch, categories: overrideCategories(cfg)}
	if view == "positions" {
		req.withCash = *withCash
		if req.missing, err = resolveMissingBasis(*missing, cfg, view); err != nil {
			return err
		}
	}

	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	out := rf.output(fmtChoice, "holdings", holdingsFamily, req, cfg, g.clock())
	out.prefix = view // a column error names the view alone
	return writeReport(ctx, v.build(cfg, req, nil), out, open, stdout)
}

const holdingsUsage = `wealthdb holdings — what is held, where, and what it is worth, as of a date

usage:
  wealthdb holdings global [-d DATE] [-f FORMAT] [-x CCY] [-p]
  wealthdb holdings <view> [-d DATE] [-f FORMAT] [-x CCY] [-C COLS] [-F COND]... [-p]

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
reported with; accounts that stopped reporting too do not count. That
ending waits until the source has run one and a half of the account's
cadences past the snapshot, the median gap between its own snapshot
days. It also ends once its source has run for a year past it. So an
account reported only in some of a source's runs stands through its
own cadence. An account a run leaves out but a later run reports
again keeps its older snapshot across the gap. Totals
reconcile: global == Σ sources == Σ portfolios == Σ accounts ==
positions --with-cash.

-F keeps the rows where a condition holds ('wealthdb help filter');
global takes no -C and no -F. Run 'wealthdb holdings <view> -h' for a
view's columns.`
