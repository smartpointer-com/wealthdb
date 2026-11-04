package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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
	// side marks the views that are no grain of the others but a table
	// of their own (returns' stated): the coarse-to-fine walks of
	// escalate, finerViews and the help skip them.
	side map[string]bool
	// excludes are the named filters a view prints the key column of
	// but does not carry, because the column means something else
	// there; keyAlias names a view's key column where it prints the
	// filter's under another name. Both by view ("" for a tool with one
	// shape); carries derives the rest.
	excludes map[string][]string
	keyAlias map[string]map[string]string
	derive   sync.Once
	carried  map[string][]string
	// adds says how a view's money adds up over its rows, for the
	// total line of a filtered result; nil for a family whose views
	// never add up.
	adds func(view string) totalRule
	// principal is the main column of the request's view: what
	// "value", "amount" and the family's own word mean in a sort.
	principal func(req request) string
	window    windowRule
	// forward says a view of a trailingYear family looks forward: its
	// window defaults to the horizon after the current month and may
	// reach into the future (parseForwardWindow). nil where none does.
	forward func(view string) bool
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

const dateDoc = `YYYY, YYYY-MM, YYYY-MM-DD, today, yesterday, last month, last year`

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
			stringParam("from", "Window start; snaps to the unit's start."),
			stringParam("to", "Window end; snaps to the unit's end. Default today unless the tool says otherwise."))
	}
	ps = append(ps, extra...)
	ps = append(ps, stringParam("currency", "Output currency, e.g. USD or CHF. Default: the configured one."))
	return append(ps, s.shapeParams()...)
}

// shapeParams pick, order and page the rows of any table result.
func (s *mcpServer) shapeParams() []param {
	return []param{
		{name: "filter", kind: paramStrings, doc: filterDoc()},
		stringParam("search", "Keep rows with this text in any cell."),
		stringParam("columns", `"default", "all", names, or "+add,-remove"; describe(topic=<tool>) lists them.`),
		stringParam("sort", `Column to sort by; "-" for descending. "-value": the main money column, largest first whatever the sign.`),
		{name: "max_chars", kind: paramInteger, doc: fmt.Sprintf("Most characters the result may take (default %d); the page ends at a row boundary and the last line says how to get the rest.", s.chars)},
		{name: "limit", kind: paramInteger, doc: "Most rows to return (default: as many as fit)."},
		{name: "offset", kind: paramInteger, doc: "Rows to skip (default 0)."},
		stringParam("format", "Result format (default table).", resultFormats...),
	}
}

// scopedParam is a named filter's parameter, its doc opened by the views
// that carry the filter (family.carries) where not every view does.
func scopedParam(fam *family, name, doc string) param {
	var views []string
	for _, v := range fam.views {
		if slices.Contains(fam.carries()[v], name) {
			views = append(views, v)
		}
	}
	switch {
	case len(views) == len(fam.views):
		doc = strings.ToUpper(doc[:1]) + doc[1:]
	case len(views) == 1:
		doc = views[0] + " only: " + doc
	default:
		doc = andList(views) + ": " + doc
	}
	return stringParam(name, doc)
}

var (
	sourceParam  = stringParam("source", `Keep this source id only (e.g. "schwab"); a comma list keeps several.`)
	accountParam = stringParam("account", "Keep rows whose account name, id or nickname contains this (case-insensitive).")
)

// tools are the server's fourteen tools, in the order describe lists them.
func (s *mcpServer) tools() []toolSpec {
	return []toolSpec{
		{
			name: "holdings", title: "Holdings",
			description: "What is held and what it is worth, as of a date (default today): every account, plus the property, loans and private holdings recorded by hand (a loan is negative). " +
				"view=global: one row with cash_balance, positions_value and total_value (net worth); it has no per-account columns, so a filter needs another view. " +
				"sources: per institution. portfolios: per portfolio. accounts: per account with kind, tax wrapper and value. positions: every holding with symbol, quantity and value. " +
				`Example: {view: "accounts", as_of: "2025-12-31"}.`,
			params: s.rowParams(false,
				stringParam("view", "Which rows to return (default global).", holdingsFamily.views...),
				stringParam("as_of", "As-of date. Default today."),
				param{name: "with_cash", kind: paramBoolean, doc: "positions only: also list cash as one row per account and currency."},
				sourceParam, accountParam,
				scopedParam(holdingsFamily, "symbol", "keep only holdings whose symbol or name contains this text."),
				scopedParam(holdingsFamily, "asset_class", "keep only this asset class (e.g. public_equity, fixed_income, cash, real_estate, crypto)."),
				scopedParam(holdingsFamily, "tax_wrapper", "keep only this tax wrapper (e.g. roth_ira, 401k, 529, taxable_joint)."),
				missingBasisParam("positions only: ", ""),
			),
			family: holdingsFamily,
		},
		{
			name: "returns", title: "Returns",
			description: "How investments performed over a window (default: since the first snapshot), as twr and mwr %, after fees and taxes. " +
				"view=global: the whole portfolio. sources, portfolios: per entity, best-effort. accounts: per account, exact. " +
				"stated: a document's stated periods beside the engine's figures; it takes no method, period, annualize, netting, inception or currency. " +
				"One row per entity for the whole window unless period adds buckets. quality names the reason for every n/a. " +
				`Example: {view: "accounts", from: "2025", to: "2025"}.`,
			params: s.rowParams(true,
				stringParam("view", "Grain (default global); stated is a side view of the documents' periods.", returnsFamily.views...),
				stringParam("method", "twr, mwr or both (default both).", returnsMethods...),
				stringParam("period", "Bucket size (default total: one row per entity for the whole window).", returnsPeriods...),
				stringParam("annualize", "auto (default; only spans of a year or more), always, never.", returnsAnnualize...),
				param{name: "netting", kind: paramBoolean, doc: "Net internal transfers at coarse grains (default true)."},
				stringParam("inception", "full (default) or strict.", returnsInception...),
				stringParam("benchmark", benchmarkParamDoc("view=global only: the benchmark beside each row, over the days both sides have.")),
				sourceParam, accountParam,
			),
			family: returnsFamily,
		},
		{
			name: "gains", title: "Gains",
			description: "What was gained or lost, realized and unrealized, over a window (default: the last twelve months). " +
				"view=summary: one row: realized, unrealized at the start and end, change, gain = realized + change, share of value with a cost basis. " +
				"sources, portfolios, accounts: the same per entity; they add up to the summary. positions: per account and holding. " +
				"realized: each lot sold, as the tax document or statement states it or the lot engine rebuilt it. lots: the open lots at the window's end. " +
				"coverage: per account, where the figures are blind. check: the lot engine against the stated lots and cost bases. quality names every gap. " +
				`Example: {view: "realized", from: "2025", to: "2025"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", gainsFamily.views...),
				stringParam("period", "Bucket size on summary, sources, portfolios, accounts (default total: one row for the whole window).", reportPeriodNames...),
				stringParam("documents", "realized only: each sale once (primary, default) or every document's copy (all).", gainsDocuments...),
				param{name: "newest_first", kind: paramBoolean, doc: "realized only: newest first (default false: oldest first)."},
				sourceParam, accountParam,
				scopedParam(gainsFamily, "symbol", "keep only rows whose symbol or name contains this text."),
				scopedParam(gainsFamily, "asset_class", "keep only this asset class (e.g. public_equity, fixed_income, private_equity)."),
				scopedParam(gainsFamily, "term", "short or long."),
				scopedParam(gainsFamily, "tax_wrapper", "keep only this tax wrapper (e.g. taxable_joint, roth_ira)."),
				missingBasisParam("Not on coverage and check: ", "; quality then says basis_assumed_zero=N"),
			),
			family: gainsFamily,
		},
		{
			name: "exposure", title: "Exposure",
			description: "What is really owned once every fund is looked through to what it holds, as of a date (default today). " +
				"Views: classes; sectors (by class and sector); sizes (large, mid, small caps); countries (with region); currencies (the fiat currency, metal or coin a value moves with); " +
				"credit (debt by rating bucket, IG or HY); maturities (debt and cash by time to maturity); coupons (fixed, floating, inflation_linked, zero); cycles (equity by cyclical, sensitive, defensive); " +
				"situs (the country whose law each holding sits under; no fund looked through, so not exposure); securities (each security, direct and through funds); " +
				"issuers (stock, bonds and deposits together, largest first, with the running share); funds (each fund held, what describes it and how old that is); coverage (what the figures rest on and what nothing describes). " +
				"Every view sums to net worth: share_pct is of it; class_share_pct is within the row's dimension (its class on sectors and sizes; every row but (none) on credit, maturities, coupons, cycles), so those sum to 100. " +
				"Read quality and coverage before trusting a figure. " +
				"period with from/to: the view at the end of every month, quarter or year, in one call. " +
				`Example: {view: "countries", as_of: "2025-12-31"}; quarter by quarter: {view: "countries", period: "quarterly", from: "2025", to: "2025", filter: ["country = US"]}.`,
			params: s.rowParams(false, append([]param{
				stringParam("view", "Which breakdown (default classes).", exposureFamily.views...),
				stringParam("as_of", "As-of date. Default today."),
				stringParam("country", "countries and securities: the country a security counts in, risk (default) or domicile.",
					exposureCountryBases...),
				stringParam("benchmark", benchmarkParamDoc("the benchmark's weight of each row and the row's share less it; not on situs, funds or coverage, and no period.")),
			}, seriesParams(exposureSeriesViews)...)...),
			family: exposureFamily,
		},
		{
			name: "yield", title: "Yield",
			description: "What the holdings earn and how they move with interest rates, as of a date (default today). A rate belongs to a currency: an aggregate row is one entity in one currency, plus a value-weighted total (quality mixed). " +
				"Views: global (per currency and a total, with duration and dv01: the whole portfolio's rate risk); sources, portfolios, accounts; positions (per holding; columns=+coupon,maturity_date,price on request); " +
				"securities (each security, direct and through funds: a bond's coupon, maturity, price, implied yield and rating (columns=+rating), a stock's earnings yield, P/E, dividend yield, payout and growth); " +
				"funds (each fund: what its lines imply, its page's yields and duration, as of when); cash (each balance, term deposit and money-market fund with rate, benchmark and gap, worst paid first); " +
				"curve (DV01 per currency and tenor: " + strings.Join(canonical.RiskTenors, ", ") + "); coverage (what the figures rest on, what is missing, where each dividend's record day comes from). " +
				"income_yield: the next twelve months' projected income over value; implied_yield: what the price implies; dv01: the value's fall per basis point. Read quality. " +
				`Example: {view: "cash"}.`,
			params: s.rowParams(false, append(append([]param{
				stringParam("view", "Which rows to return (default global).", yieldFamily.views...),
				stringParam("as_of", "As-of date. Default today."),
			}, seriesParams(yieldSeriesViews)...),
				param{name: "converted", kind: paramBoolean, doc: andList(yieldAggregateViews) + ": one row per entity in the output currency (default false)."},
				scopedParam(yieldFamily, "symbol", "keep only rows whose symbol contains this text."),
			)...),
			family: yieldFamily,
		},
		{
			name: "transactions", title: "Transactions",
			description: "Every booked line over a window (default: the past 30 days): trades, dividends, interest, fees, deposits, withdrawals, transfers, with date, account, kind, symbol, amount and value. " +
				"Oldest first unless newest_first. " +
				`Example: {from: "2026-03", to: "2026-03", sort: "-value", limit: 10}.`,
			params: s.rowParams(true,
				param{name: "newest_first", kind: paramBoolean, doc: "Newest first (default false: oldest first)."},
				sourceParam, accountParam,
				stringParam("kind", "Keep one transaction kind (e.g. buy, sell, dividend, interest, fee, deposit, withdrawal, purchase)."),
				stringParam("symbol", "Keep only rows whose symbol or instrument contains this text."),
			),
			family: transactionsFamily,
		},
		{
			name: "spending", title: "Spending",
			description: "What was spent over a window (default: the last twelve months). " +
				"view=summary: per period. categories: per period and category, with its share. transactions: the lines with merchant and category. " +
				"spend and refunds are positive; net_spend = spend - refunds. Card payments, transfers and mortgage payments are never spending: they are in cashflow. " +
				`Example: {view: "categories", from: "2025", to: "2025"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", spendingFamily.views...),
				stringParam("period", "Bucket size (default total: one row per category for the whole window).", reportPeriodNames...),
				stringParam("level", "Category vocabulary of the categories view: primary (default, broad groups) or detailed.", categoryLevels...),
				sourceParam, accountParam,
				stringParam("category", `Keep categories whose label or id contains this (e.g. "groceries"); implies level=detailed.`),
				scopedParam(spendingFamily, "merchant", "keep only rows whose merchant contains this text."),
			),
			family: spendingFamily,
		},
		{
			name: "income", title: "Income",
			description: "What was received over a window (default: the last twelve months): salary, dividends, interest, rent, refunds, rewards; and what the holdings are expected to pay. " +
				"view=summary: per period. types: per period and type, with its share. transactions: the lines with payer and type. Gross as booked; net_income = income - reversals. " +
				"The forward views take a window that may reach into the future (default: the months after this one; a lone from is its own span): " +
				"projection (per month: received, expected, each type, withheld, and basis, what the expected part rests on; the view for how much is expected in a month or a year); " +
				"calendar (each expected payment and each principal a maturity returns, dated); " +
				"sources, portfolios, accounts, positions (received, expected, total, last_year and next_12m per entity; positions lists every holding). " +
				"rates, maturing and shift set what the expected income assumes. " +
				`Example: {view: "projection", from: "2026", to: "2026"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", incomeFamily.views...),
				stringParam("period", "Bucket size (default total; monthly on the forward views).", reportPeriodNames...),
				stringParam("level", "Type vocabulary of the types view: detailed (default) or primary.", categoryLevels...),
				stringParam("rates", "Forward views: constant (today's rates) or forward (floating rates along the forward curve). Default "+string(yield.DefaultAssumptions.Path)+".", pathNames()...),
				stringParam("maturing", "Forward views: what a maturing principal earns: cash (its account's rate), idle (nothing) or roll (the same holding again). Default "+string(yield.DefaultAssumptions.Rule)+".", ruleNames()...),
				param{name: "shift", kind: paramInteger, doc: "Forward views: parallel shift of the floating rates in basis points (default 0)."},
				sourceParam, accountParam,
				stringParam("type", `Keep income types whose label or id contains this (e.g. "dividend", "salary").`),
				scopedParam(incomeFamily, "payer", "keep only rows whose payer contains this text."),
				scopedParam(incomeFamily, "symbol", "keep only rows whose symbol contains this text."),
				scopedParam(incomeFamily, "kind", "income or principal."),
			),
			family: incomeFamily,
		},
		{
			name: "cashflow", title: "Cash flow",
			description: "Where the household's cash came from and went over a window (default: the last twelve months), as a cash flow statement; positive is cash arriving. " +
				"view=summary: per period: operating_in, operating_out, investing, financing, vehicles (all retirement, education and health plans together), net_cash_flow. " +
				"flows: per period and node (section, class, group); one plan or the mortgage is flows with a class filter (Mortgage sits under financing). " +
				"sankey: the diagram's edges. transactions: the lines. coverage: per account and period, how far the statement can be trusted (read status first). " +
				"projection: one row per month ahead (default: the horizon after this month; period monthly, quarterly or annual): investment_income, principal_returned and vehicles_income as the income projection expects them, " +
				"other_income, operating_out and vehicles as trailing monthly averages, debt_service (each mortgage to its maturity), net and quality. " +
				`Example: {view: "flows", from: "2025", to: "2025", level: "class"}.`,
			params: s.rowParams(true,
				stringParam("view", "Which rows to return (default summary).", cashflowFamily.views...),
				stringParam("period", "Bucket size (default total; monthly on projection, which takes no total). Not for sankey.", reportPeriodNames...),
				stringParam("level", "Node grain of flows and sankey: section, class or group (default group).", cashflowLevels...),
				stringParam("investing", "whole (default): investing as one node; class: one node per asset class.", cashflowInvestingGrains...),
				stringParam("rates", "projection: constant or forward floating rates. Default "+string(yield.DefaultAssumptions.Path)+".", pathNames()...),
				stringParam("maturing", "projection: what a maturing principal earns: cash, idle or roll (net then leaves principal_returned out). Default "+string(yield.DefaultAssumptions.Rule)+".", ruleNames()...),
				param{name: "shift", kind: paramInteger, doc: "projection: parallel shift of the floating rates in basis points (default 0)."},
				sourceParam, accountParam,
				stringParam("section", "Keep one section: operating_in, operating_out, investing, financing, vehicles, cash."),
				stringParam("class", `Keep classes whose label contains this (e.g. "Mortgage", "Retirement").`),
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
			description: "Help. Without topic: what this server is, the configured sources and their latest data dates, the default currency, the character budget, and which tool answers which question. " +
				"topic=<tool name>: that tool's views, every column it can show, its filters and conventions. topic=" + orList(describeTopics) + ": the conventions.",
			params: []param{stringParam("topic", "A tool name, or "+strings.Join(describeTopics, ", ")+". Default: the overview.")},
			run:    s.describe,
		},
	}
}

// ---- families ------------------------------------------------------------

// reportFamilies are the report tools and commands by name, in describe's
// order: where a column a view lacks is printed instead, and the family
// a CLI condition binds with.
var reportFamilies = []struct {
	name string
	fam  *family
}{
	{"holdings", holdingsFamily}, {"returns", returnsFamily}, {"gains", gainsFamily}, {"exposure", exposureFamily},
	{"yield", yieldFamily}, {"transactions", transactionsFamily}, {"spending", spendingFamily}, {"income", incomeFamily},
	{"cashflow", cashflowFamily},
}

var holdingsFamily = &family{
	views: holdingsViews,
	// A source's or a portfolio's tax_wrapper is its accounts' common
	// one, blank where they differ: a filter there would drop the rest.
	excludes: map[string][]string{"sources": {"tax_wrapper"}, "portfolios": {"tax_wrapper"}},
	adds:     func(string) totalRule { return balanceTotal },
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
// read a cost basis; scope opens its doc (the views it belongs to), and
// note follows the zero reading inside its parentheses.
func missingBasisParam(scope, note string) param {
	return stringParam("missing_basis", scope+"a missing cost basis is blank (ignore, the default) or counts as 0 (zero"+note+"); "+
		"the config's lots.missing_basis sets the default.",
		stringsOf(lots.MissingBasisReadings)...)
}

// seriesParams are the period, from and to of a tool whose views take a
// series, as the CLI's --period and FROM [TO] do.
func seriesParams(views []string) []param {
	return []param{
		stringParam("period", andList(views)+": a series at the end of each period of from/to (default: the last twelve months).", seriesPeriods...),
		stringParam("from", "With period: the series' start ("+dateDoc+")."),
		stringParam("to", "With period: the series' end, same forms. Default today."),
	}
}

// seriesArg reads a call's period, from and to into req.series with the
// CLI's checks (checkSeriesOptions) and window (seriesWindow); without a
// period, from and to are refused.
func seriesArg(tool string, views []string, a *toolArgs, req *request, notes *[]string) error {
	if !a.has("period") {
		if a.has("from") || a.has("to") {
			return fmt.Errorf("%s: from and to belong to a series: give a period too", tool)
		}
		return nil
	}
	if err := checkSeriesOptions(tool, req.view, views, a.has, func(name string) string { return name }); err != nil {
		return err
	}
	var err error
	req.series, err = seriesWindow(windowArgs(a.str("from"), a.str("to")), a.str("period"), time.Unix(req.asOf, 0))
	switch {
	case errors.Is(err, errSeriesAfterToday), errors.Is(err, errFromAfterTo) && !a.has("to"):
		// A lone from after today: the window runs to today.
		return fmt.Errorf("%s: %w", tool, errSeriesAfterToday)
	case errors.Is(err, errFromAfterTo):
		return fmt.Errorf("%s: from %q is after to %q", tool, a.str("from"), a.str("to"))
	case err != nil:
		return fmt.Errorf("%s: %v; dates are %s", tool, err, dateDoc)
	}
	*notes = append(*notes, "series "+a.str("period"))
	return nil
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
	side:  map[string]bool{"stated": true},
	// global's silver_source is the whole portfolio's, not one source;
	// on accounts the entity is the account.
	excludes: map[string][]string{"global": {"source"}},
	keyAlias: map[string]map[string]string{"accounts": {"account": "entity"}},
	// On returns, "value" means the return itself; on the stated view,
	// which carries no return, the engine's ending value.
	principal: func(req request) string {
		switch {
		case req.view == "stated":
			return "end_value"
		case req.method == "mwr":
			return "mwr"
		}
		return "twr"
	},
	window:   sinceInception,
	currency: true,
	narrow:   "from/to or a filter",
	prepare: func(a *toolArgs, req *request, _ *[]string) error {
		if req.view == "stated" {
			// The document fixes the periods and the currency.
			for _, name := range []string{"currency", "method", "period", "annualize", "netting", "inception", "benchmark"} {
				if a.has(name) {
					return fmt.Errorf("returns: stated takes no %s — the periods and the currency are the document's", name)
				}
			}
			return nil
		}
		req.method = orDefault(a.str("method"), "both")
		req.period = orDefault(a.str("period"), "total")
		req.annualize = orDefault(a.str("annualize"), "auto")
		req.inception = orDefault(a.str("inception"), "full")
		req.netting = a.flag("netting", true)
		req.benchmark = a.str("benchmark")
		return checkReturnsBenchmark(req.view, req.benchmark)
	},
	build: func(cfg *config.Config, req request, _ *toolArgs) *report {
		if req.view == "stated" {
			return returnsStatedReport(req, cfg)
		}
		return returnsReport(req, cfg)
	},
	header: func(req request) []string {
		if req.view == "stated" {
			return nil
		}
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
		if req.benchmark != "" {
			h = append(h, "benchmark="+req.benchmark)
		}
		return h
	},
}

var exposureFamily = &family{
	views:     exposureViews,
	adds:      byRule(map[string]totalRule{"coverage": noTotal}, balanceTotal),
	principal: func(request) string { return "value" },
	window:    asOfDate,
	currency:  true,
	narrow:    "a coarser view",
	prepare: func(a *toolArgs, req *request, notes *[]string) error {
		req.country = exposureCountryBases[0]
		if a.has("country") {
			if !slices.Contains(exposureCountryViews, req.view) {
				return errCountryView("country")
			}
			req.country = a.str("country") // the enum admits exposureCountryBases alone
		}
		if err := seriesArg("exposure", exposureSeriesViews, a, req, notes); err != nil {
			return err
		}
		req.benchmark = a.str("benchmark")
		return checkExposureBenchmark(req.view, req.benchmark, req.country, req.series)
	},
	build: func(_ *config.Config, req request, _ *toolArgs) *report { return exposureReport(req) },
	header: func(req request) []string {
		var h []string
		if req.country == "domicile" {
			h = append(h, "country by domicile")
		}
		if req.benchmark != "" {
			h = append(h, "benchmark="+req.benchmark)
		}
		return h
	},
}

var gainsFamily = &family{
	views: gainsViews,
	adds:  byRule(map[string]totalRule{"lots": balanceTotal, "coverage": noTotal, "check": noTotal}, flowTotal),
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
	adds:      func(string) totalRule { return flowTotal },
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
	views:     spendingViews,
	adds:      func(string) totalRule { return flowTotal },
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
	// kind is income or principal on the calendar; on transactions the
	// column is the booked kind.
	excludes: map[string][]string{"transactions": {"kind"}},
	adds:     func(string) totalRule { return flowTotal },
	principal: byView(map[string]string{"summary": "net_income", "types": "net_income", "transactions": "value",
		"projection": "total", "calendar": "value", "sources": "total", "portfolios": "total", "accounts": "total",
		"positions": "total"}),
	window:   trailingYear,
	forward:  isForwardView,
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
		if forward {
			req.assumptions = assumptionArgs(a)
		}
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
		return append(h, assumptionsHeader(req.assumptions)...)
	},
}

var yieldFamily = &family{
	views: yieldViews,
	// The aggregate views carry a total row of their own.
	adds: byRule(map[string]totalRule{"positions": balanceTotal, "securities": balanceTotal, "funds": balanceTotal,
		"cash": balanceTotal}, noTotal),
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
		return seriesArg("yield", yieldSeriesViews, a, req, notes)
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
	// A flow node nets the nodes under it, and coverage measures gaps.
	adds: byRule(map[string]totalRule{"flows": noTotal, "sankey": noTotal, "coverage": noTotal}, flowTotal),
	principal: byView(map[string]string{
		"summary": "net_cash_flow", "flows": "net", "sankey": "value", "transactions": "value", "coverage": "gap",
		"projection": "net",
	}),
	window:   trailingYear,
	forward:  func(view string) bool { return view == "projection" },
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
		if req.view == "projection" {
			req.period = orDefault(a.str("period"), "monthly")
		}
		if err := checkCashflowPlanOptions(req.view, req.period, a.has, func(name string) string { return name }); err != nil {
			return err
		}
		if req.view != "projection" {
			return nil
		}
		if err := checkPlanWindow(req.from, req.to, time.Unix(req.asOf, 0)); err != nil {
			return fmt.Errorf("cashflow: %w; dates are %s", err, dateDoc)
		}
		req.assumptions = assumptionArgs(a)
		return nil
	},
	build: func(cfg *config.Config, req request, _ *toolArgs) *report {
		req.lookback = cfg.CashflowLookback()
		return cashflowReport(req)
	},
	header: func(req request) []string {
		var h []string
		switch req.view {
		case "summary", "coverage":
			h = append(h, "period="+req.period)
		case "flows":
			h = append(h, "period="+req.period, "level="+req.level)
		case "sankey":
			h = append(h, "level="+req.level)
		case "projection":
			h = append(append(h, "period="+req.period), assumptionsHeader(req.assumptions)...)
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
	// source asks for one source's detail block; kind is the source's.
	excludes: map[string][]string{"": {"source", "kind"}},
	narrow:   "search",
	prepare:  noOptions,
	build: func(cfg *config.Config, _ request, a *toolArgs) *report {
		return statusReport(cfg, a.flag("verbose", false))
	},
}

var snapshotsFamily = &family{
	narrow:  "source or latest_only",
	prepare: noOptions,
	build: func(_ *config.Config, _ request, a *toolArgs) *report {
		return snapshotsReport(a.flag("latest_only", false))
	},
}

var resolutionsFamily = &family{
	narrow:  "source or search",
	prepare: noOptions,
	build:   func(*config.Config, request, *toolArgs) *report { return resolutionsReport("") },
}

var categorizationsFamily = &family{
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

// byRule is a total rule per view, def for the views it does not name.
func byRule(m map[string]totalRule, def totalRule) func(string) totalRule {
	return func(view string) totalRule {
		if r, ok := m[view]; ok {
			return r
		}
		return def
	}
}

// totalRule is the family's rule for view.
func (fam *family) totalRule(view string) totalRule {
	if fam.adds == nil {
		return noTotal
	}
	return fam.adds(view)
}

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

// runFamily is every report tool's call: read the conditions, settle
// the view, the dates and the options, check the columns, the
// conditions and the sort before gold is touched, fetch the rows on a
// read-only handle held for the fetch alone, then filter, sort, page
// and render.
func (s *mcpServer) runFamily(ctx context.Context, spec toolSpec, a *toolArgs) (toolOutput, error) {
	tool, fam := spec.name, spec.family
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
	front := filterFront{prefix: tool + ":"}
	raws, badConds := parseConditions(a.strs("filter"), front)
	req, notes, err := s.settle(tool, fam, cfg, a, named, raws)
	if err != nil {
		return toolOutput{}, err
	}
	rep := fam.build(cfg, req, a)
	lookup := fam.lookup(rep, req)
	sh, err := s.shape(tool, rep, lookup, a)
	if err != nil {
		return toolOutput{}, err
	}
	target := filterTarget{view: req.view, lookup: lookup, privacy: s.privacy, now: s.now(), front: front,
		elsewhere: func(col string) string { return columnElsewhere(cfg, tool, req.view, col, false) }}
	conds, err := target.bind(raws, badConds)
	if err != nil {
		return toolOutput{}, err
	}
	for _, c := range conds {
		if c.note != "" {
			notes = append(notes, c.note)
		}
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
	before := rs.filter(filters, a.str("search"))
	idx, _ := keepRows(rs.cells, before, conds)
	if sh.sortCol >= 0 {
		rs.sortRows(idx, sh.sortCol, sh.desc)
	}

	head, meta := s.header(tool, fam, rep, req, len(idx), sh, canonicalForms(conds), append(notes, sh.notes...))
	p := page{idx: idx, offset: sh.offset, limit: sh.limit, budget: sh.budget, narrow: narrowAdvice(spec, req.view)}
	switch {
	case len(idx) == 0:
		p.empty = emptyNote(rs, fam, req.view, named, a, before, conds)
	case !s.privacy && (len(named) > 0 || len(conds) > 0 || a.has("search")):
		p.totals = totals(rs, idx, sh.cols, req.currency, fam.totalRule(req.view))
	}
	return render(rs, p, sh.cols, orDefault(a.str("format"), formatTable), head, meta), nil
}

// narrowAdvice is the paging line's advice for view: the family's, the
// named filters the view carries, and conditions on its columns.
func narrowAdvice(spec toolSpec, view string) string {
	advice := spec.family.narrow
	if fs := spec.carriedParams(view); len(fs) > 0 && strings.Contains(advice, "a filter") {
		advice = strings.Replace(advice, "a filter", "a filter ("+strings.Join(fs, ", ")+")", 1)
	}
	return advice + ", or filter on its columns"
}

// carriedParams are the named filters view carries that the tool has a
// parameter for.
func (t toolSpec) carriedParams(view string) []string {
	var out []string
	for _, f := range t.family.carries()[view] {
		if slices.ContainsFunc(t.params, func(p param) bool { return p.name == f }) {
			out = append(out, f)
		}
	}
	return out
}

// lookup resolves the names a call sends for rep's columns.
func (fam *family) lookup(rep *report, req request) columnLookup {
	l := columnLookup{cols: rep.columns, currency: req.currency}
	if fam.principal != nil {
		l.principal = fam.principal(req)
	}
	return l
}

// ladder is the family's views without its side views: the coarse to
// fine order the walks follow.
func (fam *family) ladder() []string {
	var out []string
	for _, v := range fam.views {
		if !fam.side[v] {
			out = append(out, v)
		}
	}
	return out
}

// walk is the views escalate may settle view on: the ladder, or a side
// view alone, which no filter moves off.
func (fam *family) walk(view string) []string {
	if fam.side[view] {
		return []string{view}
	}
	return fam.ladder()
}

// settle turns a call's arguments into the report request: the view
// the filters and the conditions need, the currency, the dates, and the
// family's options. notes are what it chose for the model, for the
// header.
func (s *mcpServer) settle(tool string, fam *family, cfg *config.Config, a *toolArgs, named []string, raws []rawCond) (request, []string, error) {
	var notes []string
	moved := "" // the note of a move off the requested view
	view := a.str("view")
	if fam.views != nil {
		if view == "" {
			view = fam.views[0]
		}
		carries := fam.carries()
		takesNamed := func(v string) bool { return carriesAll(carries, v, named) }
		settled, ok := escalate(fam.walk(view), func(v string) bool {
			return takesNamed(v) && printsAll(cfg, fam, v, raws, a.has("period"))
		}, view)
		if !ok {
			// No view prints every condition's column: settle for the
			// named filters, and the binding says which column is where.
			if settled, ok = escalate(fam.walk(view), takesNamed, view); !ok {
				return request{}, nil, filterViewError(tool, fam, view, named)
			}
		}
		if settled != view {
			moved = fmt.Sprintf("view %s chosen for the %s", settled, escalationReason(named, raws, view, cfg, fam, a.has("period")))
			notes = append(notes, moved)
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
	if err := resolveDates(fam, a, s.now(), cfg.YieldHorizon(), &req); err != nil {
		return request{}, nil, fmt.Errorf("%s: %v", tool, err)
	}
	if err := fam.prepare(a, &req, &notes); err != nil {
		if moved != "" {
			// Say what moved the call, or the refusal names a view
			// that was never asked for.
			return request{}, nil, fmt.Errorf("%v (%s)", err, moved)
		}
		return request{}, nil, err
	}
	return req, notes, nil
}

// printsAll reports whether view prints the column of every condition.
func printsAll(cfg *config.Config, fam *family, view string, raws []rawCond, series bool) bool {
	if len(raws) == 0 {
		return true
	}
	lookup := fam.defaultLookup(cfg, view, series)
	for _, r := range raws {
		if _, _, err := lookup.find(r.col); err != nil {
			return false
		}
	}
	return true
}

// escalationReason names what moved the call off view: the named
// filters, and the conditions whose column view lacks.
func escalationReason(named []string, raws []rawCond, view string, cfg *config.Config, fam *family, series bool) string {
	var parts []string
	if len(named) > 0 && !carriesAll(fam.carries(), view, named) {
		parts = append(parts, strings.Join(named, ", ")+" filter")
	}
	var cols []string
	if len(raws) > 0 {
		lookup := fam.defaultLookup(cfg, view, series)
		for _, r := range raws {
			if _, _, err := lookup.find(r.col); err != nil && !slices.Contains(cols, r.col) {
				cols = append(cols, r.col)
			}
		}
	}
	if len(cols) > 0 {
		parts = append(parts, strings.Join(cols, ", ")+" "+plural(len(cols), "condition", "conditions"))
	}
	return strings.Join(parts, " and ")
}

// resultShape is how a call wants its rows: the columns, the sort and
// the page, with notes for what a name resolved to.
type resultShape struct {
	cols    []int
	sortCol int // -1 for the report's own order
	desc    bool
	limit   int // 0 for no row bound
	budget  int // max_chars
	offset  int
	notes   []string
}

// shape resolves the columns, sort, limit and offset parameters
// against the report, before gold is opened, so a misspelt name costs
// no query.
func (s *mcpServer) shape(tool string, rep *report, lookup columnLookup, a *toolArgs) (resultShape, error) {
	sh := resultShape{sortCol: -1}
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
	if sh.limit, sh.budget, sh.offset, err = s.paging(a); err != nil {
		return sh, fmt.Errorf("%s: %v", tool, err)
	}
	return sh, nil
}

// header builds the result's first line and the json result's fields
// beside the rows.
func (s *mcpServer) header(tool string, fam *family, rep *report, req request, rows int, sh resultShape, conds, notes []string) (resultHeader, resultMeta) {
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
			if req.series[0].at < gold.SecondsPerDay*366 {
				// A lone to: the series begins with the data.
				meta.from = windowStart(0)
			}
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
	case tool == "returns" && req.view == "stated":
		meta.currency = ""
		head.add("each period in the currency its document states")
	case req.currency != "":
		head.add("%s", req.currency)
	}
	if fam.header != nil {
		head.parts = append(head.parts, fam.header(req)...)
	}
	head.parts = append(head.parts, conds...)
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

// resolveDates reads from/to or as_of under the family's window rule,
// with the CLI's defaults and the CLI's date grammar: from snaps to the
// start of its unit, to and as_of to the end.
func resolveDates(fam *family, a *toolArgs, now time.Time, horizon int, req *request) error {
	from, to := a.str("from"), a.str("to")
	args := windowArgs(from, to)
	var err error
	switch fam.window {
	case asOfDate:
		req.asOf, err = parseAsOf(a.str("as_of"), now)
		if err != nil {
			return fmt.Errorf("as_of %q is not a date; use %s", a.str("as_of"), dateDoc)
		}
		return nil
	case trailingYear:
		// The forward views (family.forward) compute today's holdings,
		// over a window that may reach into the future.
		req.asOf = anchorToDay(now.UTC(), true).Unix()
		if fam.forward != nil && fam.forward(req.view) {
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
	if errors.Is(err, errFromAfterTo) {
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

// paging reads limit, max_chars and offset. Without a limit the page
// is as long as the budget holds; max_chars defaults to the server's.
func (s *mcpServer) paging(a *toolArgs) (limit, budget, offset int, err error) {
	if n, ok := a.integer("limit"); ok {
		if n < 0 {
			return 0, 0, 0, fmt.Errorf("limit must be 0 or more, not %d; leave it out (or 0) for as many rows as max_chars holds", n)
		}
		limit = n
	}
	budget = s.chars
	if n, ok := a.integer("max_chars"); ok {
		if n < 1 {
			return 0, 0, 0, fmt.Errorf("max_chars must be 1 or more, not %d; it has no unlimited value", n)
		}
		budget = n
	}
	if n, ok := a.integer("offset"); ok {
		if n < 0 {
			return 0, 0, 0, fmt.Errorf("offset must be 0 or more, not %d", n)
		}
		offset = n
	}
	return limit, budget, offset, nil
}

// filterViewError says why the requested view and every finer one lack
// the filters, and where they are: the view to call when one takes
// them all, else which views carry each.
func filterViewError(tool string, fam *family, view string, named []string) error {
	for _, v := range fam.ladder() {
		if carriesAll(fam.carries(), v, named) {
			return fmt.Errorf("%s: the %s view has no %s filter; the %s view has: call view=%s, or use search",
				tool, view, strings.Join(named, ", "), v, v)
		}
	}
	var where []string
	for _, f := range named {
		var vs []string
		for _, v := range fam.views {
			if slices.Contains(fam.carries()[v], f) {
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

// emptyNote explains a result with no rows: which condition emptied
// it, and for each named filter the values its column takes, so a
// guessed section or class is corrected by the next call rather than
// concluded from. before are the rows the named filters and search
// kept, which the conditions then emptied.
func emptyNote(rs *resultSet, fam *family, view string, named []string, a *toolArgs, before []int, conds []condition) string {
	var b strings.Builder
	switch {
	case len(conds) > 0 && len(before) > 0:
		b.WriteString("(no rows: " + emptiedNote(rs.cells, before, conds) + ")")
	case len(named) > 0 || a.has("search") || len(conds) > 0:
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
	for _, v := range fam.ladder() {
		if after && carriesAll(fam.carries(), v, named) {
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
		case paramStrings:
			prop["type"] = "array"
			prop["items"] = map[string]any{"type": "string"}
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
