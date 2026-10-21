package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// YieldHistoryMonths are the month ends before today the Yield
// dashboard's as-of and over-time cards read.
const YieldHistoryMonths = 12

// yieldInstants are the instants report_yield_positions holds: the last
// second of each of the YieldHistoryMonths months before toEpoch's, then
// toEpoch.
func yieldInstants(toEpoch int64) []int64 {
	d := time.Unix(toEpoch, 0).UTC()
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]int64, 0, YieldHistoryMonths+1)
	for m := YieldHistoryMonths; m >= 1; m-- {
		out = append(out, first.AddDate(0, -m+1, 0).Unix()-1)
	}
	return append(out, toEpoch)
}

// MaterializeYield rewrites the Yield dashboard's tables (migration
// 0138): every holding's figures and DV01 per tenor in every reporting
// currency at yieldInstants, and at toEpoch the projection's parts, the
// rate table, the principals maturities return and the income received
// this year and the last. Each figure is the yield report's (yield.Compute
// over YieldInputs), so the dashboard and the CLI agree. It returns the
// number of rows written. One transaction, so a reader sees the old
// tables or the new ones.
func MaterializeYield(ctx context.Context, db *sql.DB, toEpoch, computedAt int64) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeYield begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	for _, t := range []string{"report_yield_positions", "report_yield_buckets", "report_yield_parts",
		"report_yield_rates", "report_yield_events", "report_yield_received"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("MaterializeYield %s: delete: %w", t, err)
		}
	}
	m := yieldMaterializer{ctx: ctx, db: db, tx: tx, computedAt: computedAt}
	today := map[string]*yield.Book{}
	for _, at := range yieldInstants(toEpoch) {
		for _, ccy := range materializeCurrencies {
			in, err := YieldInputs(ctx, db, at, ccy)
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
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeYield commit: %w", err)
	}
	return m.rows, nil
}

type yieldMaterializer struct {
	ctx        context.Context
	db         *sql.DB
	tx         *sql.Tx
	computedAt int64
	rows       int
}

func (m *yieldMaterializer) insert(table, cols string, n int, row func(i int, args []any) []any) error {
	if n == 0 {
		return nil
	}
	if err := InsertChunked(m.ctx, m.tx, table, `INSERT INTO `+table+` (`+cols+`) VALUES `,
		placeholders(cols), n, row); err != nil {
		return fmt.Errorf("MaterializeYield %s: %w", table, err)
	}
	m.rows += n
	return nil
}

// placeholders is a row of `?` for the comma-separated columns cols.
func placeholders(cols string) string {
	n := 1
	for _, c := range cols {
		if c == ',' {
			n++
		}
	}
	out := "("
	for i := range n {
		if i > 0 {
			out += ", "
		}
		out += "?"
	}
	return out + ")"
}

const yieldPositionCols = `computed_at, as_of, currency, silver_source_id, account_external_id, position_key,
    tax_wrapper, symbol, name, asset_class, vehicle, kind, holding_currency,
    quantity, value, price, coupon_pct, maturity_date, rating, stated_pct, current_yield_pct, implied_pct,
    implied_annual_pct, realized_pct, earnings_yield_pct, dividend_yield_pct, payout_pct, benchmark_pct, gap_pct,
    duration, convexity, maturity_years, dv01, income_12m, income_100bp, weighs, tags`

const yieldBucketCols = `computed_at, as_of, currency, silver_source_id, account_external_id, position_key,
    curve_currency, tenor, kind, dv01`

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
			emptyNull(p.Account.TaxWrapper), emptyNull(p.Symbol), emptyNull(p.Name), emptyNull(p.AssetClass),
			emptyNull(p.Vehicle), string(p.Kind), emptyNull(p.Currency), p.Quantity, p.Value, number(p.Price),
			p.Coupon, maturity, emptyNull(p.Rating))
		for _, v := range []*float64{p.Stated, p.CurrentYield, p.Implied, p.ImpliedAnnual, p.Realized,
			p.EarningsYield, p.DividendYield, p.Payout, p.Benchmark, p.Gap, p.Duration, p.Convexity, p.Maturity,
			p.DV01, p.Income12, p.Income100bp} {
			args = append(args, number(v))
		}
		return append(args, p.Kind != yield.KindLiability && p.Value > 0, tags)
	}); err != nil {
		return err
	}
	type bucket struct {
		p          yield.Position
		ccy, tenor string
		v          float64
	}
	var bs []bucket
	for _, p := range ps {
		for c, tenors := range p.Buckets {
			for t, v := range tenors {
				if v != 0 {
					bs = append(bs, bucket{p, c, t, v})
				}
			}
		}
	}
	return m.insert("report_yield_buckets", yieldBucketCols, len(bs), func(i int, args []any) []any {
		x := bs[i]
		return append(args, m.computedAt, at, ccy, x.p.Source, x.p.HoldingKey.Account, x.p.PositionKey, x.ccy, x.tenor,
			string(x.p.Kind), x.v)
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

const yieldPartCols = `computed_at, silver_source_id, account_external_id, position_key, holding_currency, rule,
    month, month_start, day, month_only, type, basis, reinvested, fixed, principal, spread, roll_constant,
    roll_forward, roll_principal, withholding_pct, fx_USD, fx_CHF, fx_EUR, fx_GBP`

const yieldEventCols = `computed_at, silver_source_id, account_external_id, position_key, holding_currency, rule,
    month, day, type, amount, fx_USD, fx_CHF, fx_EUR, fx_GBP`

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
			string(x.Basis), emptyNull(x.Reinvested), x.Fixed, x.Principal, x.Spread, x.RollConstant, x.RollForward,
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

// received writes the income received from the first day of last year
// to toEpoch, per reporting currency, with what the withholding table
// withholds from it.
func (m *yieldMaterializer) received(today map[string]*yield.Book, toEpoch int64) error {
	d := time.Unix(toEpoch, 0).UTC()
	from := time.Date(d.Year()-1, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
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
		got, err := IncomeReceived(m.ctx, m.db, b.In, from, toEpoch)
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

// emptyNull is s, NULL where it is empty.
func emptyNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// number is v, NULL where it is nil or not a number.
func number(v *float64) any {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return *v
}
