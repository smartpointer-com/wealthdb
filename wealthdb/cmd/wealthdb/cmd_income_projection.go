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

// forwardViews are the views that look forward.
var forwardViews = []string{"projection", "calendar", "sources", "portfolios", "accounts", "positions"}

func isForwardView(view string) bool { return slices.Contains(forwardViews, view) }

// projectionPeriods are the periods the forward views take.
var projectionPeriods = []string{"monthly", "quarterly", "annual"}

// forwardGrains maps the entity views to their grain.
var forwardGrains = map[string]yield.Grain{"sources": yield.GrainSources, "portfolios": yield.GrainPortfolios,
	"accounts": yield.GrainAccounts, "positions": yield.GrainPositions}

// forwardFlags are the assumptions the expected income rests on.
type forwardFlags struct {
	rates, maturing *string
	shift           *int
}

// pathNames and ruleNames are the choices of --rates and --maturing.
func pathNames() []string {
	var out []string
	for _, p := range yield.Paths {
		out = append(out, string(p))
	}
	return out
}

func ruleNames() []string {
	var out []string
	for _, r := range yield.Rules {
		out = append(out, string(r))
	}
	return out
}

func registerForwardFlags(fs *flag.FlagSet) *forwardFlags {
	return &forwardFlags{
		rates: fs.String("rates", string(yield.PathConstant),
			strings.Join(pathNames(), " | ")+" — how floating rates move (forward views)"),
		maturing: fs.String("maturing", string(yield.RuleCash),
			strings.Join(ruleNames(), " | ")+" — what a maturing principal earns (forward views)"),
		shift: fs.Int("shift", 0, "a parallel shift of the floating rates, in basis points (forward views)"),
	}
}

// check refuses the forward flags on the views that read only what was
// received, --level on the forward views, and a value outside each
// flag's choices.
func (f *forwardFlags) check(fs *flag.FlagSet, forward bool) error {
	if !forward && isSet(fs, "rates", "maturing", "shift") {
		return errs.Newf(errs.ExitUsage, "income: --rates, --maturing and --shift belong to the %s views",
			strings.Join(forwardViews, ", "))
	}
	if forward && isSet(fs, "level") {
		return errs.Newf(errs.ExitUsage, "income: --level belongs to the types view")
	}
	if err := checkChoice("income", "rates", *f.rates, pathNames()...); err != nil {
		return err
	}
	return checkChoice("income", "maturing", *f.maturing, ruleNames()...)
}

func (f *forwardFlags) assumptions() yield.Assumptions {
	return yield.Assumptions{Path: yield.Path(*f.rates), Rule: yield.Rule(*f.maturing), ShiftBP: float64(*f.shift)}
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

func loadForwardIncome(ctx context.Context, db *sql.DB, req request) (*forwardIncome, error) {
	b, err := yieldBook(ctx, db, req.asOf, req.currency)
	if err != nil {
		return nil, err
	}
	w := yield.Window{From: time.Unix(req.from, 0).UTC(), To: time.Unix(req.to, 0).UTC()}
	if w.To.After(b.Last()) {
		return nil, errs.Newf(errs.ExitUsage, "income: the projection ends on %s; the window ends after it",
			b.Last().Format(time.DateOnly))
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

// incomeForwardReport is one forward view.
func incomeForwardReport(req request) *report {
	ccy := req.currency
	switch req.view {
	case "projection":
		months := map[string]int{"monthly": 1, "quarterly": 3, "annual": 12}[req.period]
		return newReport(buildProjectionColumns(ccy, req.period), defaultProjectionColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.ProjectionRow, error) {
				fi, err := loadForwardIncome(ctx, db, req)
				if err != nil {
					return nil, err
				}
				return fi.book.ProjectionRows(fi.amounts, fi.window, months), nil
			})
	case "calendar":
		return newReport(buildCalendarColumns(ccy), defaultCalendarColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.CalendarRow, error) {
				fi, err := loadForwardIncome(ctx, db, req)
				if err != nil {
					return nil, err
				}
				return fi.book.Calendar(fi.amounts), nil
			})
	}
	grain := forwardGrains[req.view]
	kindOf, loadKinds := sourceKinds()
	return newReport(buildIncomeEntityColumns(ccy, grain, kindOf), incomeEntityDefaults[grain],
		func(ctx context.Context, db *sql.DB) ([]yield.IncomeRow, error) {
			if err := loadKinds(ctx, db); err != nil {
				return nil, err
			}
			fi, err := loadForwardIncome(ctx, db, req)
			if err != nil {
				return nil, err
			}
			return fi.book.IncomeRows(grain, fi.amounts, fi.lastYear, fi.next12), nil
		})
}

// ---- column registries ---------------------------------------------------

func money[T any](name, outCcy string, get func(T) float64) columnSpec[T] {
	return floatMoneyCol(name, outCcy, func(r T) *float64 { v := get(r); return &v })
}

func buildProjectionColumns(outCcy, period string) []columnSpec[yield.ProjectionRow] {
	type row = yield.ProjectionRow
	cols := []columnSpec[row]{
		{Name: "period", Align: output.AlignLeft, Extract: func(r row) string {
			switch {
			case r.Total:
				return "total"
			case r.Average:
				return "monthly_average"
			}
			at := r.Period.Unix()
			return periodLabel(&at, period)
		}},
		money("received", outCcy, func(r row) float64 { return r.Received }),
		money("expected", outCcy, func(r row) float64 { return r.Expected }),
		money("total", outCcy, func(r row) float64 { return r.Received + r.Expected }),
	}
	for _, t := range yield.IncomeTypes {
		cols = append(cols, money(string(t), outCcy, func(r row) float64 { v := r.ByType[t]; return v[0] + v[1] }))
	}
	for _, t := range yield.IncomeTypes {
		cols = append(cols,
			money(string(t)+"_received", outCcy, func(r row) float64 { return r.ByType[t][0] }),
			money(string(t)+"_expected", outCcy, func(r row) float64 { return r.ByType[t][1] }))
	}
	return append(cols,
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		money("principal", outCcy, func(r row) float64 { return r.Principal }),
		columnSpec[row]{Name: "status", Align: output.AlignLeft, Extract: func(r row) string { return r.Status }},
		columnSpec[row]{Name: "basis", Align: output.AlignLeft, Extract: func(r row) string { return r.Basis }},
		columnSpec[row]{Name: "quality", Align: output.AlignLeft, Extract: func(r row) string { return r.Quality }},
	)
}

var defaultProjectionColumns = []string{"period", "received", "expected", "interest", "coupons", "dividends",
	"distributions", "withheld", "status", "basis", "quality"}

func buildCalendarColumns(outCcy string) []columnSpec[yield.CalendarRow] {
	type row = yield.CalendarRow
	text := func(name string, get func(row) string) columnSpec[row] {
		return columnSpec[row]{Name: name, Align: output.AlignLeft, Extract: get}
	}
	pos := func(get func(*yield.Position) string) func(row) string {
		return func(r row) string {
			if r.Position == nil {
				return ""
			}
			return get(r.Position)
		}
	}
	return []columnSpec[row]{
		text("date", func(r row) string {
			if r.Day != nil {
				return r.Day.Format(time.DateOnly)
			}
			return r.Month.Format("2006-01")
		}),
		text("silver_source", func(r row) string { return r.Holding.Source }),
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string {
			name := r.Account.Name
			return accountLabel(&name, r.Holding.Account)
		}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.Holding.Account }},
		text("position_key", func(r row) string { return r.Holding.PositionKey }),
		text("symbol", pos(func(p *yield.Position) string { return p.Symbol })),
		text("name", pos(func(p *yield.Position) string { return p.Name })),
		text("type", func(r row) string { return string(r.Type) }),
		text("kind", func(r row) string {
			if r.Principal {
				return "principal"
			}
			return "income"
		}),
		text("currency", func(r row) string { return r.Currency }),
		{Name: "amount", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r row) string { return fmt.Sprintf("%.2f", r.Own) }},
		money("value", outCcy, func(r row) float64 { return r.Value }),
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		text("basis", func(r row) string { return string(r.Basis) }),
		text("quality", func(r row) string { return r.Quality }),
	}
}

var defaultCalendarColumns = []string{"date", "silver_source", "account", "symbol", "type", "kind", "currency",
	"amount", "value", "basis", "quality"}

func buildIncomeEntityColumns(outCcy string, grain yield.Grain, kindOf func(string) string) []columnSpec[yield.IncomeRow] {
	type row = yield.IncomeRow
	type col = columnSpec[row]
	text := func(name string, get func(row) string) col {
		return col{Name: name, Align: output.AlignLeft, Extract: get}
	}
	pos := func(get func(*yield.Position) string) func(row) string {
		return func(r row) string {
			if r.Position == nil {
				return ""
			}
			return get(r.Position)
		}
	}
	cols := []col{text("silver_source", func(r row) string { return r.Source })}
	switch grain {
	case yield.GrainPortfolios:
		cols = append(cols,
			col{Name: "portfolio", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				PrivacyFunc: func(r row) PrivacyClass {
					return portfolioNamePrivacy(kindOf)(gold.PortfolioRow{SilverSourceID: r.Source,
						PortfolioExternalID: r.Portfolio})
				},
				Extract: func(r row) string {
					if r.Portfolio == "" {
						return "(no portfolio)"
					}
					name := r.PortfolioName
					return accountLabel(&name, r.Portfolio)
				}},
			col{Name: "portfolio_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return r.Portfolio }})
	case yield.GrainAccounts, yield.GrainPositions:
		cols = append(cols,
			col{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string {
				name := r.Account.Name
				return accountLabel(&name, r.AccountID)
			}},
			col{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
				Extract: func(r row) string { return r.AccountID }},
			text("tax_wrapper", func(r row) string { return r.Account.TaxWrapper }))
	}
	if grain == yield.GrainPositions {
		cols = append(cols,
			// position_key stays legible as `holdings positions` keeps it.
			text("position_key", func(r row) string { return r.Holding.PositionKey }),
			text("symbol", pos(func(p *yield.Position) string { return p.Symbol })),
			text("name", pos(func(p *yield.Position) string { return p.Name })),
			text("vehicle", pos(func(p *yield.Position) string { return p.Vehicle })),
			text("currency", pos(func(p *yield.Position) string { return p.Currency })),
			col{Name: "quantity", Align: output.AlignRight, Privacy: PrivacyQuantity,
				Extract: pos(func(p *yield.Position) string {
					if p.Quantity == nil {
						return ""
					}
					return strconv.FormatFloat(*p.Quantity, 'f', -1, 64)
				})},
			col{Name: "price", Align: output.AlignRight, Privacy: PrivacyMoney,
				Extract: pos(func(p *yield.Position) string { return fixed4(p.Price) })},
			floatMoneyCol("value", outCcy, func(r row) *float64 {
				if r.Position == nil {
					return nil
				}
				v := r.Position.Value
				return &v
			}))
	}
	return append(cols,
		money("received", outCcy, func(r row) float64 { return r.Received }),
		money("expected", outCcy, func(r row) float64 { return r.Expected }),
		money("total", outCcy, func(r row) float64 { return r.Total() }),
		floatMoneyCol("last_year", outCcy, func(r row) *float64 { return r.LastYear }),
		money("next_12m", outCcy, func(r row) float64 { return r.Next12 }),
		floatMoneyCol("withheld", outCcy, func(r row) *float64 { return r.Withheld }),
		text("basis", func(r row) string { return r.Basis }),
		text("quality", func(r row) string { return r.Quality }),
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
