package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

// What the model reads before and around its calls: the server
// instructions, the describe topics, and the same texts as resources.
// The instructions were tuned against small local models; each
// convention below removed a class of wrong answers or round trips.

// instructionsBody follows the date line. It names the data's scope
// (without it a small model answers "how much is the house worth" with
// no call), picks the tool by the question, and states the conventions
// a model otherwise guesses at.
const instructionsBody = `wealthdb holds a household's complete financial picture in one local, read-only database: every bank, card, brokerage, pension and crypto account, plus the property, loans and private holdings recorded by hand. A house, a mortgage or a venture fund is an account like any other; a loan or a card balance is a negative value.

Pick the tool by the question:
- what is held, where, and what it is worth (today or on a date); net worth; one account or one institution -> holdings
- how investments performed, as a return % -> returns
- what was gained or lost (P&L), realized or unrealized; a year's realized gains; a holding's cost basis -> gains
- the individual booked lines (a trade, a deposit, a transfer, the largest transactions) -> transactions; totals of dividends, interest, salary or spending are income and spending
- what was spent, on what -> spending
- what was received, from whom -> income
- where the household's cash came from and went; mortgage and loan payments; money into retirement, education or health plans -> cashflow (view=flows for one class)
- is the data current -> status; which dates have data -> snapshots
- help on a tool, its columns, or a term -> describe

Conventions:
- Dates: "2025" is that year, "2025-06" that month, "2025-06-15" that day; also "today", "yesterday", "last month", "last year". from snaps to the start, to and as_of to the end.
- Every parameter is optional; each description states its default. A filter such as account, category, type or class picks the view that has that column, and the result header says which view ran.
- One call usually answers the question: a result is one row per entity, category or type for the whole window unless you ask for monthly, quarterly or annual buckets.
- Each result starts with a header line that states the resolved window or as-of date, the currency and the row count; check it matches the question. Results are capped; the last line says how to get more or narrow.
- Money columns are plain decimals in the currency named in the header (value_CHF). A blank cell means no value, not zero. In line views money leaving is negative; sort "-value" puts the largest amounts first whatever their sign.
- In returns and gains, read the quality column: it names the reason for every n/a or gap. Returns are not additive across views; gains are.
- A holding before a source's first snapshot is missing history, not zero; snapshots shows where data begins.`

const privacyNotice = "This endpoint redacts amounts, quantities, account numbers and the names taken off statements (merchants, payers, narratives); account names, holdings, categories and shares stay visible."

// instructions open with today's date: without it a small model reads
// "last year" as two years ago. Every result header also states its
// resolved dates, for a session that outlives the day.
func (s *mcpServer) instructions(today time.Time) string {
	text := "Today is " + today.UTC().Format("2006-01-02") + " (UTC).\n\n" + instructionsBody
	if s.privacy {
		text += "\n\n" + privacyNotice
	}
	return text
}

// describeTopics are the conventions describe explains beside the
// tools, in listing order.
var describeTopics = []string{"dates", "privacy", "quality", "glossary"}

// describe is the help tool: the overview, one tool's long help, or a
// convention.
func (s *mcpServer) describe(ctx context.Context, a *toolArgs) (toolOutput, error) {
	topic := strings.ToLower(strings.TrimSpace(a.str("topic")))
	text, err := s.describeText(ctx, topic)
	return toolOutput{text: text}, err
}

func (s *mcpServer) describeText(ctx context.Context, topic string) (string, error) {
	switch topic {
	case "", "overview", "help", "wealthdb":
		return s.overview(ctx)
	case "dates":
		return datesHelp, nil
	case "privacy":
		return s.privacyHelp(), nil
	case "quality":
		return qualityHelp, nil
	case "glossary":
		return s.glossary(ctx)
	}
	for _, t := range s.tools() {
		if t.name == topic && t.family != nil {
			return s.toolHelp(t)
		}
	}
	return "", fmt.Errorf("describe: unknown topic %q; topics are the tool names (%s) and %s",
		topic, strings.Join(s.reportToolNames(), ", "), orList(describeTopics))
}

func (s *mcpServer) reportToolNames() []string {
	var names []string
	for _, t := range s.tools() {
		if t.family != nil {
			names = append(names, t.name)
		}
	}
	return names
}

// overview is describe without a topic: the server, the sources and
// how fresh each is, the limits, and the pick-a-tool table.
func (s *mcpServer) overview(ctx context.Context) (string, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "wealthdb %s, MCP server. ", version.String())
	if s.privacy {
		b.WriteString(privacyNotice + "\n")
	} else {
		b.WriteString("This endpoint serves full data.\n")
	}
	fmt.Fprintf(&b, "Default currency: %s. Rows per result: %d by default; limit=0 returns every row", cfg.DefaultCurrency, s.rows)
	if s.maxRows > 0 {
		fmt.Fprintf(&b, ", up to %d", s.maxRows)
	}
	b.WriteString(".\n\nSources (id · kind · latest snapshot):\n")
	err = s.withGold(ctx, cfg, func(db *sql.DB) error {
		for _, src := range cfg.SilverSources {
			latest := "not loaded"
			if st, err := gold.StatusForSource(ctx, db, src.ID, false); err == nil && st != nil && st.LatestSnapshotAt >= 0 {
				latest = formatDate(st.LatestSnapshotAt)
			}
			fmt.Fprintf(&b, "- %s · %s · %s\n", src.ID, src.Kind, latest)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(&b, "(the database cannot be read: %s)\n", s.scrubPaths(err.Error()))
	}
	fmt.Fprintf(&b, "\n%s\n\nMore: describe(topic=...) with a tool name (%s) or %s.",
		instructionsBody, strings.Join(s.reportToolNames(), ", "), orList(describeTopics))
	return b.String(), nil
}

// toolHelp is one tool's long help: its description, its parameters
// with their defaults, and for each view every column it can show
// (from the registries, so never a second copy) and the filters it
// takes.
func (s *mcpServer) toolHelp(t toolSpec) (string, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\nParameters (all optional):\n", t.name, t.description)
	for _, p := range t.params {
		doc := p.doc
		if len(p.enum) > 0 {
			doc = strings.Join(p.enum, " | ") + ". " + doc
		}
		fmt.Fprintf(&b, "  %-12s %s\n", p.name, doc)
	}
	fam := t.family
	views := fam.views
	if views == nil {
		views = []string{""}
	}
	b.WriteString("\nColumns (* = shown by default; columns=all shows every one):\n")
	empty := &toolArgs{values: map[string]any{}}
	for _, v := range views {
		req := request{view: v, currency: cfg.DefaultCurrency, period: "total", method: "both",
			annualize: "auto", inception: "full", netting: true, investing: "whole"}
		req.level = map[string]string{"cashflow": "group", "income": "detailed"}[t.name]
		if req.level == "" {
			req.level = "primary"
		}
		rep := fam.build(cfg, req, empty)
		var cols []string
		for _, c := range rep.columns {
			name := c.header
			if contains(rep.defaults, c.name) {
				name += "*"
			}
			cols = append(cols, name)
		}
		label := "  "
		if v != "" {
			label = "  " + v + ": "
		}
		b.WriteString(label + strings.Join(cols, ", ") + "\n")
	}
	if fam.views != nil {
		b.WriteString("\nFilters each view takes (a filter the view lacks moves the call to the first finer view that has it):\n")
		for _, v := range fam.views {
			fs := fam.carries[v]
			if len(fs) == 0 {
				fs = []string{"none"}
			}
			fmt.Fprintf(&b, "  %s: %s\n", v, strings.Join(fs, ", "))
		}
	}
	if note := toolNotes[t.name]; note != "" {
		b.WriteString("\n" + note + "\n")
	}
	if s.privacy {
		b.WriteString("\n" + privacyNotice + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// toolNotes are the conventions of each tool that its columns do not
// say, with a second example call.
var toolNotes = map[string]string{
	"holdings": `Totals reconcile: global = sum of sources = sum of portfolios = sum of accounts = positions with with_cash. A loan or a card balance is negative. positions_value, cash_balance and total_value are in each account's own currency; the columns with a currency suffix are converted.
Example: {view: "accounts", tax_wrapper: "roth_ira"}.`,
	"returns": `The quality column explains every n/a (describe topic=quality). accounts is exact; the coarser views are best-effort, and returns do not add up across views.
Example: {view: "sources", from: "2024", to: "2025", period: "annual", method: "twr"}.`,
	"gains": `gain = realized + unrealized_change, the price gain on what is held; income, fees and taxes are in returns. Realized is what the sale's documents state; unrealized is the clean value (market value less accrued interest) less the cost basis. basis_stamp says which notion of cost basis a figure is. summary, sources, portfolios and accounts add up. The quality column names every gap (describe topic=quality); the coverage view says per account where the figures are blind.
Example: {view: "accounts", from: "2025", to: "2025", tax_wrapper: "taxable_joint"}.`,
	"transactions": `Money leaving an account is negative. kind is the booked kind: buy, sell, dividend, interest, fee, tax, deposit, withdrawal, purchase, refund, card_payment and others.
Example: {from: "last month", to: "last month", kind: "dividend"}.`,
	"spending": `spend and refunds are positive; net_spend = spend - refunds. The category rows of a period sum to its summary row. (uncategorized) is what no rule or model has placed yet. The transactions view keeps the ledger sign: a purchase is negative there.
Example: {view: "transactions", from: "2025-03", to: "2025-03", sort: "-value", limit: 10}.`,
	"income": `income and reversals are positive; net_income = income - reversals. The type rows of a period sum to its summary row.
Example: {view: "transactions", from: "2025", to: "2025", type: "dividend", sort: "-value"}.`,
	"cashflow": `The four sections sum to net_cash_flow. vehicles is every plan together; for one plan read view=flows at level=class. The mortgage is under financing. A node is a net: inflow and outflow beside it are the gross lines under it.
Example: {view: "flows", from: "2025", to: "2025", class: "Retirement"}.`,
	"status":          `new_data=yes means a load would bring newer data in. state "not loaded" means gold holds nothing from the source yet.`,
	"snapshots":       `One row per day with data; snapshots counts the snapshots taken that day.`,
	"resolutions":     `model_name manual-override marks a ticker set in the config; any other name is the model that answered.`,
	"categorizations": `A verdict applies to every transaction whose signature matches, at any account. The signature and name columns are text taken off statements.`,
}

const datesHelp = `Dates: "2025" is that year, "2025-06" that month, "2025-06-15" that day; "today", "yesterday", "last month", "last year" and "3 months ago" also work (English, UTC).
from snaps to the start of its unit, to and as_of to the end: from="2025", to="2025" is the calendar year; from="2025-06" alone runs to today; to alone runs from the start of the data.
Defaults: holdings as of today; transactions the past 30 days; spending, income, cashflow and gains the last twelve months; returns since the first snapshot.
Every result header states the dates it used, as ISO dates.`

const qualityHelp = `The returns quality column gives the reason for every n/a and tags every approximation. Common tags:
- since_data_inception: the return runs from the entity's first snapshot, not from when the account opened.
- configured_inception: the window starts at an inception date set in the config.
- staggered_inception: the entity's parts began on different dates.
- accounts_grain_meaningless: a return for this one account is noise (a single crypto wallet, say); read a coarser view.
- empty_bucket, carried_forward: no snapshot fell in the bucket; the value is carried from before it.
- boundary_same_snapshot: the bucket starts and ends on the same snapshot.
- stale_snapshot: the end value rests on a snapshot much older than the entity's usual gap; the feed may have stopped.
- dietz_degenerate: the bucket's approximation has no defined value.
- nonpositive_base: a loan or a net-negative entity; a return means nothing.
- mwr_no_flows: no money moved in or out, so there is no money-weighted return; read twr.
- mwr_no_sign_change, mwr_nonunique, mwr_no_converge: the money-weighted return has no single answer.
- mwr_negative_net_capital: the money put in, net, is zero or less.
- unmatched_transfers=N: N transfers found no matching leg on another account.
- nav_only, nav_only_capital_call_risk: a source that reports values only (private funds); the return is rough.
- pre_fx_history: part of the window lies before the exchange-rate history begins.
- flows_before_inception: money moved before the date the measurement starts; it counts in no window.
- after_tax: the return is net of the fees and taxes paid.
Other tags name their reason the same way.

The gains quality column names each way a figure can be incomplete:
- sells_without_documents=N: N sales have no tax document or statement lot, so realized misses them.
- lots_without_gain=N: N realized lots state no gain, and not both proceeds and a cost basis.
- undated_lots=N: N lots state only a tax year and count at its last day.
- unmatched_lots=N: N lots name an instrument the account never held in a snapshot.
- in_kind_moves=N: N securities moved in or out of the account without a sale; each brings or takes its whole unrealized gain.
- corporate_actions=N: N mergers, splits or spin-offs turned one holding into another without a realized lot.
- basis_changed=N: N holdings began or stopped carrying a cost basis inside the period; their change is left out.
- accounts_unobserved=N: N accounts are missing from one end of the period while their source has a snapshot there; the snapshot left them out, or they closed.
- paid_in_basis: a private holding's cost basis is the capital paid in; cash paid back is not realized gain.
- onboarded_in_window=<source>: the source's data begins inside the period, so its start value is zero.
- fx_missing=N: N figures had no exchange rate and are left out.`

func (s *mcpServer) privacyHelp() string {
	if s.privacy {
		return privacyNotice + ` Amounts and quantities print as *****.** and ***, account numbers keep only their last digits, and merchant, payer and narrative text prints as ***. Account names and nicknames, holdings and their symbols, dates, categories, types, classes and shares stay legible.
Filters and sorts read what is shown: a money column cannot order the rows here, and a merchant filter matches nothing.`
	}
	return `This endpoint serves full data. The server's /mcp/privacy endpoint (or a stdio server started with --privacy) serves the same tools with amounts, quantities, account numbers and the names taken off statements redacted; account names, holdings, categories, types, dates and shares stay legible.
Which endpoint a client is given is the operator's choice; no parameter changes it.`
}

// glossary explains the taxonomy columns, each with the values gold
// holds today.
func (s *mcpServer) glossary(ctx context.Context) (string, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	present := map[string]map[string]bool{}
	note := func(field, v string) {
		if v == "" {
			return
		}
		if present[field] == nil {
			present[field] = map[string]bool{}
		}
		present[field][v] = true
	}
	err = s.withGold(ctx, cfg, func(db *sql.DB) error {
		asOf := anchorToDay(s.now(), true).Unix()
		accounts, err := gold.AccountsAsOf(ctx, db, asOf, cfg.DefaultCurrency)
		if err != nil {
			return err
		}
		for _, a := range accounts {
			note("account_kind", a.AccountKind)
			note("tax_wrapper", strOrEmpty(a.TaxWrapper))
			note("management_style", strOrEmpty(a.ManagementStyle))
		}
		positions, err := gold.PositionsAsOf(ctx, db, asOf, cfg.DefaultCurrency)
		if err != nil {
			return err
		}
		for _, p := range positions {
			note("asset_class", p.AssetClass)
			note("vehicle", p.Vehicle)
		}
		return nil
	})
	var b strings.Builder
	for _, g := range glossaryTerms {
		fmt.Fprintf(&b, "%s: %s", g.field, g.meaning)
		if vals := present[g.field]; len(vals) > 0 {
			list := make([]string, 0, len(vals))
			for v := range vals {
				list = append(list, v)
			}
			sort.Strings(list)
			fmt.Fprintf(&b, " Here: %s.", strings.Join(list, ", "))
		}
		b.WriteString("\n")
	}
	if err != nil {
		fmt.Fprintf(&b, "(the values present could not be read: %s)\n", s.scrubPaths(err.Error()))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

var glossaryTerms = []struct{ field, meaning string }{
	{"account_kind", "what an account is: brokerage, cash, card, mortgage, custody, crypto_exchange, donor_advised_fund, other and more."},
	{"tax_wrapper", "how an account is taxed: taxable_personal, taxable_joint, roth_ira, traditional_ira, 401k, hsa, 529, pillar_2, pillar_3a, trust_grantor and more. A blank on the sources view means its accounts differ."},
	{"management_style", "who decides what an account holds: self_directed, advisory, discretionary, automated."},
	{"asset_class", "what a position is exposed to: public_equity, private_equity, fixed_income, real_estate, cash, crypto, metal, multi_asset and more."},
	{"vehicle", "how a position is held: stock, etf, fund, bond, demand_deposit, time_deposit, physical, loan, mortgage and more."},
}

// addResources publishes the guide as resources, for clients that
// preload them: wealthdb://guide is the overview, and
// wealthdb://guide/<topic> each tool's help and each convention.
func (s *mcpServer) addResources(srv *mcp.Server) {
	add := func(uri, name, topic string) {
		srv.AddResource(&mcp.Resource{URI: uri, Name: name, MIMEType: "text/plain"},
			func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				text, err := s.describeText(ctx, topic)
				if err != nil {
					return nil, fmt.Errorf("%s", s.scrubPaths(err.Error()))
				}
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/plain", Text: text}}}, nil
			})
	}
	add("wealthdb://guide", "guide", "")
	for _, topic := range append(s.reportToolNames(), describeTopics...) {
		add("wealthdb://guide/"+topic, "guide: "+topic, topic)
	}
}
