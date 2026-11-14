package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The forward views of `wealthdb income`: what was received and what is
// expected, over a window that may reach into the future. docs/INCOME.md
// §12 defines them; the projection is internal/yield's (Book.Amounts),
// computed from today's holdings, and the received half the income
// lines'. A day up to today shows what was received, a day after it what
// is expected.

// incomeBasisTags are the bases an expected amount rests on and what
// each means (docs/INCOME.md §12), in the order of yield.Bases: the CLI
// usage and the MCP help both render them.
var incomeBasisTags = []tagNote{
	{string(yield.BasisDeclared), "a rate a ledger record states, a coupon by contract, a bill's discount"},
	{string(yield.BasisIndicated), "the last payment at its frequency, or the filings' dividends"},
	{string(yield.BasisStated), "a rate a source states, held"},
	{string(yield.BasisForward), "a stated, declared or indicated floating rate, moved along its currency's " +
		"forward curve"},
	{string(yield.BasisReinvested), "what a principal earns after a maturity"},
	{string(yield.BasisRealized), "last year's payments, where nothing else is known"},
	{string(yield.BasisUnknown), "nothing; the amount is zero"},
}

// incomeRungTags are the ways a principal earns after a maturity, which
// the forward views' quality column shares out (docs/INCOME.md §12), in
// the order of yield.Rungs.
var incomeRungTags = []tagNote{
	{string(yield.RungCash), "the cash rule: the rate of the account's cash"},
	{string(yield.RungRoll), "the roll rule: the forward rate of the holding's term plus its own spread"},
	{string(yield.RungRollConstant), "a roll at the holding's own implied yield, held constant, where its currency " +
		"has no curve"},
	{string(yield.RungRollOwn), "a roll at its current yield, else its stated rate, held constant, where it implies " +
		"none"},
	{string(yield.RungRollCash), "a roll into cash, where the holding has no yield at all"},
}

// forwardOptions are the assumptions the expected income rests on: the
// forward views' --rates, --maturing and --shift.
var forwardOptions = []option{
	{name: "rates", cli: []string{"rates"}, choices: pathNames(),
		def: [2]string{string(yield.DefaultAssumptions.Path), string(yield.DefaultAssumptions.Path)}},
	{name: "maturing", cli: []string{"maturing"}, choices: ruleNames(),
		def: [2]string{string(yield.DefaultAssumptions.Rule), string(yield.DefaultAssumptions.Rule)}},
	{name: "shift", cli: []string{"shift"}, kind: optInt, def: [2]string{"0", ""}},
}

// pathNames and ruleNames are the choices of --rates and --maturing.
func pathNames() []string { return stringsOf(yield.Paths) }

func ruleNames() []string { return stringsOf(yield.Rules) }

// forwardAssumptions are the assumptions in asks for, --rates and
// --maturing refused outside their choices, in that order.
func forwardAssumptions(in optionSource, cmd string) (yield.Assumptions, error) {
	rates, err := choose(in, cmd, "rates")
	if err != nil {
		return yield.Assumptions{}, err
	}
	maturing, err := choose(in, cmd, "maturing")
	if err != nil {
		return yield.Assumptions{}, err
	}
	return yield.Assumptions{Path: yield.Path(rates), Rule: yield.Rule(maturing), ShiftBP: float64(in.num("shift"))}, nil
}

// checkForwardOptions refuses the options a view does not take, the CLI
// and the MCP server alike: the assumptions off the forward views; on
// them the type level, a period off projection, and a period that is
// none of seriesPeriods. has says an option is given; flag spells an
// option's name as the front-end does.
func checkForwardOptions(fam *family, view, period string, has func(string) bool, flag func(string) string) error {
	if !fam.view(view).forward {
		return refuseAssumptions("income", "the "+andList(fam.forwardViews())+" views",
			has, flag)
	}
	switch {
	case has("level"):
		return errs.Newf(errs.ExitUsage, "income: %s belongs to the types view", flag("level"))
	case view != "projection" && has("period"):
		return errs.Newf(errs.ExitUsage, "income: of the forward views, %s belongs to projection", flag("period"))
	}
	return checkSeriesPeriod("income", "the forward views take", period, flag)
}

// refuseAssumptions refuses the projection's assumptions on a view of cmd
// that does not look forward; where names the views that take them.
func refuseAssumptions(cmd, where string, has func(string) bool, flag func(string) string) error {
	if has("rates") || has("maturing") || has("shift") {
		return errs.Newf(errs.ExitUsage, "%s: %s, %s and %s belong to %s", cmd, flag("rates"), flag("maturing"),
			flag("shift"), where)
	}
	return nil
}

// checkSeriesPeriod refuses a forward view's period that is none of
// seriesPeriods; takes says who takes them ("projection takes").
func checkSeriesPeriod(cmd, takes, period string, flag func(string) string) error {
	if !slices.Contains(seriesPeriods, period) {
		return errs.Newf(errs.ExitUsage, "%s: %s %s %s", cmd, takes, flag("period"), strings.Join(seriesPeriods, " | "))
	}
	return nil
}

// windowParser reads a forward view's FROM and TO ("" where absent) at
// now, horizon months its default span: parseForwardWindow,
// parsePlanWindow.
type windowParser func(from, to string, now time.Time, horizon int) (int64, int64, error)

// reportWindow is a report's positional window: the trailing year where
// forward is nil, else forward's reading of FROM [TO]. A refusal prints
// fs's usage and is cmd's usage error.
func reportWindow(fs *flag.FlagSet, cmd string, now time.Time, horizon int, forward windowParser) (int64, int64, error) {
	var from, to int64
	var err error
	switch {
	case forward == nil:
		from, to, err = parseTrailingYearWindow(fs.Args(), now)
	case fs.NArg() > 2:
		err = fmt.Errorf("a window is FROM [TO]")
	default:
		from, to, err = forward(fs.Arg(0), fs.Arg(1), now, horizon)
	}
	if err != nil {
		fs.Usage()
		return 0, 0, errs.Newf(errs.ExitUsage, "%s: %s", cmd, err.Error())
	}
	return from, to, nil
}

// assumptionsHeader names the assumptions in an MCP result's header, the
// shift only where there is one.
func assumptionsHeader(as yield.Assumptions) []string {
	h := []string{"rates=" + string(as.Path), "maturing=" + string(as.Rule)}
	if as.ShiftBP != 0 {
		h = append(h, fmt.Sprintf("shift=%+.0fbp", as.ShiftBP))
	}
	return h
}

// parseForwardWindow is the window of a forward view: FROM and TO as the
// other views read them, a lone FROM its own span, a lone TO from the
// first day of next month, neither the horizon months after the current
// one. An open bound ("-") has no meaning ahead of today.
func parseForwardWindow(from, to string, now time.Time, horizon int) (int64, int64, error) {
	if from == "-" || to == "-" {
		return 0, 0, fmt.Errorf("a forward view's window takes no open bound (-): give FROM, FROM TO, or neither")
	}
	n := now.UTC()
	next := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	switch {
	case from == "" && to == "":
		return next.Unix(), next.AddDate(0, horizon, 0).Unix() - 1, nil
	case to == "":
		return parseDateRange([]string{from}, now)
	case from == "":
		return parseDateRange([]string{next.Format(time.DateOnly), to}, now)
	}
	return parseDateRange([]string{from, to}, now)
}

// forwardIncome is what every forward view reads: the book of today, the
// amounts of the window, of the same window a year before, and of the
// next twelve months.
type forwardIncome struct {
	book                      *yield.Book
	amounts, lastYear, next12 []yield.Amount
	window                    yield.Window
}

// forwardBook is the book of today and the request's window, refused
// where the window ends after the projection does: what every forward
// view of cmd (income, cashflow) projects over.
func forwardBook(ctx context.Context, db *sql.DB, req request, cmd string) (*yield.Book, yield.Window, error) {
	b, err := yieldBook(ctx, db, req.asOf, req.currency)
	if err != nil {
		return nil, yield.Window{}, err
	}
	w := yield.Window{From: time.Unix(req.from, 0).UTC(), To: time.Unix(req.to, 0).UTC()}
	if w.To.After(b.Last()) {
		return nil, yield.Window{}, errs.Newf(errs.ExitUsage, "%s: the projection ends on %s; the window ends after it",
			cmd, b.Last().Format(time.DateOnly))
	}
	return b, w, nil
}

func loadForwardIncome(ctx context.Context, db *sql.DB, req request) (*forwardIncome, error) {
	b, w, err := forwardBook(ctx, db, req, "income")
	if err != nil {
		return nil, err
	}
	received, err := gold.IncomeReceived(ctx, db, b.In, w.From.Unix(), min(w.To.Unix(), req.asOf))
	if err != nil {
		return nil, err
	}
	fi := &forwardIncome{book: b, window: w, amounts: b.Amounts(received, w, req.assumptions)}
	// Last year's figure is the same window a year earlier, once it is
	// over.
	lw := yield.Window{From: w.From.AddDate(-1, 0, 0), To: w.To.AddDate(-1, 0, 0)}
	if lw.To.Unix() <= req.asOf {
		prior, err := gold.IncomeReceived(ctx, db, b.In, lw.From.Unix(), min(lw.To.Unix(), req.asOf))
		if err != nil {
			return nil, err
		}
		fi.lastYear = b.Amounts(prior, lw, req.assumptions)
	}
	fi.next12 = b.Amounts(nil, b.NextMonths(12), req.assumptions)
	return fi, nil
}

// incomeProjectionReport is the projection: per month, received and
// expected.
func incomeProjectionReport(_ *config.Config, req request, _ *toolArgs) *report {
	months := seriesPeriodMonths(req.period)
	return newReport(buildProjectionColumns(req.currency, req.period), defaultProjectionColumns,
		func(ctx context.Context, db *sql.DB) ([]yield.ProjectionRow, error) {
			fi, err := loadForwardIncome(ctx, db, req)
			if err != nil {
				return nil, err
			}
			return fi.book.ProjectionRows(fi.amounts, fi.window, months), nil
		})
}

// incomeCalendarReport is the calendar: each expected payment and each
// principal a maturity returns.
func incomeCalendarReport(_ *config.Config, req request, _ *toolArgs) *report {
	kindOf, loadKinds := sourceKinds()
	return withLoads(newReport(buildCalendarColumns(req.currency, kindOf), defaultCalendarColumns,
		func(ctx context.Context, db *sql.DB) ([]yield.CalendarRow, error) {
			fi, err := loadForwardIncome(ctx, db, req)
			if err != nil {
				return nil, err
			}
			return fi.book.Calendar(fi.amounts), nil
		}), loadKinds)
}

// incomeEntityView is a forward view per entity at grain: received,
// expected and total, last year's and the next twelve months'.
func incomeEntityView(name string, grain yield.Grain) view {
	return view{name: name, forward: true, window: parseForwardWindow, total: flowTotal, principal: fixedColumn("total"), header: forwardHeader,
		build: func(_ *config.Config, req request, _ *toolArgs) *report {
			kindOf, loadKinds := sourceKinds()
			return withLoads(newReport(buildIncomeEntityColumns(req.currency, grain, kindOf), incomeEntityDefaults[grain],
				func(ctx context.Context, db *sql.DB) ([]yield.IncomeRow, error) {
					fi, err := loadForwardIncome(ctx, db, req)
					if err != nil {
						return nil, err
					}
					return fi.book.IncomeRows(grain, fi.amounts, fi.lastYear, fi.next12), nil
				}), loadKinds)
		}}
}

// forwardHeader is the header of a forward view whose period is no
// option: the assumptions.
func forwardHeader(req request) []string { return assumptionsHeader(req.assumptions) }

// ---- column registries ---------------------------------------------------

// calendarPos and incomePos read a field of a row's position, blank
// where the row names none (an account's income, a holding no longer
// held).
func calendarPos(get func(*yield.Position) string) func(yield.CalendarRow) string {
	return func(r yield.CalendarRow) string { return positionField(r.Position, get) }
}

func incomePos(get func(*yield.Position) string) func(yield.IncomeRow) string {
	return func(r yield.IncomeRow) string { return positionField(r.Position, get) }
}

func positionField(p *yield.Position, get func(*yield.Position) string) string {
	if p == nil {
		return ""
	}
	return get(p)
}

func buildProjectionColumns(outCcy, period string) []columnSpec[yield.ProjectionRow] {
	type row = yield.ProjectionRow
	cols := []columnSpec[row]{
		{Name: "period", Align: output.AlignLeft, Date: true, Extract: func(r row) string {
			switch {
			case r.Total:
				return "total"
			case r.Average:
				return "monthly_average"
			}
			at := r.Period.Unix()
			return periodLabel(&at, period)
		}},
		floatValCol("received", outCcy, func(r row) float64 { return r.Received }),
		floatValCol("expected", outCcy, func(r row) float64 { return r.Expected }),
		floatValCol("total", outCcy, func(r row) float64 { return r.Received + r.Expected }),
	}
	for _, t := range yield.IncomeTypes {
		cols = append(cols, floatValCol(string(t), outCcy, func(r row) float64 { v := r.ByType[t]; return v[0] + v[1] }))
	}
	for _, t := range yield.IncomeTypes {
		cols = append(cols,
			floatValCol(string(t)+"_received", outCcy, func(r row) float64 { return r.ByType[t][0] }),
			floatValCol(string(t)+"_expected", outCcy, func(r row) float64 { return r.ByType[t][1] }))
	}
	return append(cols,
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		floatValCol("principal", outCcy, func(r row) float64 { return r.Principal }),
		strCol("status", func(r row) string { return r.Status }),
		strCol("basis", func(r row) string { return r.Basis }),
		qualityCol(func(r row) string { return r.Quality }),
	)
}

var defaultProjectionColumns = []string{"period", "received", "expected", "interest", "coupons", "dividends",
	"distributions", "withheld", "status", "basis", "quality"}

func buildCalendarColumns(outCcy string, kindOf func(string) string) []columnSpec[yield.CalendarRow] {
	type row = yield.CalendarRow
	ref := func(r row) accountRef {
		return accountRef{source: r.Holding.Source, name: &r.Account.Name,
			nickname: &r.Account.Nickname, id: r.Holding.Account}
	}
	key, name := holdingCols(kindOf, func(r row) string { return r.Holding.Source },
		func(r row) string { return r.Holding.PositionKey }, calendarPos(func(p *yield.Position) string { return p.Name }))
	return []columnSpec[row]{
		asDate(strCol("date", func(r row) string {
			if r.Day != nil {
				return r.Day.Format(time.DateOnly)
			}
			return r.Month.Format("2006-01")
		})),
		strCol("silver_source", func(r row) string { return r.Holding.Source }),
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		key,
		strCol("symbol", calendarPos(func(p *yield.Position) string { return p.Symbol })),
		name,
		strCol("type", func(r row) string { return string(r.Type) }),
		strCol("kind", func(r row) string {
			if r.Principal {
				return "principal"
			}
			return "income"
		}),
		strCol("currency", func(r row) string { return r.Currency }),
		{Name: "amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r row) string { return fmt.Sprintf("%.2f", r.Own) }},
		floatValCol("value", outCcy, func(r row) float64 { return r.Value }),
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		strCol("basis", func(r row) string { return string(r.Basis) }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultCalendarColumns = []string{"date", "silver_source", "account", "symbol", "type", "kind", "currency",
	"amount", "value", "basis", "quality"}

func buildIncomeEntityColumns(outCcy string, grain yield.Grain, kindOf func(string) string) []columnSpec[yield.IncomeRow] {
	type row = yield.IncomeRow
	type col = columnSpec[row]
	cols := []col{strCol("silver_source", func(r row) string { return r.Source })}
	switch grain {
	case yield.GrainPortfolios:
		cols = append(cols,
			portfolioCol("portfolio", kindOf, func(r row) (string, string, *string) {
				return r.Source, r.Portfolio, &r.PortfolioName
			}),
			typedIDCol("portfolio_id", kindOf, func(r row) (string, string) { return r.Source, r.Portfolio }))
	case yield.GrainAccounts, yield.GrainPositions:
		ref := func(r row) accountRef {
			return accountRef{source: r.Source, name: &r.Account.Name, nickname: &r.Account.Nickname, id: r.AccountID}
		}
		cols = append(cols,
			accountCol(kindOf, ref),
			accountIDCol("account_id", kindOf, ref),
			strCol("tax_wrapper", func(r row) string { return r.Account.TaxWrapper }))
	}
	if grain == yield.GrainPositions {
		// A public position_key stays legible, as `holdings positions`
		// keeps it.
		key, name := holdingCols(kindOf, func(r row) string { return r.Source },
			func(r row) string { return r.Holding.PositionKey }, incomePos(func(p *yield.Position) string { return p.Name }))
		cols = append(cols,
			key,
			strCol("symbol", incomePos(func(p *yield.Position) string { return p.Symbol })),
			name,
			strCol("vehicle", incomePos(func(p *yield.Position) string { return p.Vehicle })),
			strCol("currency", incomePos(func(p *yield.Position) string { return p.Currency })),
			col{Name: "quantity", Align: output.AlignRight, Privacy: PrivacyQuantity,
				Extract: incomePos(func(p *yield.Position) string {
					if p.Quantity == nil {
						return ""
					}
					return strconv.FormatFloat(*p.Quantity, 'f', -1, 64)
				})},
			priceCol(func(r row) *float64 {
				if r.Position == nil {
					return nil
				}
				return r.Position.Price
			}),
			floatMoneyCol("value", outCcy, func(r row) *float64 {
				if r.Position == nil {
					return nil
				}
				v := r.Position.Value
				return &v
			}))
	}
	return append(cols,
		floatValCol("received", outCcy, func(r row) float64 { return r.Received }),
		floatValCol("expected", outCcy, func(r row) float64 { return r.Expected }),
		floatValCol("total", outCcy, func(r row) float64 { return r.Total() }),
		floatMoneyCol("last_year", outCcy, func(r row) *float64 { return r.LastYear }),
		floatValCol("next_12m", outCcy, func(r row) float64 { return r.Next12 }),
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		strCol("basis", func(r row) string { return r.Basis }),
		qualityCol(func(r row) string { return r.Quality }),
	)
}

var incomeEntityFigures = []string{"received", "expected", "total", "next_12m", "basis", "quality"}

var incomeEntityDefaults = map[yield.Grain][]string{
	yield.GrainSources:    append([]string{"silver_source"}, incomeEntityFigures...),
	yield.GrainPortfolios: append([]string{"silver_source", "portfolio"}, incomeEntityFigures...),
	yield.GrainAccounts:   append([]string{"silver_source", "account", "tax_wrapper"}, incomeEntityFigures...),
	yield.GrainPositions: append([]string{"account", "symbol", "vehicle", "quantity", "price", "value"},
		incomeEntityFigures...),
}
