package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// toolSpec is one MCP tool: what the model reads about it, its
// parameters, and how a call runs.
type toolSpec struct {
	name        string
	title       string
	description string
	params      []param
	// family is set for the tools that run a report through the row
	// layer; run is set for the one that answers otherwise (describe).
	family *family
	run    func(ctx context.Context, a *toolArgs) (toolOutput, error)
}

// windowRule is how a family reads its dates: a window over from/to
// with its CLI default, an as-of date, or none.
type windowRule int

const (
	noWindow windowRule = iota
	asOfDate
	trailingYear   // spending, income, cashflow: the last twelve months
	sinceInception // returns: since the first snapshot
	pastMonth      // transactions: the past 30 days
)

// family is how a report tool turns a call's arguments into a report.
type family struct {
	// views are the tool's views, coarse to fine; nil for a tool with
	// one shape.
	views []string
	// carries lists the filters each view reads, by view ("" for a
	// tool with one shape). It decides which view a filter picks.
	carries map[string][]string
	// principal is the main column of the request's view: what
	// "value", "amount" and the family's own word mean in a sort.
	principal func(req request) string
	window    windowRule
	// currency says the tool takes a currency parameter.
	currency bool
	// narrow is the paging line's advice for cutting a long result.
	narrow string
	// prepare fills the family's options into req once the view is
	// settled, refuses what the CLI refuses, and notes what it chose.
	prepare func(a *toolArgs, req *request, notes *[]string) error
	build   func(cfg *config.Config, req request, a *toolArgs) *report
	// header names the options in force, for the result header.
	header func(req request) []string
}

const dateDoc = `YYYY, YYYY-MM, YYYY-MM-DD, or "today", "yesterday", "last month", "last year".`

func stringParam(name, doc string, enum ...string) param {
	return param{name: name, kind: paramString, doc: doc, enum: enum}
}

// rowParams are a report tool's parameters: the window, the tool's own
// (extra), the currency, and the shape of the result. The order is the
// order a model reads them in.
func (s *mcpServer) rowParams(window bool, extra ...param) []param {
	var ps []param
	if window {
		ps = append(ps,
			stringParam("from", "Window start: "+dateDoc+" Snaps to the start of the unit."),
			stringParam("to", "Window end: "+dateDoc+" Snaps to the end of the unit. Default: today, unless the tool's description says otherwise."))
	}
	ps = append(ps, extra...)
	ps = append(ps, stringParam("currency", "3-letter output currency, e.g. USD or CHF. Default: the configured currency."))
	return append(ps, s.shapeParams()...)
}

// shapeParams pick, order and page the rows of any table result.
func (s *mcpServer) shapeParams() []param {
	return []param{
		stringParam("search", "Keep only rows where any text cell contains this (case-insensitive)."),
		stringParam("columns", `Columns to show: "default", "all", a comma list of column names, or "+add,-remove". describe(topic=<tool>) lists them.`),
		stringParam("sort", `Sort by a column of the result; prefix "-" for descending. "-value" means the view's main money column, largest first. Money columns sort by size regardless of sign.`),
		{name: "limit", kind: paramInteger, doc: s.limitDoc()},
		{name: "offset", kind: paramInteger, doc: "Rows to skip, for paging (default 0)."},
		stringParam("format", "Result format (default table).", resultFormats...),
	}
}

func (s *mcpServer) limitDoc() string {
	doc := fmt.Sprintf("Rows to return (default %d; 0 returns every row", s.rows)
	if s.maxRows > 0 {
		doc += fmt.Sprintf(", up to %d", s.maxRows)
	}
	return doc + ")."
}

var (
	sourceParam  = stringParam("source", `Keep only this source id (e.g. "schwab"); a comma list keeps several.`)
	accountParam = stringParam("account", "Keep only rows whose account name, id or nickname contains this text (case-insensitive).")
)

// tools are the server's fourteen tools, in the order describe lists them.
func (s *mcpServer) tools() []toolSpec {
	return []toolSpec{
		{
			name: "holdings", title: "Holdings",
			description: "What is held, where, and what it is worth, as of a date (default today): bank, card, brokerage, pension and crypto accounts, and the property, loans and private holdings recorded by hand (a loan is negative). " +
				"view=global: one total row with cash, positions value and total value (net worth); it has no per-account columns, so filters need another view. view=sources: one row per institution, the row for how much is at one institution. " +
				"view=portfolios: one row per portfolio. view=accounts: one row per account with its kind, tax wrapper and value. " +
				"view=positions: every individual holding with symbol, quantity and value. " +
				`Example: {view: "global", as_of: "2025-12-31", currency: "CHF"}.`,
			params: s.rowParams(false,
				stringParam("view", "Which rows to return (default global).", holdingsFamily.views...),
				stringParam("as_of", "As-of date: "+dateDoc+" Default: today."),
				param{name: "with_cash", kind: paramBoolean, doc: "positions only: also list cash as one row per account and currency."},
				sourceParam, accountParam,
				stringParam("symbol", "positions only: keep only holdings whose symbol or name contains this text."),
				stringParam("asset_class", "positions only: keep only this asset class (e.g. public_equity, fixed_income, cash, real_estate, crypto)."),
				stringParam("tax_wrapper", "accounts: keep only this tax wrapper (e.g. roth_ira, 401k, 529, taxable_joint)."),
				missingBasisParam("positions only: how ", ""),
			),
			family: holdingsFamily,
		},
		{
			name: "returns", title: "Returns",
			description: "How investments performed over a window, as time-weighted (twr) and money-weighted (mwr) return percentages, after fees and taxes. " +
				"view=global: the whole portfolio. view=sources: per institution. view=portfolios: per portfolio. view=accounts: per account (exact; the other views are best-effort). " +
				"By default one row per entity for the whole window; period adds buckets. Read the quality column: an n/a always has its reason there. " +
				"Default window: since the first snapshot. " +
				`Example: {view: "accounts", from: "2025", to: "2025", period: "total"}.`,
			params: s.rowParams(true,
				stringParam("view", "Grain (default global).", returnsFamily.views...),
				stringParam("method", "twr, mwr or both (default both).", "twr", "mwr", "both"),
				stringParam("period", "Bucket size (default total: one row per entity for the whole window). monthly, quarterly or annual add one row per bucket.",
					"monthly", "quarterly", "annual", "total"),
				stringParam("annualize", "auto (default; only spans of a year or more), always, never.", "auto", "always", "never"),
				param{name: "netting", kind: paramBoolean, doc: "Net internal transfers at coarse grains (default true)."},
				stringParam("inception", "full (default) or strict.", "full", "strict"),
				sourceParam, accountParam,
			),
			family: returnsFamily,
		},
		{
			name: "gains", title: "Gains",
			description: "What was gained or lost (P&L) on what is held, realized and unrealized, over a window (default: the last twelve months). " +
				"view=summary: one row for the whole portfolio with realized, unrealized at the start and the end, the change, gain = realized + change, and the share of value with a cost basis. " +
				"view=sources, portfolios, accounts: the same per institution, portfolio or account (with its tax wrapper); they add up to the summary. " +
				"view=positions: per account and holding. view=realized: each realized lot, as the sale's tax document or statement states it, or as the lot engine rebuilt it from the trades. view=lots: the open lots at the window's end. view=coverage: per account, where the figures are blind. view=check: the lot engine against the stated lots and cost bases, per account. " +
				"Read the quality column: it names every gap, such as sales without a tax document. " +
				`Example: {view: "realized", from: "2025", to: "2025"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", gainsFamily.views...),
				stringParam("period", "Bucket size for summary, sources, portfolios and accounts (default total: one row for the whole window). monthly, quarterly or annual give a series.", reportPeriodNames...),
				stringParam("documents", "realized only: primary (default; each sale once) or all (every document's copy of a sale).", "primary", "all"),
				param{name: "newest_first", kind: paramBoolean, doc: "realized only: newest first (default false: oldest first)."},
				sourceParam, accountParam,
				stringParam("symbol", "positions, realized and lots: keep only rows whose symbol or name contains this text."),
				stringParam("asset_class", "positions only: keep only this asset class (e.g. public_equity, fixed_income, private_equity)."),
				stringParam("term", "realized and lots: short or long."),
				stringParam("tax_wrapper", "accounts only: keep only this tax wrapper (e.g. taxable_joint, roth_ira)."),
				missingBasisParam("Not on coverage and check. How ", " Under zero, quality says basis_assumed_zero=N."),
			),
			family: gainsFamily,
		},
		{
			name: "exposure", title: "Exposure",
			description: "What is really owned: every fund looked through to what it holds, down to the single security, added to what is held directly, as of a date (default today). " +
				"view=classes: by asset class. view=sectors: by asset class and sector. view=sizes: public equity by large, mid and small caps. view=countries: by country with its region. view=currencies: by the currency a value moves with (a fiat currency, a metal such as XAU, a coin such as BTC). " +
				"view=credit: debt by rating bucket with its grade, IG or HY. view=maturities: debt and cash by time to maturity. view=coupons: debt by coupon kind (fixed, floating, inflation_linked, zero). view=cycles: public equity by cyclical, sensitive and defensive sectors. " +
				"view=situs: by legal situs, the country whose law each holding is an interest under; it looks through no fund, so a fund counts whole at its own situs, and it is no exposure. " +
				"view=securities: each security, held directly and through every fund. view=issuers: each issuer, its stock, bonds and deposits together, largest first, with the running share. view=funds: each fund held directly, what describes it and how old that is. view=coverage: what the figures rest on, the labels no map knows, the holdings no source describes, the private holdings no declaration describes, how far each fund read from a report lies from its proxy, and what each situs rests on. " +
				"Every view sums to net worth; share_pct is a share of it, direct_pct and via_funds_pct split it where present. " +
				"class_share_pct is a share of what the row's dimension describes: its asset class on sectors and sizes, every row but (none) on credit, maturities, coupons and cycles, so those rows sum to 100. " +
				"Read the quality column and view=coverage before trusting a figure. " +
				`Example: {view: "countries", as_of: "2025-12-31"}.`,
			params: s.rowParams(false,
				stringParam("view", "Which breakdown (default classes).", exposureFamily.views...),
				stringParam("as_of", "As-of date: "+dateDoc+" Default: today."),
				stringParam("country", "countries and securities only: the country a security counts in, risk (default) or domicile.",
					exposureCountryBases...),
			),
			family: exposureFamily,
		},
		{
			name: "yield", title: "Yield",
			description: "What the holdings earn now, and how they move with interest rates, as of a date (default today). A rate belongs to a currency: an aggregate row is one entity in one currency, plus a value-weighted total (quality mixed). " +
				"view=global: one row per currency and a total. view=sources, portfolios, accounts: the same per entity. view=positions: per holding, with its kind and figures; its coupon, maturity_date and price on request (columns=+coupon,maturity_date,price). " +
				"view=securities: each security held directly and through funds: a bond's coupon, maturity, price and implied yield; a stock's earnings yield, P/E, dividend yield, payout and growth. " +
				"view=funds: each fund held directly: what its lines imply, its page's yields and duration, as of which day. view=cash: each cash balance, term deposit and money-market fund with its rate, the benchmark and the gap, worst paid first. " +
				"view=curve: DV01 per currency and tenor (" + strings.Join(canonical.RiskTenors, ", ") + "), funds, bonds, cash and liabilities. view=coverage: what the figures rest on, what is missing, and where each dividend's record day comes from (record_day: stated by the dividend calendar, derived from its ex day, learned from its issuer's gap, the market's lag, or none; dropped: those the record window drops). " +
				"income_yield is the next twelve months' projected income over value; implied_yield the yield the price implies in the market's convention, implied_yield_annual the same as an effective annual rate; dv01 the value's fall for a basis point. Read the quality column. " +
				`Example: {view: "cash"}.`,
			params: s.rowParams(false,
				stringParam("view", "Which rows to return (default global).", yieldFamily.views...),
				stringParam("as_of", "As-of date: "+dateDoc+" Default: today."),
				stringParam("period", andList(yieldSeriesViews)+": a series at the end of every "+orList(seriesPeriods)+
					" period of from/to (default: the last twelve months).", seriesPeriods...),
				stringParam("from", "With period: the series' start: "+dateDoc),
				stringParam("to", "With period: the series' end: "+dateDoc+" Default: today."),
				param{name: "converted", kind: paramBoolean, doc: andList(yieldAggregateViews) + ": one row per entity in the output currency, rates across currencies (default false)."},
			),
			family: yieldFamily,
		},
		{
			name: "transactions", title: "Transactions",
			description: "Every booked transaction over a window (default: the past 30 days): trades, dividends, interest, fees, deposits, withdrawals, transfers. " +
				"One row per line with date, account, kind, symbol, amount and its value in the output currency. Oldest first unless newest_first. " +
				`Example: {from: "2026-03", to: "2026-03", sort: "-value", limit: 10}.`,
			params: s.rowParams(true,
				param{name: "newest_first", kind: paramBoolean, doc: "Newest first (default false: oldest first)."},
				sourceParam, accountParam,
				stringParam("kind", "Keep only this transaction kind (e.g. buy, sell, dividend, interest, fee, deposit, withdrawal, purchase)."),
				stringParam("symbol", "Keep only rows whose symbol or instrument contains this text."),
			),
			family: transactionsFamily,
		},
		{
			name: "spending", title: "Spending",
			description: "What the tracked accounts spent over a window (default: the last twelve months). " +
				"view=summary: one row per period. view=categories: one row per period and category, with its share of the period. " +
				"view=transactions: the spending lines with merchant and category. " +
				"spend and refunds are positive amounts; net_spend = spend - refunds. Own-account moves (card payments, transfers) and mortgage payments are never spending: they are in cashflow. " +
				`Example: {view: "categories", from: "2025", to: "2025", period: "total"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", spendingFamily.views...),
				stringParam("period", "Bucket size (default total: the whole window as one row per category). monthly, quarterly or annual give a series.", reportPeriodNames...),
				stringParam("level", "Category vocabulary of the categories view: primary (default, broad groups) or detailed.", "primary", "detailed"),
				sourceParam, accountParam,
				stringParam("category", `Keep only categories whose label or id contains this text (e.g. "groceries", "travel"); switches the categories view to level=detailed unless level is given.`),
				stringParam("merchant", "transactions only: keep only rows whose merchant contains this text."),
			),
			family: spendingFamily,
		},
		{
			name: "income", title: "Income",
			description: "What the tracked accounts received over a window (default: the last twelve months): salary, dividends, interest, rent, refunds, rewards; and what the holdings are expected to pay. " +
				"view=summary: one row per period. view=types: one row per period and income type, with its share. " +
				"view=transactions: the income lines with payer and type. Amounts are gross as booked; income and reversals are positive, net_income = income - reversals. " +
				"The forward views take a window that may reach into the future (default: the projection's horizon after the current month, twelve months unless the config says otherwise; a lone from is its own span, a lone to starts next month): a day up to today shows what was received, a day after it what is expected. " +
				"view=projection: one row per month (period monthly, quarterly or annual) with received, expected and each type (interest, coupons, dividends, distributions, rent, pension), withheld, and basis, what the expected part rests on. " +
				"view=calendar: each expected payment, dated where its date is known, and each principal a maturity returns. " +
				"view=sources, portfolios, accounts, positions: received, expected, total, last_year (the same window a year before) and next_12m per entity; positions lists every holding, the ones that pay nothing too. " +
				"rates, maturing and shift say what the expected income assumes. " +
				`Example: {view: "projection", from: "2026", to: "2026"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", incomeFamily.views...),
				stringParam("period", "Bucket size (default total: the whole window as one row per type; monthly for the forward views). monthly, quarterly or annual give a series.", reportPeriodNames...),
				stringParam("level", "Type vocabulary of the types view: detailed (default) or primary.", "primary", "detailed"),
				stringParam("rates", "Forward views: constant (today's rates for the whole window) or forward (floating rates along the currency's forward curve). Default "+string(yield.DefaultAssumptions.Path)+".", pathNames()...),
				stringParam("maturing", "Forward views: what a maturing holding's principal earns: cash (its account's cash rate), idle (nothing) or roll (the same holding again). Default "+string(yield.DefaultAssumptions.Rule)+".", ruleNames()...),
				param{name: "shift", kind: paramInteger, doc: "Forward views: a parallel shift of the floating rates, in basis points (default 0)."},
				sourceParam, accountParam,
				stringParam("type", `Keep only income types whose label or id contains this text (e.g. "dividend", "salary", "interest").`),
				stringParam("payer", "transactions only: keep only rows whose payer contains this text."),
				stringParam("symbol", "calendar and positions: keep only rows whose symbol contains this text."),
			),
			family: incomeFamily,
		},
		{
			name: "cashflow", title: "Cash flow",
			description: "Where the household's cash came from and where it went over a window (default: the last twelve months), as a cash flow statement. " +
				"view=summary: one row per period with operating_in, operating_out, investing, financing, vehicles (all retirement, education and health plans together) and net_cash_flow. " +
				"view=flows: one row per period and node (section, class, group) with inflow, outflow and net. view=sankey: the diagram's edges for the whole window. " +
				"view=transactions: the lines. view=coverage: per account and period, how far the statement can be trusted (read status first). " +
				"To split the vehicles into retirement, education and health, use view=flows with a class filter; mortgage and loan payments are the Mortgage class under financing. " +
				"Positive is cash arriving, negative is cash leaving. " +
				`Example: {view: "flows", from: "2025", to: "2025", level: "class", period: "total"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", cashflowFamily.views...),
				stringParam("period", "Bucket size (default total: the whole window as one row per node). monthly, quarterly or annual give a series. Not for sankey.", reportPeriodNames...),
				stringParam("level", "Node grain for flows and sankey: section, class or group (default group).", "section", "class", "group"),
				stringParam("investing", "whole (default): investing as one node; class: one node per asset class.", cashflowInvestingGrains...),
				sourceParam, accountParam,
				stringParam("section", "Keep only this section: operating_in, operating_out, investing, financing, vehicles, cash."),
				stringParam("class", `Keep only classes whose label contains this text (e.g. "Earnings", "Consumption", "Mortgage", "Retirement").`),
				stringParam("group", "Keep only groups whose label contains this text."),
			),
			family: cashflowFamily,
		},
		{
			name: "status", title: "Status",
			description: "Is the data current, and what does each source hold: per source its kind, row counts, latest snapshot, latest transaction and whether newer data is waiting to be loaded. " +
				"source narrows to one source and adds detail. Example: {}.",
			params: append([]param{
				stringParam("source", "One source id, for its detail and recent loads."),
				{name: "verbose", kind: paramBoolean, doc: "Add the counts of rows no category, type or asset class could be found for."},
			}, s.shapeParams()...),
			family: statusFamily,
		},
		{
			name: "snapshots", title: "Snapshots",
			description: "Which dates gold has data for, per source, oldest first. Use it to confirm a date exists before concluding a balance is zero. " +
				`Example: {source: "schwab"}.`,
			params: append([]param{
				stringParam("source", "One source id, or a comma list. Default: every source."),
				{name: "latest_only", kind: paramBoolean, doc: "Only the newest date per source."},
			}, s.shapeParams()...),
			family: snapshotsFamily,
		},
		{
			name: "resolutions", title: "Ticker resolutions",
			description: "Diagnostic: the resolved instrument ticker symbols (model-derived and manual overrides) the reports fall back on where a source carried no ticker.",
			params:      append([]param{sourceParam}, s.shapeParams()...),
			family:      resolutionsFamily,
		},
		{
			name: "categorizations", title: "Stored categorizations",
			description: "Diagnostic: the stored merchant and payer category verdicts the spending and income reports apply.",
			params: append([]param{
				stringParam("family", "spending or income (default both).", "spending", "income"),
				stringParam("category", "Keep only verdicts whose category id contains this text."),
			}, s.shapeParams()...),
			family: categorizationsFamily,
		},
		{
			name: "describe", title: "Describe",
			description: "Help. Without topic: what this server is, the configured sources and their latest data dates, the default currency, the row limit, and which tool answers which question. " +
				"topic=<tool name>: that tool's views, every column it can show, its filters and conventions. topic=dates, privacy, quality or glossary: the conventions.",
			params: []param{stringParam("topic", "A tool name, or dates, privacy, quality, glossary. Default: the overview.")},
			run:    s.describe,
		},
	}
}

// ---- families ------------------------------------------------------------

var holdingsFamily = &family{
	views: holdingsViews,
	carries: map[string][]string{
		"sources":    {"source"},
		"portfolios": {"source"},
		"accounts":   {"source", "account", "tax_wrapper"},
		"positions":  {"source", "account", "symbol", "asset_class"},
	},
	principal: byView(map[string]string{
		"global": "total_value_outccy", "sources": "total_value_outccy", "portfolios": "total_value_outccy",
		"accounts": "total_value_outccy", "positions": "value",
	}),
	window:   asOfDate,
	currency: true,
	narrow:   "a filter",
	prepare: func(a *toolArgs, req *request, notes *[]string) error {
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
	build: func(cfg *config.Config, req request, _ *toolArgs) *report {
		req.missing = missingOr(req.missing, cfg)
		return holdingsReport(req)
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

// missingBasisParam is the missing_basis argument of the tools that
// read a cost basis; scope opens its doc ("How ", or the views it
// belongs to), and note follows the readings.
func missingBasisParam(scope, note string) param {
	return stringParam("missing_basis", scope+"a missing cost basis counts: ignore (the default; a figure that needs one is blank) "+
		"or zero (it counts as 0)."+note+" The config's lots.missing_basis sets the default.",
		stringsOf(lots.MissingBasisReadings)...)
}

// stringsOf spells a vocabulary as strings: a flag's choices, a param's
// enum.
func stringsOf[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// missingBasisArg is the missing_basis argument, "" when absent: build
// then takes the config's default (missingOr).
func missingBasisArg(a *toolArgs) (lots.MissingBasis, error) {
	if !a.has("missing_basis") {
		return "", nil
	}
	m, err := lots.ParseMissingBasis(a.str("missing_basis"))
	if err != nil {
		return "", fmt.Errorf("missing_basis: %w", err)
	}
	return m, nil
}

var returnsFamily = &family{
	views: returnsViews,
	carries: map[string][]string{
		"sources":    {"source"},
		"portfolios": {"source"},
		"accounts":   {"source", "account"},
	},
	// On returns, "value" means the return itself.
	principal: func(req request) string {
		if req.method == "mwr" {
			return "mwr"
		}
		return "twr"
	},
	window:   sinceInception,
	currency: true,
	narrow:   "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		req.method = orDefault(a.str("method"), "both")
		req.period = orDefault(a.str("period"), "total")
		req.annualize = orDefault(a.str("annualize"), "auto")
		req.inception = orDefault(a.str("inception"), "full")
		req.netting = a.flag("netting", true)
		return nil
	},
	build: func(cfg *config.Config, req request, _ *toolArgs) *report { return returnsReport(req, cfg) },
	header: func(req request) []string {
		h := []string{"method=" + req.method, "period=" + req.period}
		if req.annualize != "auto" {
			h = append(h, "annualize="+req.annualize)
		}
		if !req.netting {
			h = append(h, "netting off")
		}
		if req.inception != "full" {
			h = append(h, "inception="+req.inception)
		}
		return h
	},
}

var exposureFamily = &family{
	views:     exposureViews,
	principal: func(request) string { return "value" },
	window:    asOfDate,
	currency:  true,
	narrow:    "a coarser view",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		req.country = exposureCountryBases[0]
		if !a.has("country") {
			return nil
		}
		if !slices.Contains(exposureCountryViews, req.view) {
			return errCountryView("country")
		}
		req.country = a.str("country") // the enum admits exposureCountryBases alone
		return nil
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return exposureReport(req) },
	header: func(req request) []string {
		if req.country == "domicile" {
			return []string{"country by domicile"}
		}
		return nil
	},
}

var gainsFamily = &family{
	views: gainsViews,
	carries: map[string][]string{
		"sources":    {"source"},
		"portfolios": {"source"},
		"accounts":   {"source", "account", "tax_wrapper"},
		"positions":  {"source", "account", "symbol", "asset_class"},
		"realized":   {"source", "account", "symbol", "term"},
		"lots":       {"source", "account", "symbol", "term"},
		"coverage":   {"source", "account"},
		"check":      {"source", "account"},
	},
	principal: byView(map[string]string{
		"summary": "gain", "sources": "gain", "portfolios": "gain", "accounts": "gain",
		"positions": "gain", "realized": "gain_outccy", "lots": "unrealized_gain_outccy", "coverage": "value",
		"check": "cost_delta",
	}),
	window:   trailingYear,
	currency: true,
	narrow:   "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		// The CLI's refusal: on another view the two would ask a
		// question it cannot answer.
		if req.view != "realized" && (a.has("documents") || a.has("newest_first")) {
			return fmt.Errorf("gains: documents and newest_first belong to view=realized")
		}
		if !gainsReadsBasis(req.view) && a.has("missing_basis") {
			return fmt.Errorf("gains: missing_basis does not apply to view=%s", req.view)
		}
		req.period = orDefault(a.str("period"), "total")
		req.allDocuments = a.str("documents") == "all"
		req.newestFirst = a.flag("newest_first", false)
		var err error
		if req.missing, err = missingBasisArg(a); err != nil {
			return fmt.Errorf("gains: %w", err)
		}
		return nil
	},
	build: func(cfg *config.Config, req request, _ *toolArgs) *report {
		req.missing = missingOr(req.missing, cfg)
		return gainsReport(req)
	},
	header: func(req request) []string {
		var h []string
		if req.missing == lots.MissingZero {
			h = append(h, "missing_basis=zero")
		}
		if _, ok := gainsGrains[req.view]; ok {
			return append(h, "period="+req.period)
		}
		if req.allDocuments {
			h = append(h, "documents=all")
		}
		if req.newestFirst {
			h = append(h, "newest first")
		}
		return h
	},
}

var transactionsFamily = &family{
	carries:   map[string][]string{"": {"source", "account", "kind", "symbol"}},
	principal: byView(map[string]string{"": "value"}),
	window:    pastMonth,
	currency:  true,
	narrow:    "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		req.newestFirst = a.flag("newest_first", false)
		return nil
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return transactionsReport(req) },
	header: func(req request) []string {
		if req.newestFirst {
			return []string{"newest first"}
		}
		return nil
	},
}

var spendingFamily = &family{
	views: spendingViews,
	carries: map[string][]string{
		"categories":   {"category"},
		"transactions": {"source", "account", "category", "merchant"},
	},
	principal: byView(map[string]string{"summary": "net_spend", "categories": "net_spend", "transactions": "value"}),
	window:    trailingYear,
	currency:  true,
	narrow:    "from/to or a filter",
	prepare: func(a *toolArgs, req *request, notes *[]string) error {
		req.period = orDefault(a.str("period"), "total")
		req.level = a.str("level")
		if req.level == "" {
			req.level = "primary"
			// A category filter is a name search, and names live in
			// the detailed vocabulary: "groceries" is a detailed
			// category under "Food and drink".
			if req.view == "categories" && a.has("category") {
				req.level = "detailed"
				*notes = append(*notes, "level detailed for the category filter")
			}
		}
		return nil
	},
	build:  func(_ *config.Config, req request, _ *toolArgs) *report { return spendingReport(req) },
	header: bucketHeader,
}

var incomeFamily = &family{
	views: incomeViews,
	carries: map[string][]string{
		"types":        {"type"},
		"transactions": {"source", "account", "type", "payer"},
		"calendar":     {"source", "account", "symbol", "type"},
		"sources":      {"source"},
		"portfolios":   {"source"},
		"accounts":     {"source", "account"},
		"positions":    {"source", "account", "symbol"},
	},
	principal: byView(map[string]string{"summary": "net_income", "types": "net_income", "transactions": "value",
		"projection": "total", "calendar": "value", "sources": "total", "portfolios": "total", "accounts": "total",
		"positions": "total"}),
	window:   trailingYear,
	currency: true,
	narrow:   "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		req.period = orDefault(a.str("period"), "total")
		req.level = orDefault(a.str("level"), "detailed")
		forward := isForwardView(req.view)
		if forward {
			req.period = orDefault(a.str("period"), "monthly")
		}
		if err := checkForwardOptions(req.view, req.period, a.has, func(name string) string { return name }); err != nil {
			return err
		}
		if !forward {
			return nil
		}
		shift, _ := a.integer("shift")
		def := yield.DefaultAssumptions
		req.assumptions = yield.Assumptions{Path: yield.Path(orDefault(a.str("rates"), string(def.Path))),
			Rule: yield.Rule(orDefault(a.str("maturing"), string(def.Rule))), ShiftBP: float64(shift)}
		return nil
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return incomeReport(req) },
	header: func(req request) []string {
		if !isForwardView(req.view) {
			return bucketHeader(req)
		}
		var h []string
		if req.view == "projection" {
			h = append(h, "period="+req.period)
		}
		h = append(h, "rates="+string(req.assumptions.Path), "maturing="+string(req.assumptions.Rule))
		if req.assumptions.ShiftBP != 0 {
			h = append(h, fmt.Sprintf("shift=%+.0fbp", req.assumptions.ShiftBP))
		}
		return h
	},
}

var yieldFamily = &family{
	views: yieldViews,
	principal: func(req request) string {
		if req.view == "curve" {
			return "dv01"
		}
		return "value"
	},
	window:   asOfDate,
	currency: true,
	narrow:   "a coarser view",
	prepare: func(a *toolArgs, req *request, notes *[]string) error {
		req.converted = a.flag("converted", false)
		if _, ok := yieldGrains[req.view]; !ok && req.converted {
			return errConverted("converted")
		}
		if !a.has("period") {
			if a.has("from") || a.has("to") {
				return fmt.Errorf("yield: from and to belong to a series: give a period too")
			}
			return nil
		}
		if err := checkSeriesOptions("yield", req.view, yieldSeriesViews, a.has,
			func(name string) string { return name }); err != nil {
			return err
		}
		var err error
		req.series, err = seriesWindow(windowArgs(a.str("from"), a.str("to")), a.str("period"), time.Unix(req.asOf, 0))
		switch {
		case errors.Is(err, errSeriesAfterToday):
			return fmt.Errorf("yield: %w", err)
		case err != nil:
			return fmt.Errorf("yield: %v; dates are %s", err, dateDoc)
		}
		*notes = append(*notes, "series "+a.str("period"))
		return nil
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return yieldReport(req) },
	header: func(req request) []string {
		if req.converted {
			return []string{"converted"}
		}
		return nil
	},
}

var cashflowFamily = &family{
	views: cashflowViews,
	carries: map[string][]string{
		"flows":        {"section", "class", "group"},
		"sankey":       {"section"},
		"transactions": {"source", "account", "section", "class", "group"},
		"coverage":     {"source", "account"},
	},
	principal: byView(map[string]string{
		"summary": "net_cash_flow", "flows": "net", "sankey": "value", "transactions": "value", "coverage": "gap",
	}),
	window:   trailingYear,
	currency: true,
	narrow:   "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		// The CLI's three refusals, for the CLI's reasons: each would
		// answer a question the caller did not ask.
		switch req.view {
		case "sankey":
			if a.has("period") {
				return fmt.Errorf("cashflow: sankey takes no period — a diagram is a window, not a series; call it once per year instead")
			}
			if a.str("level") == "section" {
				return fmt.Errorf("cashflow: sankey at level=section would draw no inner column; use class or group")
			}
		case "coverage":
			if a.has("currency") {
				return fmt.Errorf("cashflow: coverage takes no currency — each account is measured against its own balances, in its own currency")
			}
		}
		req.period = orDefault(a.str("period"), "total")
		req.level = orDefault(a.str("level"), "group")
		req.investing = orDefault(a.str("investing"), "whole")
		return nil
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return cashflowReport(req) },
	header: func(req request) []string {
		var h []string
		switch req.view {
		case "summary", "coverage":
			h = append(h, "period="+req.period)
		case "flows":
			h = append(h, "period="+req.period, "level="+req.level)
		case "sankey":
			h = append(h, "level="+req.level)
		}
		if req.investing != "whole" && (req.view == "flows" || req.view == "sankey") {
			h = append(h, "investing="+req.investing)
		}
		return h
	},
}

// bucketHeader is the spending and income header: the bucket on the
// aggregating views, the level where it changes the vocabulary.
func bucketHeader(req request) []string {
	switch req.view {
	case "summary":
		return []string{"period=" + req.period}
	case "transactions":
		return nil
	}
	return []string{"period=" + req.period, "level=" + req.level}
}

// status's source parameter is not a row filter: it asks for the
// source's detail block instead of the table.
var statusFamily = &family{
	narrow:  "search",
	prepare: noOptions,
	build: func(cfg *config.Config, _ request, a *toolArgs) *report {
		return statusReport(cfg, a.flag("verbose", false))
	},
}

var snapshotsFamily = &family{
	carries: map[string][]string{"": {"source"}},
	narrow:  "source or latest_only",
	prepare: noOptions,
	build: func(_ *config.Config, _ request, a *toolArgs) *report {
		return snapshotsReport(a.flag("latest_only", false))
	},
}

var resolutionsFamily = &family{
	carries: map[string][]string{"": {"source"}},
	narrow:  "source or search",
	prepare: noOptions,
	build:   func(*config.Config, request, *toolArgs) *report { return resolutionsReport("") },
}

var categorizationsFamily = &family{
	carries: map[string][]string{"": {"category"}},
	narrow:  "family, category or search",
	prepare: noOptions,
	build: func(_ *config.Config, _ request, a *toolArgs) *report {
		// The parameter's enum has already admitted only a family
		// name, or nothing for both.
		families, _ := resolveCategorizeFamilies(a.str("family"))
		return categorizationsReport(families, "")
	},
}

func noOptions(*toolArgs, *request, *[]string) error { return nil }

// byView is a principal column per view.
func byView(m map[string]string) func(request) string {
	return func(req request) string { return m[req.view] }
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---- the call pipeline ---------------------------------------------------

// runFamily is every report tool's call: settle the view, the dates
// and the options, check the columns and the sort before gold is
// touched, fetch the rows on a read-only handle held for the fetch
// alone, then filter, sort, page and render.
func (s *mcpServer) runFamily(ctx context.Context, tool string, fam *family, a *toolArgs) (toolOutput, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return toolOutput{}, err
	}
	// status for one source is a different shape: its detail block.
	if tool == "status" && a.has("source") {
		var text string
		err := s.withGold(ctx, cfg, func(db *sql.DB) (err error) {
			text, err = statusDetail(ctx, db, cfg, a.str("source"), a.flag("verbose", false))
			return err
		})
		return toolOutput{text: text}, err
	}

	var named []string
	for _, f := range filterOrder {
		if a.has(f) {
			named = append(named, f)
		}
	}
	req, notes, err := s.settle(tool, fam, cfg, a, named)
	if err != nil {
		return toolOutput{}, err
	}
	rep := fam.build(cfg, req, a)
	sh, err := s.shape(tool, fam, rep, req, a)
	if err != nil {
		return toolOutput{}, err
	}

	var rows reportRows
	if err := s.withGold(ctx, cfg, func(db *sql.DB) (err error) {
		rows, err = rep.fetch(ctx, db)
		return err
	}); err != nil {
		return toolOutput{}, err
	}
	rs := newResultSet(rep, rows, s.privacy)
	filters := make([]rowFilter, 0, len(named))
	for _, f := range named {
		filters = append(filters, newRowFilter(rep, f, a.str(f)))
	}
	idx := rs.filter(filters, a.str("search"))
	if sh.sortCol >= 0 {
		rs.sortRows(idx, sh.sortCol, sh.desc)
	}

	head, meta := s.header(tool, fam, rep, req, len(idx), sh, append(notes, sh.notes...))
	p := page{idx: cut(idx, sh.offset, sh.limit), total: len(idx), offset: sh.offset, limit: sh.limit,
		capped: sh.capped, narrow: fam.narrow}
	if len(idx) == 0 {
		p.empty = emptyNote(rs, fam, req.view, named, a)
	}
	return render(rs, p, sh.cols, orDefault(a.str("format"), formatTable), head, meta), nil
}

// settle turns a call's arguments into the report request: the view
// the filters need, the currency, the dates, and the family's options.
// notes are what it chose for the model, for the header.
func (s *mcpServer) settle(tool string, fam *family, cfg *config.Config, a *toolArgs, named []string) (request, []string, error) {
	var notes []string
	view := a.str("view")
	if fam.views != nil {
		if view == "" {
			view = fam.views[0]
		}
		settled, ok := escalate(fam.views, fam.carries, view, named)
		if !ok {
			return request{}, nil, filterViewError(tool, fam, view, named)
		}
		if settled != view {
			notes = append(notes, fmt.Sprintf("view %s chosen for the %s filter", settled, strings.Join(named, ", ")))
		}
		view = settled
	}
	req := request{view: view, categories: overrideCategories(cfg)}
	if fam.currency {
		req.currency = strings.ToUpper(a.str("currency"))
		if req.currency == "" {
			req.currency = cfg.DefaultCurrency
		}
		if !config.IsLikelyISO4217(req.currency) {
			return request{}, nil, fmt.Errorf("%s: currency %q is not a 3-letter code such as USD or CHF", tool, a.str("currency"))
		}
	}
	if err := resolveDates(fam.window, a, s.now(), cfg.YieldHorizon(), &req); err != nil {
		return request{}, nil, fmt.Errorf("%s: %v", tool, err)
	}
	if err := fam.prepare(a, &req, &notes); err != nil {
		return request{}, nil, err
	}
	return req, notes, nil
}

// resultShape is how a call wants its rows: the columns, the sort and
// the page, with notes for what a name resolved to.
type resultShape struct {
	cols    []int
	sortCol int // -1 for the report's own order
	desc    bool
	limit   int
	capped  bool
	offset  int
	notes   []string
}

// shape resolves the columns, sort, limit and offset parameters
// against the report, before gold is opened, so a misspelt name costs
// no query.
func (s *mcpServer) shape(tool string, fam *family, rep *report, req request, a *toolArgs) (resultShape, error) {
	sh := resultShape{sortCol: -1}
	lookup := columnLookup{cols: rep.columns, currency: req.currency}
	if fam.principal != nil {
		lookup.principal = fam.principal(req)
	}
	var err error
	if sh.cols, sh.notes, err = lookup.pick(a.str("columns"), rep.defaults); err != nil {
		return sh, fmt.Errorf("%s: %v", tool, err)
	}
	if spec := strings.TrimSpace(a.str("sort")); spec != "" {
		i, note, err := lookup.resolve(strings.TrimLeft(spec, "+-"), true)
		if err != nil {
			return sh, fmt.Errorf("%s: sort: %v", tool, err)
		}
		if c := rep.columns[i]; s.privacy && (c.privacy == PrivacyMoney || c.privacy == PrivacyQuantity) {
			return sh, fmt.Errorf("%s: %s is redacted on this endpoint, so it cannot order the rows; sort by another column", tool, c.header)
		}
		sh.sortCol, sh.desc = i, strings.HasPrefix(spec, "-")
		if note != "" {
			sh.notes = append(sh.notes, note)
		}
	}
	if sh.limit, sh.capped, sh.offset, err = s.paging(a); err != nil {
		return sh, fmt.Errorf("%s: %v", tool, err)
	}
	return sh, nil
}

// header builds the result's first line and the json result's fields
// beside the rows.
func (s *mcpServer) header(tool string, fam *family, rep *report, req request, rows int, sh resultShape, notes []string) (resultHeader, resultMeta) {
	head := resultHeader{}
	meta := resultMeta{tool: tool, view: req.view, currency: req.currency}
	if fam.views != nil {
		head.add("%s %s", tool, req.view)
	} else {
		head.add("%s", tool)
	}
	switch fam.window {
	case asOfDate:
		if n := len(req.series); n > 0 {
			// A series states its first and last period end, not one day.
			meta.from, meta.to = formatDate(req.series[0].at), formatDate(req.series[n-1].at)
			head.add("window %s → %s", meta.from, meta.to)
		} else {
			meta.asOf = formatDate(req.asOf)
			head.add("as of %s", meta.asOf)
		}
	case trailingYear, sinceInception, pastMonth:
		meta.from, meta.to = windowStart(req.from), formatDate(req.to)
		head.add("window %s → %s", meta.from, meta.to)
	}
	switch {
	case tool == "cashflow" && req.view == "coverage":
		meta.currency = ""
		head.add("each account in its own currency")
	case req.currency != "":
		head.add("%s", req.currency)
	}
	if fam.header != nil {
		head.parts = append(head.parts, fam.header(req)...)
	}
	head.add("%s %s", thousands(rows), plural(rows, "row", "rows"))
	if sh.sortCol >= 0 {
		c := rep.columns[sh.sortCol]
		name, dir := c.header, "ascending"
		if c.privacy == PrivacyMoney {
			name = "|" + name + "|"
		}
		if sh.desc {
			dir = "descending"
		}
		head.add("sorted by %s %s", name, dir)
	}
	for _, n := range notes {
		head.add("%s", n)
	}
	if s.privacy {
		head.add("redacted")
	}
	return head, meta
}

// windowArgs are the CLI's positional window for from and to: FROM TO,
// an open bound ("-") for the one not given, none for neither.
func windowArgs(from, to string) []string {
	switch {
	case from != "" && to != "":
		return []string{from, to}
	case from != "":
		return []string{from, "-"}
	case to != "":
		return []string{"-", to}
	}
	return nil
}

// resolveDates reads from/to or as_of under the family's rule, with
// the CLI's defaults and the CLI's date grammar: from snaps to the
// start of its unit, to and as_of to the end.
func resolveDates(rule windowRule, a *toolArgs, now time.Time, horizon int, req *request) error {
	from, to := a.str("from"), a.str("to")
	args := windowArgs(from, to)
	var err error
	switch rule {
	case asOfDate:
		req.asOf, err = parseAsOf(a.str("as_of"), now)
		if err != nil {
			return fmt.Errorf("as_of %q is not a date; use %s", a.str("as_of"), dateDoc)
		}
		return nil
	case trailingYear:
		// The forward views compute today's holdings, over a window that
		// may reach into the future.
		req.asOf = anchorToDay(now.UTC(), true).Unix()
		if isForwardView(req.view) {
			req.from, req.to, err = parseForwardWindow(from, to, now, horizon)
		} else {
			req.from, req.to, err = parseTrailingYearWindow(args, now)
		}
	case sinceInception:
		req.from, req.to, err = parseReturnsWindow(args, now)
	case pastMonth:
		req.from, req.to, err = parseDateRange(args, now)
	default:
		return nil
	}
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "from > to") {
		return fmt.Errorf("from %q is after to %q", from, to)
	}
	return fmt.Errorf("%v; dates are %s", err, dateDoc)
}

// windowStart renders a window's first day; an open start (the returns
// default, or a lone to) begins with the data.
func windowStart(from int64) string {
	if from == 0 {
		return "start of data"
	}
	return formatDate(from)
}

// paging reads limit and offset against the server's default and
// ceiling. capped reports a limit the ceiling lowered.
func (s *mcpServer) paging(a *toolArgs) (limit int, capped bool, offset int, err error) {
	limit = s.rows
	if n, ok := a.integer("limit"); ok {
		if n < 0 {
			return 0, false, 0, fmt.Errorf("limit must be 0 (every row) or more, not %d", n)
		}
		limit = n
	}
	if s.maxRows > 0 && (limit == 0 || limit > s.maxRows) {
		limit, capped = s.maxRows, true
	}
	if n, ok := a.integer("offset"); ok {
		if n < 0 {
			return 0, false, 0, fmt.Errorf("offset must be 0 or more, not %d", n)
		}
		offset = n
	}
	return limit, capped, offset, nil
}

// filterViewError says why the requested view and every finer one lack
// the filters, and where they are: the view to call when one takes
// them all, else which views carry each.
func filterViewError(tool string, fam *family, view string, named []string) error {
	for _, v := range fam.views {
		if carriesAll(fam.carries, v, named) {
			return fmt.Errorf("%s: the %s view has no %s filter; the %s view has: call view=%s, or use search",
				tool, view, strings.Join(named, ", "), v, v)
		}
	}
	var where []string
	for _, f := range named {
		var vs []string
		for _, v := range fam.views {
			if slices.Contains(fam.carries[v], f) {
				vs = append(vs, v)
			}
		}
		if len(vs) == 0 {
			where = append(where, fmt.Sprintf("no view filters by %s", f))
		} else {
			where = append(where, fmt.Sprintf("%s is on %s", f, orList(vs)))
		}
	}
	return fmt.Errorf("%s: no view takes all of %s together (%s); make one call per view, or use search",
		tool, strings.Join(named, ", "), strings.Join(where, "; "))
}

// emptyNote explains a result with no rows, and for each filter lists
// the values its column takes, so a guessed section or class is
// corrected by the next call rather than concluded from.
func emptyNote(rs *resultSet, fam *family, view string, named []string, a *toolArgs) string {
	var b strings.Builder
	switch {
	case len(named) > 0 || a.has("search"):
		b.WriteString("(no rows: nothing matched the filter")
		if finer := finerViews(fam, view, named); len(finer) > 0 {
			fmt.Fprintf(&b, "; a finer view (%s) may carry what it looks for", strings.Join(finer, ", "))
		}
		b.WriteString(")")
	case fam.window == asOfDate:
		b.WriteString("(no rows: nothing is held as of this date, or it lies before the data begins; snapshots shows where each source's data begins)")
	case fam.window != noWindow:
		b.WriteString("(no rows: the window may lie before the data begins; snapshots shows where each source's data begins)")
	default:
		b.WriteString("(no rows)")
	}
	for _, f := range named {
		cols := newRowFilter(rs.rep, f, "").cols
		if len(cols) == 0 {
			continue
		}
		vals, more := rs.distinctValues(cols[0], 25)
		if len(vals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s values here: %s", f, strings.Join(vals, ", "))
		if more {
			b.WriteString(", …")
		}
	}
	return b.String()
}

// finerViews are the views after view that take every named filter:
// the ones worth a second call when a filter matched nothing here.
func finerViews(fam *family, view string, named []string) []string {
	var out []string
	after := false
	for _, v := range fam.views {
		if after && carriesAll(fam.carries, v, named) {
			out = append(out, v)
		}
		after = after || v == view
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// toolSchema is a tool's JSON input schema: an object of optional
// properties, each typed and described, enums listed.
func toolSchema(params []param) json.RawMessage {
	props := make(map[string]any, len(params))
	for _, p := range params {
		prop := map[string]any{"description": p.doc}
		switch p.kind {
		case paramInteger:
			prop["type"] = "integer"
		case paramBoolean:
			prop["type"] = "boolean"
		default:
			prop["type"] = "string"
		}
		if len(p.enum) > 0 {
			prop["enum"] = p.enum
		}
		props[p.name] = prop
	}
	out, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
	return out
}
