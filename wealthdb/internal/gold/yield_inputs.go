package gold

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// YieldInputs assembles what the yield report and the income projection
// read at the instant asOf, valued in ccy (docs/YIELD.md): the holdings
// as `holdings global` values them, their accounts, terms and rates, the
// funds' figures and lines, the curves, the income of the twelve months
// before, and the reported figures of the securities held.
func YieldInputs(ctx context.Context, db *sql.DB, asOf int64, ccy string) (*yield.Inputs, error) {
	day := time.Unix(asOf, 0).UTC()
	in := &yield.Inputs{AsOf: asOf, Day: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
		Currency: ccy, Accounts: map[yield.AccountKey]yield.Account{}, Terms: map[yield.InstrumentKey]yield.Terms{},
		Rates: map[yield.AccountKey][]yield.Rate{}, Funds: map[string]*yield.Fund{}, Curves: map[string]*yield.Curve{},
		Income: map[yield.HoldingKey]yield.Received{}, AccountIncome: map[yield.AccountCurrency]yield.Received{},
		Payments: map[yield.HoldingKey][]yield.Payment{}, Dividends: map[yield.InstrumentKey]canonical.DeclaredDividend{},
		Securities: map[string]yield.Facts{}, AverageAmount: map[yield.HoldingKey]float64{},
		Withholding: map[string]map[string]float64{}, Situs: map[yield.HoldingKey]string{},
		FXRates: map[string]float64{}}
	steps := []struct {
		name string
		run  func() error
	}{
		{"holdings", func() error { return readYieldHoldings(ctx, db, in) }},
		{"accounts", func() error { return readYieldAccounts(ctx, db, in) }},
		{"terms", func() error { return readYieldTerms(ctx, db, in) }},
		{"rates", func() error { return readYieldRates(ctx, db, in) }},
		{"conventions", func() error {
			c, err := readConventions(ctx, db)
			in.Conventions = c
			return err
		}},
		{"curves", func() error { return readYieldCurves(ctx, db, in) }},
		{"funds", func() error { return readYieldFunds(ctx, db, in) }},
		{"income", func() error { return readYieldIncome(ctx, db, in) }},
		{"dividends", func() error { return readDeclaredDividends(ctx, db, in) }},
		{"securities", func() error { return readSecurityFacts(ctx, db, in) }},
		{"average", func() error { return readAverageAmounts(ctx, db, in) }},
		{"withholding", func() error { return readWithholding(ctx, db, in) }},
		{"fx", func() error { return readYieldFX(ctx, db, in) }},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			return nil, fmt.Errorf("YieldInputs: %s: %w", s.name, err)
		}
	}
	return in, nil
}

func readYieldHoldings(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT h.src, h.acct, h.position_key, COALESCE(h.iid, ''), COALESCE(h.asset_class, ''), COALESCE(h.vehicle, ''),
       COALESCE(h.ccy, ''), h.is_cash, h.value, COALESCE(h.amount, 0), h.quantity, h.accrued, h.acquisition_date,
       COALESCE(h.symbol, ''), COALESCE(h.name, ''), COALESCE(h.key, ''),
       COALESCE(h.key, '`+HoldingKeyPrefix+`' || h.src || ':' || COALESCE(h.iid, h.position_key)), COALESCE(st.situs, '')
  FROM yield_holdings(?, ?) h
  LEFT JOIN exposure_situs_at(?) st ON st.silver_source_id = h.src AND st.account_external_id = h.acct
       AND st.position_key = h.position_key
 WHERE h.value IS NOT NULL
 ORDER BY 1, 2, 3`, func(rows *sql.Rows) error {
		var h yield.Holding
		var qty, accrued sql.NullFloat64
		var acquired sql.NullTime
		var situs string
		if err := rows.Scan(&h.Source, &h.Account, &h.PositionKey, &h.Instrument, &h.AssetClass, &h.Vehicle, &h.Currency,
			&h.IsCash, &h.Value, &h.Amount, &qty, &accrued, &acquired, &h.Symbol, &h.Name, &h.SecurityID, &h.Key,
			&situs); err != nil {
			return err
		}
		h.Quantity, h.Accrued, h.Acquired = nullFloatToPtr(qty), nullFloatToPtr(accrued), nullDate(acquired)
		in.Holdings = append(in.Holdings, h)
		if situs != "" {
			in.Situs[h.HoldingKey] = situs
		}
		return nil
	}, in.AsOf, in.Currency, in.AsOf)
}

func readYieldAccounts(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT a.silver_source_id, a.account_external_id, COALESCE(a.display_name, ''), COALESCE(a.account_kind, ''),
       COALESCE(a.tax_wrapper, ''), COALESCE(a.nickname, ''), COALESCE(a.portfolio_external_id, ''),
       COALESCE(p.display_name, '')
  FROM accounts a
  LEFT JOIN portfolios p ON p.silver_source_id = a.silver_source_id
       AND p.portfolio_external_id = a.portfolio_external_id`, func(rows *sql.Rows) error {
		var k yield.AccountKey
		var a yield.Account
		if err := rows.Scan(&k.Source, &k.Account, &a.Name, &a.Kind, &a.TaxWrapper, &a.Nickname, &a.Portfolio,
			&a.PortfolioName); err != nil {
			return err
		}
		in.Accounts[k] = a
		return nil
	})
}

func readYieldTerms(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, instrument_external_id, coupon_pct, coupon_origin = 'declared', maturity,
       maturity_origin = 'declared', frequency, coupon_kind, day_count, call_date, make_whole, rating, in_default,
       fixed_until, reset_date, duration, yield_pct, terms_as_of
  FROM instrument_terms_at(?)`, func(rows *sql.Rows) error {
		var k yield.InstrumentKey
		var t yield.Terms
		var coupon, duration, yieldPct sql.NullFloat64
		var couponDeclared, maturityDeclared sql.NullBool
		var maturity, call, fixed, reset sql.NullTime
		var frequency, asOf sql.NullInt64
		var kind, dayCount, rating sql.NullString
		if err := rows.Scan(&k.Source, &k.Instrument, &coupon, &couponDeclared, &maturity, &maturityDeclared, &frequency,
			&kind, &dayCount, &call, &t.MakeWhole, &rating, &t.InDefault, &fixed, &reset, &duration, &yieldPct,
			&asOf); err != nil {
			return err
		}
		t.Coupon, t.Duration, t.Yield = nullFloatToPtr(coupon), nullFloatToPtr(duration), nullFloatToPtr(yieldPct)
		t.Maturity, t.CallDate, t.FixedUntil, t.Reset = nullDate(maturity), nullDate(call), nullDate(fixed), nullDate(reset)
		t.CouponDeclared, t.MaturityDeclared = couponDeclared.Bool, maturityDeclared.Bool
		if frequency.Valid {
			f := int(frequency.Int64)
			t.Frequency = &f
		}
		t.CouponKind, t.DayCount, t.Rating = canonical.CouponKind(kind.String), canonical.DayCount(dayCount.String),
			rating.String
		if asOf.Valid {
			d := time.Unix(asOf.Int64, 0).UTC()
			t.TermsAsOf = &d
		}
		in.Terms[k] = t
		return nil
	}, in.AsOf)
}

// readYieldRates reads the account rates in force at the instant: a
// rate read from a snapshot, the latest snapshot's on or before it; a
// rate a source states once, if it holds from on or before the day; of
// either, the newest schedule that holds, every tier of it; a ledger
// record's, the latest
// dated on or before the day, else the one without a date, which
// replaces whatever a source states for that account and currency.
func readYieldRates(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
WITH observed AS (
    SELECT * FROM account_rates
     WHERE observed_at IS NOT NULL AND observed_at <= $1
   QUALIFY observed_at = MAX(observed_at) OVER (PARTITION BY silver_source_id, account_external_id, currency, side)),
once AS (
    SELECT * FROM account_rates WHERE observed_at IS NULL AND origin = 'stated'),
stated AS (
    SELECT * FROM (SELECT * FROM observed UNION ALL SELECT * FROM once)
     WHERE valid_from IS NULL OR valid_from <= CAST(to_timestamp($1) AT TIME ZONE 'UTC' AS DATE)
   QUALIFY rank() OVER (PARTITION BY silver_source_id, account_external_id, currency, side
                        ORDER BY valid_from DESC NULLS LAST, observed_at DESC NULLS LAST) = 1),
declared AS (
    SELECT * FROM account_rates
     WHERE origin = 'declared'
       AND (valid_from IS NULL OR valid_from <= CAST(to_timestamp($1) AT TIME ZONE 'UTC' AS DATE))
   QUALIFY row_number() OVER (PARTITION BY silver_source_id, account_external_id, currency
                              ORDER BY valid_from DESC NULLS LAST, line_no DESC) = 1)
SELECT silver_source_id, account_external_id, COALESCE(currency, ''), side, tier, rate_pct, up_to, valid_from,
       term_end, origin = 'declared'
  FROM (SELECT * FROM declared
        UNION ALL
        SELECT s.* FROM stated s ANTI JOIN declared d ON d.silver_source_id = s.silver_source_id
               AND d.account_external_id = s.account_external_id
               AND COALESCE(d.currency, '') IN (COALESCE(s.currency, ''), ''))
 ORDER BY 1, 2, 3, 4, 5`, func(rows *sql.Rows) error {
		var k yield.AccountKey
		var r yield.Rate
		var side string
		var upTo sql.NullFloat64
		var validFrom, termEnd sql.NullTime
		if err := rows.Scan(&k.Source, &k.Account, &r.Currency, &side, &r.Tier, &r.Pct, &upTo, &validFrom, &termEnd,
			&r.Declared); err != nil {
			return err
		}
		r.Credit, r.UpTo, r.ValidFrom, r.TermEnd = side == "credit", nullFloatToPtr(upTo), nullDate(validFrom), nullDate(termEnd)
		in.Rates[k] = append(in.Rates[k], r)
		return nil
	}, in.AsOf)
}

// benchmarkMonths is the benchmark tenor of a curve that has it: three
// months; a curve without it takes its shortest.
const benchmarkMonths = 3

// curveAgeDays bounds how old a curve's newest observation may be to
// stand for the instant.
const curveAgeDays = 31

// readYieldCurves reads each currency's curve at the instant: the newest
// observation of each tenor's series within curveAgeDays, the basis its
// payload states (fixedincome.ParseBasis: none is the Treasury's), and
// the zero curve built on that basis. The benchmark is the zero curve's
// rate at the benchmark tenor, in the curve's semiannual convention; on
// the Treasury basis that is the tenor as published.
//
// A curve whose tenors state two bases, a basis ParseBasis refuses, or a
// curve on another basis that NewCurve refuses gives no curve at all: its
// rates are of no known convention. A Treasury curve that does not
// bootstrap still gives its benchmark as published. Each refusal logs one
// line naming the currency (warnCurve).
func readYieldCurves(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	type tenors struct {
		points []fixedincome.Point
		basis  fixedincome.Basis
		// refused is why the tenors give no curve: a basis ParseBasis
		// refuses, or a tenor that states another basis than the first.
		refused error
	}
	curves := map[string]*tenors{}
	if err := scanAll(ctx, db, `
SELECT c.currency, c.tenor_months, o.value::DOUBLE,
       coalesce(json_extract_string(o.payload, '$.basis'), ''),
       coalesce(json_extract_string(o.payload, '$.compounding'), '')
  FROM yield_curves c
  JOIN rate_observations o ON o.silver_source_id = c.silver_source_id AND o.series_id = c.series_id
 WHERE o.as_of <= $1 AND o.as_of > $1 - $2 * 86400
QUALIFY row_number() OVER (PARTITION BY c.currency, c.tenor ORDER BY o.as_of DESC) = 1
 ORDER BY 1, 2`, func(rows *sql.Rows) error {
		var ccy, kind, compounding string
		var p fixedincome.Point
		if err := rows.Scan(&ccy, &p.Months, &p.Rate, &kind, &compounding); err != nil {
			return err
		}
		b, err := fixedincome.ParseBasis(kind, compounding)
		t := curves[ccy]
		if t == nil {
			t = &tenors{basis: b}
			curves[ccy] = t
		}
		t.points = append(t.points, p)
		switch {
		case t.refused != nil:
		case err != nil:
			t.refused = err
		case b != t.basis:
			t.refused = fmt.Errorf("its tenors state two bases, %v and %v", t.basis, b)
		}
		return nil
	}, in.AsOf, curveAgeDays); err != nil {
		return err
	}
	for ccy, t := range curves {
		if t.refused != nil {
			warnCurve("yield: no %s curve: %v", ccy, t.refused)
			continue
		}
		i := slices.IndexFunc(t.points, func(p fixedincome.Point) bool { return p.Months == benchmarkMonths })
		if i < 0 {
			i = 0
		}
		c := &yield.Curve{Currency: ccy, Benchmark: t.points[i].Rate, BenchmarkMonths: t.points[i].Months}
		zero, err := fixedincome.NewCurve(t.points, t.basis)
		switch {
		case err != nil && t.basis == fixedincome.Treasury:
			warnCurve("yield: the %s curve does not bootstrap; it gives its benchmark alone: %v", ccy, err)
		case err != nil:
			warnCurve("yield: no %s curve: %v", ccy, err)
			continue
		case t.basis != fixedincome.Treasury:
			c.Zero, c.Benchmark = zero, zero.Zero(c.BenchmarkMonths)
		default:
			c.Zero = zero
		}
		in.Curves[ccy] = c
	}
	return nil
}

// curveWarned holds the curve refusals already logged: one instant's
// refusal is every instant's, and a refresh reads many.
var curveWarned = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// logCurve is where warnCurve writes; a test swaps it.
var logCurve = log.Printf

// warnCurve logs a refused curve's line once per process.
func warnCurve(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	curveWarned.Lock()
	defer curveWarned.Unlock()
	if !curveWarned.seen[line] {
		curveWarned.seen[line] = true
		logCurve("%s", line)
	}
}

// fundFields maps the fund_characteristics fields the report reads to
// the figure each fills.
var fundFields = map[string]func(f *yield.Fund) **yield.Dated{
	"yield_to_maturity":               func(f *yield.Fund) **yield.Dated { return &f.YieldToMaturity },
	"yield_to_worst":                  func(f *yield.Fund) **yield.Dated { return &f.YieldToWorst },
	"effective_duration":              func(f *yield.Fund) **yield.Dated { return &f.Duration },
	"sec_yield":                       func(f *yield.Fund) **yield.Dated { return &f.SECYield },
	"distribution_yield":              func(f *yield.Fund) **yield.Dated { return &f.DistributionYield },
	"trailing_yield":                  func(f *yield.Fund) **yield.Dated { return &f.TrailingYield },
	"seven_day_yield":                 func(f *yield.Fund) **yield.Dated { return &f.SevenDayYield },
	"weighted_average_maturity_years": func(f *yield.Fund) **yield.Dated { return &f.MaturityYears },
	"weighted_average_maturity":       func(f *yield.Fund) **yield.Dated { return &f.MaturityYears },
	"price_earnings":                  func(f *yield.Fund) **yield.Dated { return &f.PriceEarnings },
}

// readYieldFunds reads, for each fund held directly, its newest figure
// of each field on or before the instant (within the exposure's
// max_gap_days), its newest filed rate sensitivity with the net assets of
// that filing, and what the lines of its composition at the instant say.
func readYieldFunds(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	held := map[string]bool{}
	for _, h := range in.Holdings {
		if (h.Vehicle == "etf" || h.Vehicle == "fund") && h.SecurityID != "" {
			held[h.SecurityID] = true
			in.Funds[h.SecurityID] = &yield.Fund{}
		}
	}
	if len(held) == 0 {
		return nil
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range held {
			if !yield(k) {
				return
			}
		}
	})
	list := "[" + strings.Repeat("?, ", len(keys)-1) + "?]"
	args := func(more ...any) []any {
		out := make([]any, 0, len(keys)+len(more))
		for _, k := range keys {
			out = append(out, k)
		}
		return append(out, more...)
	}
	fields := slices.Sorted(func(yield func(string) bool) {
		for f := range fundFields {
			if !yield(f) {
				return
			}
		}
	})
	if err := scanAll(ctx, db, `
SELECT exposure_key(fund_ref) AS key, field, value::DOUBLE, as_of
  FROM fund_characteristics, exposure_settings_row() s
 WHERE label = '' AND list_contains(`+list+`, exposure_key(fund_ref)) AND as_of <= ?
   AND (? // 86400) - (as_of // 86400) <= s.max_gap_days AND field IN ('`+strings.Join(fields, "', '")+`')
QUALIFY row_number() OVER (PARTITION BY key, field ORDER BY as_of DESC, provider) = 1`, func(rows *sql.Rows) error {
		var key, field string
		var v float64
		var asOf int64
		if err := rows.Scan(&key, &field, &v, &asOf); err != nil {
			return err
		}
		if field == "weighted_average_maturity" {
			v /= 365.25 // days, as a money-market filing states it
		}
		// Two fields may fill one figure: the newer wins.
		if p, day := fundFields[field](in.Funds[key]), time.Unix(asOf, 0).UTC(); *p == nil || (*p).Day.Before(day) {
			*p = &yield.Dated{Value: v, Day: day}
		}
		return nil
	}, args(in.AsOf, in.AsOf)...); err != nil {
		return err
	}
	if err := scanAll(ctx, db, `
WITH r AS (
    SELECT exposure_key(r.fund_ref) AS key, r.silver_source_id, r.fund_ref, r.as_of, r.currency, r.tenor, r.dv01::DOUBLE AS dv01
      FROM fund_risk r, exposure_settings_row() s
     WHERE list_contains(`+list+`, exposure_key(r.fund_ref)) AND r.as_of <= ?
       AND (? // 86400) - (r.as_of // 86400) <= s.max_gap_days AND r.dv01 IS NOT NULL
   QUALIFY r.as_of = MAX(r.as_of) OVER (PARTITION BY exposure_key(r.fund_ref)))
SELECT r.key, r.as_of, r.currency, r.tenor, r.dv01, MAX(c.net_assets)::DOUBLE
  FROM r
  LEFT JOIN fund_compositions c ON c.silver_source_id = r.silver_source_id AND c.fund_ref = r.fund_ref
       AND c.as_of = r.as_of
 GROUP BY ALL`, func(rows *sql.Rows) error {
		var key, ccy, tenor string
		var asOf int64
		var dv01 float64
		var net sql.NullFloat64
		if err := rows.Scan(&key, &asOf, &ccy, &tenor, &dv01, &net); err != nil {
			return err
		}
		f := in.Funds[key]
		if f.Risk == nil {
			f.Risk = map[string]map[string]float64{}
			d := time.Unix(asOf, 0).UTC()
			f.RiskAsOf, f.NetAssets = &d, nullFloatToPtr(net)
		}
		if f.Risk[ccy] == nil {
			f.Risk[ccy] = map[string]float64{}
		}
		f.Risk[ccy][tenor] = dv01
		return nil
	}, args(in.AsOf, in.AsOf)...); err != nil {
		return err
	}
	return readFundLines(ctx, db, in, list, args)
}

// fundLine is one line of a fund's composition, as far as its yield and
// duration go.
type fundLine struct {
	key                                        string
	weight, value, quantity                    float64
	ccy                                        string
	maturity                                   *time.Time
	coupon, rate, ytm, ytw, duration, modified *float64
	kind                                       string
	defaulted, inKind                          bool
}

// readFundLines summarises each fund's lines at the instant
// (yield.LineSummary): the yield and the duration their prices imply,
// weighted by value. A line's own stated yield and duration win; else
// its price implies them, from its coupon, maturity and face (its
// balance), its value read in its own currency at the exchange rate it
// was valued at. A floating line, a line in default and one whose
// coupon is paid in kind imply nothing; their shares are counted.
func readFundLines(ctx context.Context, db *sql.DB, in *yield.Inputs, list string, args func(...any) []any) error {
	type comp struct {
		asOf  int64
		aged  bool
		lines []fundLine
	}
	comps := map[string]*comp{}
	if err := scanAll(ctx, db, `
SELECT fa.fund_ref, fa.as_of, fa.aged, COALESCE(exposure_key(l.security_key), ''), COALESCE(l.weight, 0)::DOUBLE,
       COALESCE(l.market_value, 0)::DOUBLE,
       COALESCE(l.quantity, 0)::DOUBLE, COALESCE(l.currency, ''), l.maturity, l.coupon::DOUBLE,
       l.exchange_rate::DOUBLE, l.yield_to_maturity::DOUBLE, l.yield_to_worst::DOUBLE, l.duration::DOUBLE,
       l.modified_duration::DOUBLE,
       COALESCE(l.coupon_kind, ''), COALESCE(l.is_default OR l.in_arrears, FALSE), COALESCE(l.paid_in_kind, FALSE)
  FROM exposure_funds_at(?) fa
  JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
       AND l.as_of = fa.as_of AND l.basis = fa.basis
 WHERE list_contains(`+list+`, fa.fund_ref)`, func(rows *sql.Rows) error {
		var key string
		var c comp
		var l fundLine
		var maturity sql.NullTime
		var coupon, rate, ytm, ytw, dur, mod sql.NullFloat64
		if err := rows.Scan(&key, &c.asOf, &c.aged, &l.key, &l.weight, &l.value, &l.quantity, &l.ccy, &maturity, &coupon,
			&rate, &ytm, &ytw, &dur, &mod, &l.kind, &l.defaulted, &l.inKind); err != nil {
			return err
		}
		l.maturity, l.coupon, l.rate = nullDate(maturity), nullFloatToPtr(coupon), nullFloatToPtr(rate)
		l.ytm, l.ytw, l.duration, l.modified = nullFloatToPtr(ytm), nullFloatToPtr(ytw), nullFloatToPtr(dur),
			nullFloatToPtr(mod)
		if comps[key] == nil {
			comps[key] = &comp{asOf: c.asOf, aged: c.aged}
		}
		comps[key].lines = append(comps[key].lines, l)
		return nil
	}, append([]any{in.AsOf}, args()...)...); err != nil {
		return err
	}
	// A security's line is the one of the most value held: its weight
	// times the value of the fund that holds it.
	held := map[string]float64{}
	for _, h := range in.Holdings {
		held[h.SecurityID] += math.Abs(h.Value)
	}
	in.Lines = map[string]yield.LineInfo{}
	most := map[string]float64{}
	for key, c := range comps {
		day := time.Unix(c.asOf, 0).UTC()
		infos := make([]yield.LineInfo, len(c.lines))
		for i, l := range c.lines {
			infos[i] = readLine(in, l, day)
			if v := math.Abs(l.weight) * held[key]; l.key != "" && v >= most[l.key] {
				in.Lines[l.key], most[l.key] = infos[i], v
			}
		}
		in.Funds[key].Lines = summarizeLines(c.lines, infos, day, c.aged)
	}
	return nil
}

// readLine is what one fund line says of its security on day: its price
// in its own currency (per 100 of face for a line with a coupon and a
// maturity, per unit otherwise, its value read at the exchange rate it
// was valued at), and the yield (to worst where it states one) and
// modified duration it states, else those its price implies. A line in
// default or arrears, or paid in kind, implies no yield.
func readLine(in *yield.Inputs, l fundLine, day time.Time) yield.LineInfo {
	info := yield.LineInfo{Coupon: l.coupon, Maturity: l.maturity, CouponKind: l.kind, Currency: l.ccy,
		Duration: firstFloat(l.modified, l.duration), Default: l.defaulted}
	if !l.defaulted && !l.inKind {
		info.Yield = firstFloat(l.ytw, l.ytm)
	}
	if l.quantity <= 0 || l.value <= 0 {
		return info
	}
	rate := 1.0
	if l.rate != nil && *l.rate > 0 {
		rate = *l.rate
	}
	debt := l.coupon != nil && l.maturity != nil
	price := l.value * rate / l.quantity
	if debt {
		price *= 100
	}
	info.Price = &price
	if !debt || !l.maturity.After(day) || info.Yield != nil || !(price > 1 && price < 1000) ||
		l.kind == "Floating" || l.kind == "Variable" || l.defaulted || l.inKind {
		return info
	}
	f := 2
	if c, ok := in.Conventions[l.ccy]; ok && c.Frequency > 0 {
		f = c.Frequency
	}
	b := fixedincome.Bond{Coupon: *l.coupon, Frequency: f, Maturity: *l.maturity, DayCount: canonical.DayCountActAct}
	if v, err := b.Yield(day, price+b.Accrued(day)); err == nil {
		info.Yield = &v
		if info.Duration == nil {
			r := b.Risk(day, v).Modified
			info.Duration = &r
		}
	}
	return info
}

// summarizeLines is yield.LineSummary of a fund's lines on day, given
// what each says of its security (readLine).
func summarizeLines(lines []fundLine, infos []yield.LineInfo, day time.Time, aged bool) *yield.LineSummary {
	s := &yield.LineSummary{AsOf: day, Aged: aged}
	var total, wy, sy, wd, sd, wm, sm float64
	for i, l := range lines {
		w := math.Abs(l.weight)
		total += w
		switch {
		case l.kind == "Floating" || l.kind == "Variable":
			s.FloatingPct += w
			continue
		case l.defaulted:
			s.DefaultPct += w
			continue
		case l.inKind:
			s.PaidInKindPct += w
			continue
		}
		if l.maturity == nil || !l.maturity.After(day) {
			continue
		}
		wm += w * (l.maturity.Sub(day).Hours() / 24 / 365.25)
		sm += w
		y, dur := infos[i].Yield, infos[i].Duration
		if y != nil {
			wy += w * *y
			sy += w
		}
		if dur != nil {
			wd += w * *dur
			sd += w
		}
	}
	if total == 0 {
		return s
	}
	if sy > 0 {
		s.Yield = f64(wy / sy)
	}
	if sd > 0 {
		s.Duration = f64(wd / sd)
	}
	if sm > 0 {
		s.MaturityYears = f64(wm / sm)
	}
	scale := 100 / total
	s.CoveredPct = sy * scale
	s.FloatingPct *= scale
	s.DefaultPct *= scale
	s.PaidInKindPct *= scale
	return s
}

func firstFloat(vs ...*float64) *float64 {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// incomeTypeSQL maps an income line to the projection's types, by its
// detailed type and the instrument it names (i, its terms tm): interest
// a bond pays is a coupon, but a bill's or a zero's discount is
// interest; a dividend a fund pays is a distribution.
const incomeTypeSQL = `CASE
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' AND i.vehicle = 'bond'
         AND NOT COALESCE(tm.coupon_kind = 'zero' OR tm.coupon_pct = 0, FALSE) THEN 'coupons'
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' THEN 'interest'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' AND i.vehicle IN ('etf', 'fund') THEN 'distributions'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' THEN 'dividends'
    WHEN l.income_detailed = 'INCOME_DISTRIBUTIONS' THEN 'distributions'
    WHEN l.income_detailed = 'INCOME_RENTAL' THEN 'rent'
    WHEN l.income_detailed = 'INCOME_RETIREMENT_PENSION' THEN 'pension' END`

// readYieldIncome reads the income of the twelve months to the instant,
// in the output currency, per holding (yield.Inputs.HoldingOf), and per
// account where a line names no instrument; and the payments of the 24
// months to it per holding, in the line's currency, each with the
// quantity held on its day.
func readYieldIncome(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	yearAgo := in.Day.AddDate(-1, 0, 0)
	return incomeLines(ctx, db, in.Day.AddDate(-2, 0, 0).Unix(), in.AsOf, in.Currency, func(l IncomeLine) {
		day := time.Unix(l.At, 0).UTC()
		key, held := in.HoldingOf(l.Source, l.Account, l.Instrument)
		recent := day.After(yearAgo)
		switch {
		case held:
			if recent && l.Value != nil {
				if in.Income[key] == nil {
					in.Income[key] = yield.Received{}
				}
				in.Income[key][l.Type] += *l.Value
			}
			in.Payments[key] = append(in.Payments[key], yield.Payment{Day: day, Amount: l.Amount,
				Quantity: l.Quantity, Type: l.Type})
		case l.Instrument == "" && recent:
			ac := yield.AccountCurrency{AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account},
				Currency: l.Currency}
			if l.Value != nil {
				if in.AccountIncome[ac] == nil {
					in.AccountIncome[ac] = yield.Received{}
				}
				in.AccountIncome[ac][l.Type] += *l.Value
			}
			if l.Type == yield.TypeRent || l.Type == yield.TypePension || l.Type == yield.TypeInterest {
				in.AccountPayments = append(in.AccountPayments, yield.AccountPayment{AccountCurrency: ac, Day: day,
					Amount: l.Amount, Type: l.Type})
			}
		}
	})
}

// IncomeLine is one income transaction of a type the projection counts:
// its amount in its currency, its value in the output currency (nil
// where no rate converts it), and the quantity of the instrument it
// names held on its day.
type IncomeLine struct {
	Source, Account, Instrument, Currency string
	At                                    int64
	Type                                  yield.IncomeType
	Amount                                float64
	Value, Quantity                       *float64
}

// incomeLines calls fn with each income line of [from, to], oldest
// first: an income transaction of a type the projection counts, in ccy
// at its day's rate.
func incomeLines(ctx context.Context, db *sql.DB, from, to int64, ccy string, fn func(IncomeLine)) error {
	return scanAll(ctx, db, `
WITH l AS (
    SELECT l.silver_source_id, l.account_external_id, t.instrument_external_id, l.transaction_external_id,
           l.occurred_at, l.currency, l.net_amount::DOUBLE AS amount, `+incomeTypeSQL+` AS type
      FROM income_lines_base(?, ?) l
      JOIN transactions t ON t.silver_source_id = l.silver_source_id
           AND t.transaction_external_id = l.transaction_external_id
      LEFT JOIN instruments i ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
      LEFT JOIN instrument_terms tm ON tm.silver_source_id = t.silver_source_id
           AND tm.instrument_external_id = t.instrument_external_id),
fx AS (SELECT * FROM fx_rates_to(?))
SELECT l.silver_source_id, l.account_external_id, COALESCE(l.instrument_external_id, ''), l.occurred_at,
       l.currency, l.amount, l.type,
       CASE WHEN l.currency = ? THEN l.amount ELSE l.amount * fx.rate END AS value,
       p.quantity::DOUBLE
  FROM l
  ASOF LEFT JOIN fx ON fx.from_ccy = l.currency AND fx.day <= l.occurred_at // 86400
  ASOF LEFT JOIN positions p ON p.silver_source_id = l.silver_source_id
       AND p.account_external_id = l.account_external_id
       AND p.instrument_external_id = l.instrument_external_id AND p.snapshot_at <= l.occurred_at
 WHERE l.type IS NOT NULL
 ORDER BY l.occurred_at, l.silver_source_id, l.account_external_id, l.transaction_external_id`, func(rows *sql.Rows) error {
		var l IncomeLine
		var typ string
		var value, qty sql.NullFloat64
		if err := rows.Scan(&l.Source, &l.Account, &l.Instrument, &l.At, &l.Currency, &l.Amount, &typ, &value,
			&qty); err != nil {
			return err
		}
		l.Type, l.Value, l.Quantity = yield.IncomeType(typ), nullFloatToPtr(value), nullFloatToPtr(qty)
		fn(l)
		return nil
	}, from, to, ccy, ccy)
}

// IncomeReceived is every income line of [from, to] that the income
// projection counts as received, each named by the holding of the book
// it belongs to (yield.Inputs.HoldingOf), else by its account: a
// payment of interest by the account's cash in its currency, any other
// by the account and its type.
func IncomeReceived(ctx context.Context, db *sql.DB, in *yield.Inputs, from, to int64) ([]yield.ReceivedLine, error) {
	var out []yield.ReceivedLine
	err := incomeLines(ctx, db, from, to, in.Currency, func(l IncomeLine) {
		r := yield.ReceivedLine{Day: time.Unix(l.At, 0).UTC(), Type: l.Type}
		if l.Value != nil {
			r.Value = *l.Value
		}
		key, held := in.HoldingOf(l.Source, l.Account, l.Instrument)
		switch {
		case held:
			r.Holding = key
		case l.Type == yield.TypeInterest && l.Instrument == "":
			r.Holding = yield.HoldingKey{Source: l.Source, Account: l.Account, PositionKey: "cash:" + l.Currency}
		default:
			r.Holding = yield.AccountIncomeKey(yield.AccountCurrency{
				AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account}, Currency: l.Currency}, l.Type)
			if l.Instrument != "" {
				r.Holding.PositionKey = l.Instrument
			}
		}
		out = append(out, r)
	})
	if err != nil {
		return nil, fmt.Errorf("IncomeReceived: %w", err)
	}
	return out, nil
}

func readDeclaredDividends(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, instrument_external_id, amount, frequency, COALESCE(currency, '')
  FROM yield_declared_dividends
 WHERE as_of IS NULL OR as_of <= ?
QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                           ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1`, func(rows *sql.Rows) error {
		var k yield.InstrumentKey
		var d canonical.DeclaredDividend
		if err := rows.Scan(&k.Source, &k.Instrument, &d.Amount, &d.Frequency, &d.Currency); err != nil {
			return err
		}
		in.Dividends[k] = d
		return nil
	}, in.AsOf)
}

// periodFact is one reported figure for a period.
type periodFact struct {
	start, end time.Time
	value      float64
}

// readSecurityFacts reads the reported figures of each stock held
// directly and gives it the trailing twelve months of each: the last
// four quarters, a quarter a year-to-date figure implies taken as the
// difference from the one before it, the fourth as the fiscal year less
// the nine months; a year earlier too, for growth.
func readSecurityFacts(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	held := map[string]bool{}
	for _, h := range in.Holdings {
		if h.Vehicle == "stock" && h.SecurityID != "" {
			held[h.SecurityID] = true
		}
	}
	return YieldSecurityFacts(ctx, db, in, held)
}

// YieldSecurityFacts gives each security of keys its trailing twelve
// months of each reported figure (yield.Facts), into in.Securities. A
// figure counts by its period's end: one a later filing restated reads
// restated, on any day.
func YieldSecurityFacts(ctx context.Context, db *sql.DB, in *yield.Inputs, keys map[string]bool) error {
	if len(keys) == 0 {
		return nil
	}
	facts := map[string]map[string][]periodFact{}
	units := map[string]string{}
	if err := scanAll(ctx, db, `
SELECT exposure_key(security_key) AS key, field, period_start, as_of, value::DOUBLE, COALESCE(unit, '')
  FROM security_period_facts
 WHERE as_of <= ? AND field IN ('eps_diluted', 'dividends_per_share', 'operating_income', 'interest_expense')
 ORDER BY 1, 2, 4, 3`, func(rows *sql.Rows) error {
		var key, field, unit string
		var start time.Time
		var end int64
		var v float64
		if err := rows.Scan(&key, &field, &start, &end, &v, &unit); err != nil {
			return err
		}
		if !keys[key] {
			return nil
		}
		if facts[key] == nil {
			facts[key] = map[string][]periodFact{}
		}
		facts[key][field] = append(facts[key][field], periodFact{start: start.UTC(), end: time.Unix(end, 0).UTC(), value: v})
		if field == "eps_diluted" && unit != "" {
			units[key] = unit
		}
		return nil
	}, in.AsOf); err != nil {
		return err
	}
	for key, byField := range facts {
		var f yield.Facts
		f.Currency, _, _ = strings.Cut(units[key], "/")
		if eps, ok := trailing(byField["eps_diluted"], in.Day, 0); ok {
			f.EPS = &eps
			if v, ok := trailing(byField["eps_diluted"], in.Day, 1); ok {
				f.EPSYearAgo = &v
			}
		}
		if v, ok := trailing(byField["dividends_per_share"], in.Day, 0); ok {
			f.DPS = &v
			if w, ok := trailing(byField["dividends_per_share"], in.Day, 1); ok {
				f.DPSYearAgo = &w
			}
		}
		if v, ok := trailing(byField["operating_income"], in.Day, 0); ok {
			f.OperatingIncome = &v
		}
		if v, ok := trailing(byField["interest_expense"], in.Day, 0); ok {
			f.InterestExpense = &v
		}
		in.Securities[key] = f
	}
	return nil
}

// staleDays bounds the age of the newest quarter a trailing figure may
// end on: a quarter and the 45 days a quarterly report may take.
const staleDays = 135

// trailing is the trailing twelve months of a reported figure as of day,
// years back: the sum of the four quarters ending with the newest one
// (no older than staleDays), each quarter a three-month period, the
// difference of two year-to-date periods that start on one day, or a
// year less the nine months to its third quarter; else the newest
// fiscal year ending within a quarter of the newest quarter. ok is false
// where neither is known.
func trailing(periods []periodFact, day time.Time, years int) (float64, bool) {
	at := day.AddDate(-years, 0, 0)
	months := func(p periodFact) int {
		return int(math.Round(p.end.Sub(p.start).Hours() / 24 / 30.44))
	}
	quarters := map[time.Time]float64{} // by end
	byStart := map[time.Time][]periodFact{}
	for _, p := range periods {
		if p.end.After(at) {
			continue
		}
		switch months(p) {
		case 3:
			quarters[p.end] = p.value
		case 6, 9, 12:
			byStart[p.start] = append(byStart[p.start], p)
		}
	}
	// Year-to-date periods that start on one day differ by a quarter.
	for _, ps := range byStart {
		slices.SortFunc(ps, func(a, b periodFact) int { return a.end.Compare(b.end) })
		for i := range ps {
			prevEnd, prevValue := ps[i].start, 0.0
			if i > 0 {
				prevEnd, prevValue = ps[i-1].end, ps[i-1].value
			} else if q, ok := quarters[ps[i].start.AddDate(0, 3, -1)]; ok && months(ps[i]) == 6 {
				prevEnd, prevValue = ps[i].start.AddDate(0, 3, -1), q
			} else {
				continue
			}
			if m := int(math.Round(ps[i].end.Sub(prevEnd).Hours() / 24 / 30.44)); m == 3 {
				if _, ok := quarters[ps[i].end]; !ok {
					quarters[ps[i].end] = ps[i].value - prevValue
				}
			}
		}
	}
	ends := make([]time.Time, 0, len(quarters))
	for e := range quarters {
		ends = append(ends, e)
	}
	slices.SortFunc(ends, func(a, b time.Time) int { return b.Compare(a) })
	if len(ends) >= 4 && at.Sub(ends[0]) <= staleDays*24*time.Hour {
		sum := 0.0
		ok := true
		for i := 0; i < 4; i++ {
			if i > 0 && math.Round(ends[i-1].Sub(ends[i]).Hours()/24/30.44) != 3 {
				ok = false
				break
			}
			sum += quarters[ends[i]]
		}
		if ok {
			return sum, true
		}
	}
	var year *periodFact
	for _, ps := range byStart {
		for i := range ps {
			p := ps[i]
			if months(p) == 12 && (year == nil || p.end.After(year.end)) {
				year = &p
			}
		}
	}
	if year != nil && at.Sub(year.end) <= (staleDays+92)*24*time.Hour {
		return year.value, true
	}
	return 0, false
}

// readAverageAmounts reads each holding's average amount, in its own
// currency, at the last second of each of the twelve months to the
// instant and at the instant itself: a position as positions_at reads it,
// a cash balance as cash_chosen does. A month the holding was not held
// counts as zero.
func readAverageAmounts(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	instants := make([]any, 0, 13)
	for m := 0; m < 12; m++ {
		end := time.Date(in.Day.Year(), in.Day.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -m, 0).Unix() - 1
		instants = append(instants, end)
	}
	instants = append(instants, in.AsOf)
	list := "[" + strings.Repeat("?::BIGINT, ", len(instants)-1) + "?::BIGINT]"
	sums := map[yield.HoldingKey]float64{}
	if err := scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, position_key, SUM(market_value)::DOUBLE
  FROM positions_at(`+list+`)
 GROUP BY ALL`, func(rows *sql.Rows) error {
		var k yield.HoldingKey
		var v sql.NullFloat64
		if err := rows.Scan(&k.Source, &k.Account, &k.PositionKey, &v); err != nil {
			return err
		}
		sums[k] = v.Float64
		return nil
	}, instants...); err != nil {
		return err
	}
	for _, at := range instants {
		if err := scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, 'cash:' || currency, amount::DOUBLE FROM cash_chosen(?)`,
			func(rows *sql.Rows) error {
				var k yield.HoldingKey
				var v float64
				if err := rows.Scan(&k.Source, &k.Account, &k.PositionKey, &v); err != nil {
					return err
				}
				sums[k] += v
				return nil
			}, at); err != nil {
			return err
		}
	}
	for _, h := range in.Holdings {
		if s, ok := sums[h.HoldingKey]; ok {
			in.AverageAmount[h.HoldingKey] = s / float64(len(instants))
		}
	}
	return nil
}

func readWithholding(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `SELECT country, wrapper, rate_pct FROM yield_withholding`, func(rows *sql.Rows) error {
		var country, wrapper string
		var pct float64
		if err := rows.Scan(&country, &wrapper, &pct); err != nil {
			return err
		}
		if in.Withholding[country] == nil {
			in.Withholding[country] = map[string]float64{}
		}
		in.Withholding[country][wrapper] = pct
		return nil
	})
}

// readYieldFX reads the rate from each currency gold holds figures in to
// the output currency, on the instant's day.
func readYieldFX(ctx context.Context, db *sql.DB, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT from_ccy, rate FROM fx_rates_to(?) WHERE day <= ? // 86400 AND rate IS NOT NULL
QUALIFY row_number() OVER (PARTITION BY from_ccy ORDER BY day DESC) = 1`, func(rows *sql.Rows) error {
		var ccy string
		var rate float64
		if err := rows.Scan(&ccy, &rate); err != nil {
			return err
		}
		in.FXRates[ccy] = rate
		return nil
	}, in.Currency, in.AsOf)
}
