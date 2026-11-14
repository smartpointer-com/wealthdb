package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func init() {
	register("holdings", routeReport(holdingsFamily))
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
			name: "global", usage: globalCmdUsage, fixed: true, total: balanceTotal,
			principal: fixedColumn("total_value_outccy"),
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
			takes: []string{"with_cash", "missing_basis"},
			build: func(cfg *config.Config, req request, _ *toolArgs) *report {
				ccy, asOf := req.currency, req.asOf
				missing, withCash := missingOr(req.missing, cfg), req.withCash
				kindOf, loadKinds := sourceKinds()
				return withLoads(newReport(buildColumnRegistry(ccy, kindOf, req.categories), defaultColumns,
					func(ctx context.Context, db *sql.DB) ([]gold.PositionRow, error) {
						return gold.PositionsAsOf(ctx, db, asOf, ccy, missing, withCash)
					}), loadKinds)
			},
		},
	},
	options: []option{
		{name: "as_of", cli: []string{"d", "as-of"}},
		{name: "with_cash", cli: []string{"with-cash"}, kind: optFlag, only: true},
		{name: "missing_basis", cli: []string{"missing-basis"}, only: true},
	},
	window:     asOfDate,
	currency:   true,
	narrow:     "a filter",
	viewPrefix: true,
	usage:      func() string { return holdingsUsage },
	settle: func(_ *family, in optionSource, req *request, notes *[]string) error {
		// The CLI knows the two flags on positions alone.
		if req.view != "positions" && in.has("missing_basis") {
			return fmt.Errorf("holdings: missing_basis belongs to view=positions")
		}
		req.withCash = req.view == "positions" && in.on("with_cash")
		// Cash is a position only when asked for; a filter for the cash
		// class has asked.
		if req.view == "positions" && !req.withCash && normalizeCell(in.str("asset_class")) == "cash" {
			req.withCash = true
			*notes = append(*notes, "with_cash on for the cash filter")
		}
		return nil
	},
	after: func(in optionSource, cfg *config.Config, req *request) error {
		if req.view != "positions" {
			return nil
		}
		var err error
		req.missing, err = missingBasisOption(in, cfg, req.view)
		return err
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
