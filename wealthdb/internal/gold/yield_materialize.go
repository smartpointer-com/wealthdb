package gold

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// YieldHistoryMonths are the month ends before today the Yield
// dashboard's as-of and over-time cards read.
const YieldHistoryMonths = 12

// yieldInstants are the instants report_yield_positions holds: the last
// second of each of the YieldHistoryMonths months before toEpoch's, then
// toEpoch.
func yieldInstants(toEpoch int64) []int64 {
	return monthEndsTo(toEpoch, YieldHistoryMonths)
}

// MaterializeYield rewrites the Yield dashboard's tables (migration
// 0138) and the cash flow projection's look-back parts (migration
// 0149): every holding's figures and DV01 per tenor in every reporting
// currency at yieldInstants, and at toEpoch the projection's parts, the
// rate table, the principals maturities return, every income line
// received, and the cash flow projection's parts over a look-back of
// lookback months (CashflowPlan). Each figure is the yield report's
// (yield.Compute over YieldInputs), so the dashboard and the CLI agree.
// It returns the number of rows written.
// One transaction, so a reader sees the old tables or the new ones.
func MaterializeYield(ctx context.Context, db *sql.DB, toEpoch, computedAt int64, lookback int) (int, error) {
	// Every instant reads the same endings and spans, through the
	// transaction.
	tx, commit, err := beginPinned(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("MaterializeYield: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after commit
	for _, t := range []string{"report_yield_positions", "report_yield_buckets", "report_yield_parts",
		"report_yield_rates", "report_yield_events", "report_yield_received", "report_cashflow_plan"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("MaterializeYield %s: delete: %w", t, err)
		}
	}
	m := yieldMaterializer{ctx: ctx, tx: tx, computedAt: computedAt}
	today := map[string]*yield.Book{}
	// What depends on neither the currency nor the instant is read once.
	shared := &yieldShared{}
	for _, at := range yieldInstants(toEpoch) {
		// What does not depend on the currency is read once an instant.
		base, err := readYieldBase(ctx, tx, at, shared)
		if err != nil {
			return 0, fmt.Errorf("MaterializeYield: %w", err)
		}
		for _, ccy := range materializeCurrencies {
			in, err := base.inputs(ctx, tx, ccy)
			if err != nil {
				return 0, fmt.Errorf("MaterializeYield: %w", err)
			}
			b := yield.Compute(in)
			if err := m.positions(b, at, ccy); err != nil {
				return 0, err
			}
			if at == toEpoch {
				today[ccy] = b
			}
		}
	}
	if err := m.projection(today); err != nil {
		return 0, err
	}
	if err := m.received(today, toEpoch); err != nil {
		return 0, err
	}
	if err := m.plan(today, toEpoch, lookback); err != nil {
		return 0, err
	}
	if err := commit(); err != nil {
		return 0, fmt.Errorf("MaterializeYield: %w", err)
	}
	return m.rows, nil
}

type yieldMaterializer struct {
	ctx        context.Context
	tx         *sql.Tx
	computedAt int64
	rows       int
}

func (m *yieldMaterializer) insert(table, cols string, n int, row func(i int, args []any) []any) error {
	if n == 0 {
		return nil
	}
	if err := insertColumns(m.ctx, m.tx, table, table, cols, n, row); err != nil {
		return fmt.Errorf("MaterializeYield %s: %w", table, err)
	}
	m.rows += n
	return nil
}

const yieldPositionCols = `computed_at, as_of, currency, silver_source_id, account_external_id, position_key,
    tax_wrapper, symbol, name, asset_class, vehicle, kind, holding_currency,
    quantity, value, price, coupon_pct, maturity_date, rating, stated_pct, current_yield_pct, implied_pct,
    implied_annual_pct, realized_pct, received_12m, mean_value, earnings_yield_pct, dividend_yield_pct, payout_pct,
    benchmark_pct, gap_pct, duration, convexity, maturity_years, dv01, income_12m, income_100bp, weighs, tags`

const yieldBucketCols = `computed_at, as_of, currency, silver_source_id, account_external_id, position_key,
    curve_currency, tenor, tenor_order, held_in, dv01`

// positions writes a book's holdings and their DV01 per tenor.
func (m *yieldMaterializer) positions(b *yield.Book, at int64, ccy string) error {
	ps := b.Positions
	if err := m.insert("report_yield_positions", yieldPositionCols, len(ps), func(i int, args []any) []any {
		p := ps[i]
		var tags []string
		for _, t := range yield.Tags {
			if p.Tags[t] {
				tags = append(tags, string(t))
			}
		}
		var maturity any
		if p.MaturityDate != nil {
			maturity = p.MaturityDate.Format(time.DateOnly)
		}
		args = append(args, m.computedAt, at, ccy, p.Source, p.HoldingKey.Account, p.PositionKey,
			nullableEnumValue(p.Account.TaxWrapper), nullableEnumValue(p.Symbol), nullableEnumValue(p.Name),
			nullableEnumValue(p.AssetClass), nullableEnumValue(p.Vehicle), string(p.Kind), nullableEnumValue(p.Currency),
			p.Quantity, p.Value, number(p.Price), p.Coupon, maturity, nullableEnumValue(p.Rating))
		received, mean := p.RealizedParts()
		for _, v := range []*float64{p.Stated, p.CurrentYield, p.Implied, p.ImpliedAnnual, p.Realized, &received,
			&mean, p.EarningsYield, p.DividendYield, p.Payout, p.Benchmark, p.Gap, p.Duration, p.Convexity,
			p.Maturity, p.DV01, p.Income12, p.Income100bp} {
			args = append(args, number(v))
		}
		return append(args, p.Weighs(), tags)
	}); err != nil {
		return err
	}
	type bucket struct {
		p          yield.Position
		ccy, tenor string
		order      int
		v          float64
	}
	var bs []bucket
	for _, p := range ps {
		for c, tenors := range p.Buckets {
			for t, v := range tenors {
				if v != 0 {
					bs = append(bs, bucket{p, c, t, slices.Index(canonical.RiskTenors, t) + 1, v})
				}
			}
		}
	}
	return m.insert("report_yield_buckets", yieldBucketCols, len(bs), func(i int, args []any) []any {
		x := bs[i]
		return append(args, m.computedAt, at, ccy, x.p.Source, x.p.HoldingKey.Account, x.p.PositionKey, x.ccy, x.tenor,
			x.order, yield.HeldIn(x.p.Kind), x.v)
	})
}

// fxColumns are a holding's rates to each reporting currency, from the
// books of today, in materializeCurrencies' order.
func fxColumns(today map[string]*yield.Book, k yield.HoldingKey) []any {
	out := make([]any, 0, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		var fx any
		if b := today[ccy]; b != nil {
			if v, ok := b.Projection.FX[k]; ok && v != 0 {
				fx = v
			}
		}
		out = append(out, fx)
	}
	return out
}

// fxColumnNames are fxColumns' columns: fx_<CCY> for each reporting
// currency.
var fxColumnNames = func() string {
	cols := make([]string, len(materializeCurrencies))
	for i, ccy := range materializeCurrencies {
		cols[i] = "fx_" + ccy
	}
	return strings.Join(cols, ", ")
}()

var yieldPartCols = `computed_at, silver_source_id, account_external_id, position_key, holding_currency, rule,
    month, month_start, day, month_only, type, basis, reinvested, fixed, principal, spread, roll_constant,
    roll_forward, roll_principal, withholding_pct, ` + fxColumnNames

var yieldEventCols = `computed_at, silver_source_id, account_external_id, position_key, holding_currency, rule,
    month, day, type, amount, ` + fxColumnNames

// projection writes today's parts, rates and principal events. A
// holding's parts are in its own currency, the same in every book that
// holds it; they come from the first book, in materializeCurrencies'
// order, that does, as a holding no rate converts to a currency drops out
// of that currency's book.
func (m *yieldMaterializer) projection(today map[string]*yield.Book) error {
	type part struct {
		x yield.Part
		b *yield.Book
	}
	owner := map[yield.HoldingKey]*yield.Book{}
	var parts []part
	var events []yield.Event
	type rate struct {
		ccy   string
		month int
		path  yield.Path
		pct   float64
	}
	var rates []rate
	seen := map[string]bool{}
	for _, ccy := range materializeCurrencies {
		b := today[ccy]
		if b == nil {
			continue
		}
		for _, x := range b.Projection.Parts {
			if o, ok := owner[x.Holding]; !ok {
				owner[x.Holding] = b
			} else if o != b {
				continue
			}
			parts = append(parts, part{x, b})
		}
		for _, e := range b.Projection.Events {
			if owner[e.Holding] == b {
				events = append(events, e)
			}
		}
		for _, c := range b.Projection.Currencies() {
			if seen[c] {
				continue
			}
			seen[c] = true
			for _, path := range yield.Paths {
				for mo := 0; mo <= yield.Months; mo++ {
					rates = append(rates, rate{c, mo, path, b.Projection.Rate(c, mo, path)})
				}
			}
		}
	}
	if err := m.insert("report_yield_parts", yieldPartCols, len(parts), func(i int, args []any) []any {
		x, p := parts[i].x, parts[i].b.Projection
		var day any
		if x.Day != nil {
			day = x.Day.Format(time.DateOnly)
		}
		var withholding any
		if w, ok := p.Withholding[x.Holding]; ok {
			withholding = w
		}
		args = append(args, m.computedAt, x.Holding.Source, x.Holding.Account, x.Holding.PositionKey, x.Currency,
			string(x.Rule), x.Month, p.MonthStart(x.Month).Format(time.DateOnly), day, x.MonthOnly, string(x.Type),
			string(x.Basis), nullableEnumValue(x.Reinvested), x.Fixed, x.Principal, x.Spread, x.RollConstant, x.RollForward,
			x.RollPrincipal, withholding)
		return append(args, fxColumns(today, x.Holding)...)
	}); err != nil {
		return err
	}
	if err := m.insert("report_yield_rates", "computed_at, currency, month, path, rate_pct", len(rates),
		func(i int, args []any) []any {
			r := rates[i]
			return append(args, m.computedAt, r.ccy, r.month, string(r.path), r.pct)
		}); err != nil {
		return err
	}
	return m.insert("report_yield_events", yieldEventCols, len(events), func(i int, args []any) []any {
		e := events[i]
		args = append(args, m.computedAt, e.Holding.Source, e.Holding.Account, e.Holding.PositionKey,
			owner[e.Holding].Projection.Currency[e.Holding], string(e.Rule), e.Month, e.Day.Format(time.DateOnly),
			string(e.Type), e.Amount)
		return append(args, fxColumns(today, e.Holding)...)
	})
}

// received writes the income received from the first income line to
// toEpoch, per reporting currency, with what the withholding table
// withholds from it. The whole history, so the Income dashboard's Time
// range reaches as far back as its income lines do.
func (m *yieldMaterializer) received(today map[string]*yield.Book, toEpoch int64) error {
	type line struct {
		ccy string
		r   yield.ReceivedLine
		w   any
	}
	var lines []line
	for _, ccy := range materializeCurrencies {
		b := today[ccy]
		if b == nil {
			continue
		}
		got, err := IncomeReceived(m.ctx, m.tx, b.In, math.MinInt64, toEpoch)
		if err != nil {
			return fmt.Errorf("MaterializeYield: %w", err)
		}
		for _, r := range got {
			var w any
			if pct, ok := b.Projection.Withholding[r.Holding]; ok {
				w = r.Value * pct / 100
			}
			lines = append(lines, line{ccy, r, w})
		}
	}
	return m.insert("report_yield_received",
		"computed_at, currency, silver_source_id, account_external_id, position_key, day, type, value, withheld",
		len(lines), func(i int, args []any) []any {
			x := lines[i]
			return append(args, m.computedAt, x.ccy, x.r.Holding.Source, x.r.Holding.Account, x.r.Holding.PositionKey,
				x.r.Day.Format(time.DateOnly), string(x.r.Type), x.r.Value, x.w)
		})
}

// plan writes the cash flow projection's look-back parts
// (report_cashflow_plan) for each month of today's income projection
// after the current one, per reporting currency: what CashflowPlan sets
// against the income projection, over a look-back of lookback months.
// The other income is written by class and account (OtherIncomeBy),
// which sums to the month's OtherIncome; a look-back with none writes no
// other_income row. The income lines outside the pool are written the
// same way as PlanOutsidePart (CashflowPlanOutside), which the cash flow
// projection does not read.
func (m *yieldMaterializer) plan(today map[string]*yield.Book, toEpoch int64, lookback int) error {
	type part struct {
		ccy, part, class string
		source, account  string
		month            int
		start            time.Time
		value            float64
		quality          string
	}
	var parts []part
	from, to := PlanWindow(time.Unix(toEpoch, 0), lookback)
	// PlanParts, in its order.
	other, out, debt, vehicles := PlanParts[0], PlanParts[1], PlanParts[2], PlanParts[3]
	for _, ccy := range materializeCurrencies {
		b := today[ccy]
		if b == nil {
			continue
		}
		lb, err := CashflowPlanLookback(m.ctx, m.tx, from, to, ccy)
		if err != nil {
			return fmt.Errorf("MaterializeYield: %w", err)
		}
		outside, err := CashflowPlanOutside(m.ctx, m.tx, from, to, ccy)
		if err != nil {
			return fmt.Errorf("MaterializeYield: %w", err)
		}
		outsideKeys := slices.SortedFunc(maps.Keys(outside), comparePlanIncomeKeys)
		for i, r := range CashflowPlan(lb, PlanStanding(b), planMonths(b), 1, false) {
			if r.Total || r.Average {
				continue
			}
			quality := strings.TrimSpace(strings.TrimPrefix(r.Quality, PlanProjected))
			add := func(name, class string, v float64) {
				parts = append(parts, part{ccy, name, class, "", "", i + 1, r.Period, v, quality})
			}
			// CashflowPlan's OtherIncome: the look-back's average each month.
			if lb.Months > 0 {
				for _, k := range lb.IncomeKeys() {
					parts = append(parts, part{ccy, other, k.Class, k.Source, k.Account, i + 1, r.Period,
						lb.OtherIncomeBy[k], quality})
				}
			}
			for _, k := range outsideKeys {
				parts = append(parts, part{ccy, PlanOutsidePart, k.Class, k.Source, k.Account, i + 1, r.Period,
					outside[k], quality})
			}
			for _, c := range PlanOutClasses {
				add(out, c, r.OutClasses[c])
			}
			add(debt, "", r.DebtService)
			add(vehicles, "", r.Vehicles)
		}
	}
	return m.insert("report_cashflow_plan",
		"computed_at, currency, month, month_start, part, class, silver_source_id, account_external_id, value, quality",
		len(parts), func(i int, args []any) []any {
			x := parts[i]
			return append(args, m.computedAt, x.ccy, x.month, x.start.Format(time.DateOnly), x.part, x.class, x.source,
				x.account, x.value, x.quality)
		})
}

// planMonths are the months of b's projection after the current one,
// each by its first day, with no figure: the cash flow projection's
// look-back parts depend on no picker.
func planMonths(b *yield.Book) []PlanMonth {
	months := make([]PlanMonth, yield.Months)
	for i := range months {
		months[i].Start = b.Projection.MonthStart(i + 1)
	}
	return months
}

// number is v, NULL where it is nil or not a number.
func number(v *float64) any {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return *v
}
