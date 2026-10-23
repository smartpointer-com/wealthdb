package gold

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log"
	"maps"
	"math"
	"slices"
	"sort"
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
func YieldInputs(ctx context.Context, db querier, asOf int64, ccy string) (*yield.Inputs, error) {
	base, err := readYieldBase(ctx, db, asOf, &yieldShared{})
	if err != nil {
		return nil, err
	}
	return base.inputs(ctx, db, ccy)
}

// yieldBase is what the inputs of one instant read whatever the output
// currency: the accounts, terms, rates, conventions and curves, the
// ledger's dividends, the withholding table, the aliases and the
// instruments held, each holding's amount on each day of the twelve
// months to the instant, the reported figures of the stocks a currency's
// inputs have already read, and the units of the instruments paid. Every
// currency's inputs share its maps, and none changes them except
// Securities: each currency's inputs add the stocks no earlier one read
// (asked).
type yieldBase struct {
	in *yield.Inputs
	// amounts is each position's amount day by day, in each currency it
	// is valued in (readAmounts), and instrument the instrument of each.
	amounts    map[amountKey]*yield.Daily
	instrument map[yield.HoldingKey]string
	// lag is the lag of the record day of the dividends of each instrument
	// held, where its market fixes one (readYieldHeld).
	lag map[yield.InstrumentKey]yield.Lag
	// asked are the securities whose reported figures were read.
	asked map[string]bool
	// units are the units of the instruments the income lines pay, which
	// the first currency's inputs read (readIncome).
	units *yieldUnits
}

// yieldShared is what the bases of several instants read once: the
// positions the situs rule reads as receipts, which no instant changes.
type yieldShared struct {
	receipts map[[3]string]bool
}

// readYieldBase reads the instant asOf's yieldBase. The positions read
// as receipts come from shared, read on the first instant that needs
// them.
func readYieldBase(ctx context.Context, db querier, asOf int64, shared *yieldShared) (*yieldBase, error) {
	day := time.Unix(asOf, 0).UTC()
	in := &yield.Inputs{AsOf: asOf, Day: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
		Accounts: map[yield.AccountKey]yield.Account{}, Terms: map[yield.InstrumentKey]yield.Terms{},
		Rates: map[yield.AccountKey][]yield.Rate{}, Curves: map[string]*yield.Curve{},
		Dividends: map[yield.InstrumentKey]canonical.DeclaredDividend{}, Securities: map[string]yield.Facts{},
		Withholding: map[string]map[string]float64{}, Held: map[yield.InstrumentKey][]string{},
		HeldSecurity: map[yield.InstrumentKey]string{}, Aliases: map[yield.AliasKey]string{}}
	b := &yieldBase{in: in, asked: map[string]bool{}, lag: map[yield.InstrumentKey]yield.Lag{}}
	if err := runSteps("YieldInputs", []step{
		{"accounts", func() error { return readYieldAccounts(ctx, db, in) }},
		{"terms", func() error { return readYieldTerms(ctx, db, in) }},
		{"rates", func() error { return readYieldRates(ctx, db, in) }},
		{"conventions", func() error {
			c, err := readConventions(ctx, db)
			in.Conventions = c
			return err
		}},
		{"curves", func() error { return readYieldCurves(ctx, db, in) }},
		{"dividends", func() error { return readDeclaredDividends(ctx, db, in) }},
		{"daily", func() error { return b.readAmounts(ctx, db) }},
		{"aliases", func() error { return readYieldAliases(ctx, db, in) }},
		{"held", func() error { return readYieldHeld(ctx, db, in, b.lag, shared) }},
		{"withholding", func() error { return readWithholding(ctx, db, in) }},
	}); err != nil {
		return nil, err
	}
	return b, nil
}

// inputs are the base's instant valued in ccy: the base's figures, the
// holdings as `holdings global` values them, the funds held, the income
// of the twelve months before, the reported figures of the stocks held,
// each holding's daily amounts and the exchange rates.
func (b *yieldBase) inputs(ctx context.Context, db querier, ccy string) (*yield.Inputs, error) {
	in := *b.in
	in.Currency = ccy
	in.Funds, in.Income, in.AccountIncome = map[string]*yield.Fund{}, map[yield.HoldingKey]yield.Received{},
		map[yield.AccountCurrency]yield.Received{}
	in.Payments, in.Daily = map[yield.HoldingKey][]yield.Payment{}, map[yield.HoldingKey]yield.Daily{}
	in.FormerDaily = map[yield.InstrumentKey][]yield.Daily{}
	in.Situs, in.FXRates = map[yield.HoldingKey]string{}, map[string]float64{}
	if err := runSteps("YieldInputs", []step{
		{"holdings", func() error { return readYieldHoldings(ctx, db, &in) }},
		{"funds", func() error { return readYieldFunds(ctx, db, &in) }},
		{"income", func() error { return b.readIncome(ctx, db, &in) }},
		{"securities", func() error { return b.readSecurities(ctx, db, &in) }},
		{"fx", func() error { return readYieldFX(ctx, db, &in) }},
		{"daily", func() error {
			b.daily(&in)
			return nil
		}},
	}); err != nil {
		return nil, err
	}
	return &in, nil
}

func readYieldHoldings(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT h.src, h.acct, h.position_key, COALESCE(h.iid, ''), COALESCE(h.asset_class, ''), COALESCE(h.vehicle, ''),
       COALESCE(h.ccy, ''), h.is_cash, h.value, COALESCE(h.amount, 0), h.quantity, h.accrued, h.acquisition_date,
       COALESCE(h.symbol, ''), COALESCE(h.name, ''), COALESCE(h.key, ''),
       COALESCE(h.key, '`+HoldingKeyPrefix+`' || h.src || ':' || COALESCE(h.iid, h.position_key)), COALESCE(st.situs, '')
  FROM yield_holdings(?, ?) h
  LEFT JOIN exposure_situs_at(?) st ON st.silver_source_id = h.src AND st.account_external_id = h.acct
       AND st.position_key = h.position_key
 WHERE h.value IS NOT NULL AND ABS(COALESCE(h.amount, 0)) >= 0.005
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

func readYieldAccounts(ctx context.Context, db querier, in *yield.Inputs) error {
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

func readYieldTerms(ctx context.Context, db querier, in *yield.Inputs) error {
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

// readYieldRates reads the account rates in force at the instant:
//
//   - a rate read from a snapshot: the latest snapshot's on or before it;
//   - a rate a source states once: if it holds from on or before the day;
//   - of either, the newest schedule that holds, every tier of it;
//   - a ledger record's: the latest dated on or before the day, else the
//     undated one. It replaces the credit rates a source states for that
//     account and currency; their debit rates stay.
func readYieldRates(ctx context.Context, db querier, in *yield.Inputs) error {
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
               AND d.account_external_id = s.account_external_id AND d.side = s.side
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
func readYieldCurves(ctx context.Context, db querier, in *yield.Inputs) error {
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
		c := &yield.Curve{Benchmark: t.points[i].Rate, BenchmarkMonths: t.points[i].Months}
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
// max_gap_days), its newest filed rate sensitivity in the report's sign
// (fund_risk_signed) with the net assets of that filing, what the lines
// of its composition at the instant say, and the share of its filing's
// lines that float, with the name the filing gives it.
func readYieldFunds(ctx context.Context, db querier, in *yield.Inputs) error {
	for _, h := range in.Holdings {
		if (h.Vehicle == "etf" || h.Vehicle == "fund") && h.SecurityID != "" {
			in.Funds[h.SecurityID] = &yield.Fund{}
		}
	}
	keys := slices.Sorted(maps.Keys(in.Funds))
	if len(keys) == 0 {
		return nil
	}
	list, args := sqlList(keys, "")
	fields := slices.Sorted(maps.Keys(fundFields))
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
      FROM fund_risk_signed r, exposure_settings_row() s
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
	if err := readFundFilings(ctx, db, in, list, args); err != nil {
		return err
	}
	return readFundLines(ctx, db, in, list, args)
}

// readFundFilings reads, for each fund of list held directly, the share
// of its filing's lines at the instant (fund_filings_at, migration 0141)
// that float, by weight, and the name the filing gives it.
func readFundFilings(ctx context.Context, db querier, in *yield.Inputs, list string, args func(...any) []any) error {
	return scanAll(ctx, db, `
SELECT f.fund_ref, COALESCE(f.fund_name, ''),
       100 * COALESCE(SUM(abs(l.weight)) FILTER (WHERE l.coupon_kind IN ('`+strings.Join(floatingLineKinds, "', '")+`')),
                      0)::DOUBLE / NULLIF(SUM(abs(l.weight)), 0)::DOUBLE
  FROM fund_filings_at(?) f
  JOIN fund_lines l ON l.silver_source_id = f.silver_source_id AND l.fund_ref = f.stored_ref AND l.as_of = f.as_of
       AND l.basis = f.basis
 WHERE list_contains(`+list+`, f.fund_ref)
 GROUP BY ALL`, func(rows *sql.Rows) error {
		var key, name string
		var floating sql.NullFloat64
		if err := rows.Scan(&key, &name, &floating); err != nil {
			return err
		}
		f := in.Funds[key]
		f.FilingFloatingPct, f.FilingName = nullFloatToPtr(floating), name
		return nil
	}, append([]any{in.AsOf}, args()...)...)
}

// sqlList is a DuckDB list of one `?` per value, each cast by cast (""
// for none, "::BIGINT"), and the arguments that fill it, more after
// them.
func sqlList[T any](vs []T, cast string) (string, func(more ...any) []any) {
	list := "[" + strings.Repeat("?"+cast+", ", len(vs)-1) + "?" + cast + "]"
	return list, func(more ...any) []any {
		out := make([]any, 0, len(vs)+len(more))
		for _, v := range vs {
			out = append(out, v)
		}
		return append(out, more...)
	}
}

// fundLine is one line of a fund's composition, as far as its kind, its
// yield and its duration go. class is the asset class its asset type
// maps to, "" where none does.
type fundLine struct {
	key                                        string
	weight, value, quantity                    float64
	ccy                                        string
	maturity                                   *time.Time
	coupon, rate, ytm, ytw, duration, modified *float64
	kind, class                                string
	defaulted, inKind                          bool
}

// readFundLines summarises each fund's lines at the instant
// (yield.LineSummary): the yield and the duration their prices imply,
// weighted by value. A line's own stated yield and duration win; else
// its price implies them, from its coupon, maturity and face (its
// balance), its value read in its own currency at the exchange rate it
// was valued at. A floating line, a line in default and one whose
// coupon is paid in kind imply nothing; their shares are counted. So
// are the shares of debt and of equity, by the class each line's asset
// type maps to, as exposure reads it. A line that states no coupon kind,
// default flag or arrears flag takes what the filing of the fund whose
// lines they are states of its security (fund_filing_kinds, migration
// 0141), as exposure_leaves reads its coupon kind.
func readFundLines(ctx context.Context, db querier, in *yield.Inputs, list string, args func(...any) []any) error {
	type comp struct {
		asOf  int64
		aged  bool
		lines []fundLine
	}
	comps := map[string]*comp{}
	if err := scanAll(ctx, db, `
WITH ln AS (
    SELECT fa.fund_ref, fa.as_of, fa.aged, fa.provider,
           CASE fa.via WHEN 'proxy' THEN fa.proxy_ref ELSE fa.fund_ref END AS own_ref,
           exposure_key(l.security_key) AS key, l.line_no, l.weight, l.market_value, l.quantity, l.currency,
           l.maturity, l.coupon, l.exchange_rate, l.yield_to_maturity, l.yield_to_worst, l.duration,
           l.modified_duration, l.coupon_kind, l.is_default, l.in_arrears, l.paid_in_kind, l.asset_type
      FROM exposure_funds_at(?) fa
      JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
           AND l.as_of = fa.as_of AND l.basis = fa.basis
     WHERE list_contains(`+list+`, fa.fund_ref))
SELECT ln.fund_ref, ln.as_of, ln.aged, COALESCE(ln.key, ''), COALESCE(ln.weight, 0)::DOUBLE,
       COALESCE(ln.market_value, 0)::DOUBLE,
       COALESCE(ln.quantity, 0)::DOUBLE, COALESCE(ln.currency, ''), ln.maturity, ln.coupon::DOUBLE,
       ln.exchange_rate::DOUBLE, ln.yield_to_maturity::DOUBLE, ln.yield_to_worst::DOUBLE, ln.duration::DOUBLE,
       ln.modified_duration::DOUBLE, COALESCE(ln.coupon_kind, k.coupon_kind, ''),
       COALESCE(COALESCE(ln.is_default, k.is_default) OR COALESCE(ln.in_arrears, k.in_arrears), FALSE),
       COALESCE(ln.paid_in_kind, FALSE), COALESCE(lt.value, '')
  FROM ln
  LEFT JOIN fund_line_labels lt ON lt.provider = ln.provider AND lt.field = 'asset_type' AND lt.raw = ln.asset_type
  LEFT JOIN fund_filing_kinds(?) k ON k.fund_ref = ln.own_ref AND k.key = ln.key
 ORDER BY ln.fund_ref, ln.line_no`, func(rows *sql.Rows) error {
		var key string
		var c comp
		var l fundLine
		var maturity sql.NullTime
		var coupon, rate, ytm, ytw, dur, mod sql.NullFloat64
		if err := rows.Scan(&key, &c.asOf, &c.aged, &l.key, &l.weight, &l.value, &l.quantity, &l.ccy, &maturity, &coupon,
			&rate, &ytm, &ytw, &dur, &mod, &l.kind, &l.defaulted, &l.inKind, &l.class); err != nil {
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
	}, append(append([]any{in.AsOf}, args()...), in.AsOf)...); err != nil {
		return err
	}
	// A security's line is the one of the most value held: its weight
	// times the value of the fund that holds it; of equal ones, the first
	// fund's, by its key, and the first line of it.
	held := map[string]float64{}
	for _, h := range in.Holdings {
		held[h.SecurityID] += math.Abs(h.Value)
	}
	in.Lines = map[string]yield.LineInfo{}
	most := map[string]float64{}
	for _, key := range slices.Sorted(maps.Keys(comps)) {
		c := comps[key]
		day := time.Unix(c.asOf, 0).UTC()
		infos := make([]yield.LineInfo, len(c.lines))
		for i, l := range c.lines {
			infos[i] = readLine(in, l, day)
			if l.key == "" {
				continue
			}
			v := math.Abs(l.weight) * held[key]
			if _, seen := in.Lines[l.key]; !seen || v > most[l.key] {
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
		Duration: coalesce(l.modified, l.duration), Default: l.defaulted}
	if !l.defaulted && !l.inKind {
		info.Yield = coalesce(l.ytw, l.ytm)
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
		floatingLine(l.kind) || l.defaulted || l.inKind {
		return info
	}
	b := fixedincome.Bond{Coupon: *l.coupon, Frequency: in.ConventionFrequency(l.ccy), Maturity: *l.maturity,
		DayCount: canonical.DayCountActAct}
	if v, err := b.Yield(day, price+b.Accrued(day)); err == nil {
		info.Yield = &v
		if info.Duration == nil {
			r := b.Risk(day, v).Modified
			info.Duration = &r
		}
	}
	return info
}

// floatingLineKinds are the coupon kinds of a fund's line whose rate
// floats, as N-PORT types them; exposure_yield_leaves reads the same
// ones as floating.
var floatingLineKinds = []string{"Floating", "Variable"}

// floatingLine says a line of coupon kind kind floats.
func floatingLine(kind string) bool { return slices.Contains(floatingLineKinds, kind) }

// summarizeLines is yield.LineSummary of a fund's lines on day, given
// what each says of its security (readLine). A line is debt where its
// asset type maps to fixed income, or maps to nothing and the line is a
// bond: it states a coupon or a yield, floats, or is in default or paid
// in kind. It is equity where its type maps to public equity or real
// estate, and a derivative where it maps to other or foreign exchange.
// The debt and equity shares are of the whole fund, as exposure reads a
// partial composition: what the lines leave out of 100% is neither.
func summarizeLines(lines []fundLine, infos []yield.LineInfo, day time.Time, aged bool) *yield.LineSummary {
	s := &yield.LineSummary{AsOf: day, Aged: aged}
	var total, wy, sy, wd, sd, wm, sm float64
	yearOut := day.AddDate(1, 0, 0)
	for i, l := range lines {
		w := math.Abs(l.weight)
		total += w
		debt := false
		switch l.class {
		case string(canonical.AssetClassFixedIncome):
			debt = true
		case string(canonical.AssetClassPublicEquity), string(canonical.AssetClassRealEstate):
			s.EquityPct += w
		case string(canonical.AssetClassOther), string(canonical.AssetClassForeignExchange):
			s.Derivatives = true
		case "":
			debt = l.coupon != nil || l.ytm != nil || l.ytw != nil || floatingLine(l.kind) || l.defaulted || l.inKind
		}
		if debt {
			s.DebtPct += w
			if l.maturity != nil && l.maturity.Before(yearOut) {
				s.ShortDebtPct += w
			}
		}
		switch {
		case floatingLine(l.kind):
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
	s.CoveredPct, s.DurationPct = sy*scale, sd*scale
	whole := 100 / max(total, 100)
	s.DebtPct, s.ShortDebtPct, s.EquityPct = s.DebtPct*whole, s.ShortDebtPct*whole, s.EquityPct*whole
	s.FloatingPct *= scale
	s.DefaultPct *= scale
	s.PaidInKindPct *= scale
	return s
}

// incomeTypeSQL maps an income line to the projection's types, by its
// detailed type and the instrument it names (i, and tm, its terms in
// force at the window's end): interest a bond pays is a coupon, but a
// bill's or a zero's discount is interest; a dividend a fund pays is a
// distribution.
var incomeTypeSQL = fmt.Sprintf(`CASE
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' AND i.vehicle = 'bond'
         AND NOT COALESCE(tm.coupon_kind = '%[7]s' OR tm.coupon_pct = 0, FALSE) THEN '%[1]s'
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' THEN '%[2]s'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' AND i.vehicle IN ('etf', 'fund') THEN '%[3]s'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' THEN '%[4]s'
    WHEN l.income_detailed = 'INCOME_DISTRIBUTIONS' THEN '%[3]s'
    WHEN l.income_detailed = 'INCOME_RENTAL' THEN '%[5]s'
    WHEN l.income_detailed = 'INCOME_RETIREMENT_PENSION' THEN '%[6]s' END`, yield.TypeCoupons, yield.TypeInterest,
	yield.TypeDistributions, yield.TypeDividends, yield.TypeRent, yield.TypePension, canonical.CouponZero)

// readIncome reads the income of the twelve months to the instant, in
// the output currency, per holding or sold position (lineHolding), and
// per account where a line names no instrument; and the payments of the
// 24 months to it the same way, in the line's currency. A dividend or a
// distribution comes with the quantities yield.Payment documents, read
// under its account's keys (yieldUnits.keysOf, quantity, span,
// firstHeld), its record day by the market of its instrument (lagOf).
func (b *yieldBase) readIncome(ctx context.Context, db querier, in *yield.Inputs) error {
	from := in.Day.AddDate(-2, 0, 0).Unix()
	var lines []incomeLine
	if err := incomeLines(ctx, db, from, in.AsOf, in.Currency, func(l incomeLine) {
		lines = append(lines, l)
	}); err != nil {
		return err
	}
	if b.units == nil {
		// A payment of the window's first days reads what was held up to
		// RecordLagDays before it, or across its record window.
		u, err := readYieldUnits(ctx, db, in, unitIDs(in, lines),
			from-max(yield.RecordLagDays, yield.RecordWindowDays)*86400, in.AsOf)
		if err != nil {
			return err
		}
		b.units = u
	}
	in.Index()
	yearAgo := in.Day.AddDate(-1, 0, 0)
	for _, l := range lines {
		key, held := lineHolding(in, &l)
		day := time.Unix(l.At, 0).UTC()
		recent := day.After(yearAgo)
		switch {
		case held:
			if recent && l.Value != nil {
				if in.Income[key] == nil {
					in.Income[key] = yield.Received{}
				}
				in.Income[key][l.Type] += *l.Value
			}
			p := yield.Payment{Day: day, Amount: l.Amount, Currency: l.Currency, Account: l.Account, Type: l.Type}
			if paysPerShare(l.Type) {
				start := day.Truncate(24 * time.Hour).Unix()
				keys := b.units.keysOf(in, l.Source, l.Account, l.Instrument)
				p.Quantity, p.Units = b.units.quantity(keys, start)
				if lag := b.lagOf(in, l); lag.Days == 0 {
					p.Before, _ = b.units.quantity(keys, start-yield.RecordLagDays*86400)
				} else {
					record := lag.RecordDay(time.Unix(start, 0).UTC()).Unix()
					p.LagDays = int((start - record) / 86400)
					far, near := lag.Window(time.Unix(start, 0).UTC())
					q, units := b.units.quantity(keys, record)
					if q != nil && b.units.seenBy(keys, far.Unix()) {
						p.Record, p.Units = q, units
						p.Least, p.Most = b.units.span(keys, far.Unix(), near.Unix())
					} else {
						// None held on the record day, none known, or the
						// account first seen inside the window: the pay day's
						// quantity rates, against the first the account shows
						// held after the record day (yield.Payment).
						p.Before = b.units.firstHeld(keys, record, start)
					}
				}
			}
			in.Payments[key] = append(in.Payments[key], p)
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
	}
	return nil
}

// unitIDs are the instruments the units of the dividends and
// distributions of lines read: each line's id folded under its account
// and under each account of its portfolio (portfolioKeys).
func unitIDs(in *yield.Inputs, lines []incomeLine) map[yield.InstrumentKey]bool {
	paid := map[yield.InstrumentKey]bool{}
	for _, l := range lines {
		if l.Instrument != "" && paysPerShare(l.Type) {
			for _, k := range portfolioKeys(in, l.Source, l.Account, l.Instrument) {
				paid[yield.InstrumentKey{Source: l.Source, Instrument: k.iid}] = true
			}
		}
	}
	return paid
}

// lagOf is the lag of the record day of the dividend a line pays
// (yieldBase.lag), under the first id its instrument reads under that has
// one (portfolioKeys).
func (b *yieldBase) lagOf(in *yield.Inputs, l incomeLine) yield.Lag {
	for _, k := range portfolioKeys(in, l.Source, l.Account, l.Instrument) {
		if lag := b.lag[yield.InstrumentKey{Source: l.Source, Instrument: k.iid}]; lag.Days > 0 {
			return lag
		}
	}
	return yield.Lag{}
}

// paysPerShare says a payment of type t is paid per share of what its
// holding holds: a dividend or a distribution.
func paysPerShare(t yield.IncomeType) bool {
	return t == yield.TypeDividends || t == yield.TypeDistributions
}

// incomeLine is one income transaction of a type the projection counts:
// its amount in its currency, its value in the output currency (nil
// where no rate converts it), and its description.
type incomeLine struct {
	Source, Account, Instrument, Currency, Description string
	At                                                 int64
	Type                                               yield.IncomeType
	Amount                                             float64
	Value                                              *float64
}

// couponOf reads an account's interest line that names no instrument but
// whose description names a bond (canonical.ReadCouponLine), as a broker
// books a Treasury's coupon: it is the coupon of the one bond of its
// account in the book that the line pays. Where no held bond, or more
// than one, pays it, a line that names a bond issue is the account's
// coupons (lineHolding); any other, such as a deposit's that states its
// rate and its end, stays interest.
func couponOf(in *yield.Inputs, l *incomeLine) {
	if l.Instrument != "" || l.Type != yield.TypeInterest {
		return
	}
	line, ok := canonical.ReadCouponLine(l.Description)
	if !ok {
		return
	}
	var bonds []yield.Holding
	for _, h := range in.Holdings {
		if h.Source == l.Source && h.Account == l.Account && h.Vehicle == string(canonical.VehicleBond) {
			bonds = append(bonds, h)
		}
	}
	paid, ok := line.OnePaying(len(bonds), func(i int) (string, *float64, *time.Time) {
		t := in.Terms[yield.InstrumentKey{Source: bonds[i].Source, Instrument: bonds[i].Instrument}]
		return bonds[i].Instrument, t.Coupon, t.Maturity
	})
	if ok || line.Issue {
		l.Type, l.Instrument = yield.TypeCoupons, paid
	}
}

// lineHolding is the holding a line belongs to (yield.Inputs.HoldingOf),
// else the sold position that holds it (yield.Inputs.Sold); false where
// neither does. Interest that names a bond is its coupon (couponOf). A
// sold position's coupons pool with the bond's amounts in its realized
// yield. A coupon whose line names no bond, or one that neither its
// account, its portfolio, nor one account of its source alone held,
// names no instrument: it reads as the account's coupons
// (`income:coupons:<CCY>`), received, but neither the cash's realized
// yield nor the projection reads it. So a coupon reads the same whether
// its line names its bond or only its description does.
func lineHolding(in *yield.Inputs, l *incomeLine) (yield.HoldingKey, bool) {
	couponOf(in, l)
	key, held := in.HoldingOf(l.Source, l.Account, l.Instrument)
	if !held {
		key, held = in.Sold(l.Source, l.Account, l.Instrument)
	}
	if !held && l.Type == yield.TypeCoupons {
		l.Instrument = ""
		return yield.HoldingKey{}, false
	}
	return key, held
}

// incomeLines calls fn with each income line of [from, to], oldest
// first: an income transaction of a type the projection counts, in ccy
// at its day's rate.
func incomeLines(ctx context.Context, db querier, from, to int64, ccy string, fn func(incomeLine)) error {
	return scanAll(ctx, db, `
WITH l AS (
    SELECT l.silver_source_id, l.account_external_id, t.instrument_external_id, l.transaction_external_id,
           l.occurred_at, l.currency, l.net_amount::DOUBLE AS amount, `+incomeTypeSQL+` AS type,
           COALESCE(t.description, '') AS description
      FROM income_lines_base(?, ?) l
      JOIN transactions t ON t.silver_source_id = l.silver_source_id
           AND t.transaction_external_id = l.transaction_external_id
      LEFT JOIN instruments i ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
      LEFT JOIN instrument_terms_at(?) tm ON tm.silver_source_id = t.silver_source_id
           AND tm.instrument_external_id = t.instrument_external_id),
fx AS (SELECT * FROM fx_rates_to(?))
SELECT l.silver_source_id, l.account_external_id, COALESCE(l.instrument_external_id, ''), l.occurred_at,
       l.currency, l.amount, l.type, l.description,
       CASE WHEN l.currency = ? THEN l.amount ELSE l.amount * fx.rate END AS value
  FROM l
  ASOF LEFT JOIN fx ON fx.from_ccy = l.currency AND fx.day <= l.occurred_at // 86400
 WHERE l.type IS NOT NULL
 ORDER BY l.occurred_at, l.silver_source_id, l.account_external_id, l.transaction_external_id`, func(rows *sql.Rows) error {
		var l incomeLine
		var typ string
		var value sql.NullFloat64
		if err := rows.Scan(&l.Source, &l.Account, &l.Instrument, &l.At, &l.Currency, &l.Amount, &typ, &l.Description,
			&value); err != nil {
			return err
		}
		l.Type, l.Value = yield.IncomeType(typ), nullFloatToPtr(value)
		fn(l)
		return nil
	}, from, to, to, ccy, ccy)
}

// IncomeReceived is every income line of [from, to] that the income
// projection counts as received, each named by the holding of the book
// it belongs to, else by the sold position that holds it (lineHolding),
// else by its account: a payment of interest by the account's cash in its
// currency, a line that names an instrument by its account and that
// instrument, by the id the matching folds it into, as Sold names a sold
// position, and any other by the account and its type.
func IncomeReceived(ctx context.Context, db querier, in *yield.Inputs, from, to int64) ([]yield.ReceivedLine, error) {
	var out []yield.ReceivedLine
	err := incomeLines(ctx, db, from, to, in.Currency, func(l incomeLine) {
		key, held := lineHolding(in, &l)
		r := yield.ReceivedLine{Day: time.Unix(l.At, 0).UTC(), Type: l.Type}
		if l.Value != nil {
			r.Value = *l.Value
		}
		switch {
		case held:
			r.Holding = key
		case l.Type == yield.TypeInterest && l.Instrument == "":
			r.Holding = yield.HoldingKey{Source: l.Source, Account: l.Account, PositionKey: "cash:" + l.Currency}
		default:
			r.Holding = yield.AccountIncomeKey(yield.AccountCurrency{
				AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account}, Currency: l.Currency}, l.Type)
			if l.Instrument != "" {
				r.Holding.PositionKey = in.Fold(l.Source, l.Account, l.Instrument)
			}
		}
		out = append(out, r)
	})
	if err != nil {
		return nil, fmt.Errorf("IncomeReceived: %w", err)
	}
	return out, nil
}

func readDeclaredDividends(ctx context.Context, db querier, in *yield.Inputs) error {
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

// YieldSecurities is exposure's securities at the book's instant and in
// its currency, each with what the book knows of it
// (yield.Book.Securities), the reported figures of every equity among
// them read for it. A value no row states is 0.
func YieldSecurities(ctx context.Context, db *sql.DB, b *yield.Book) ([]yield.SecurityRow, error) {
	sec, err := ExposureSecurities(ctx, db, b.In.AsOf, b.In.Currency, "risk")
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	rows := make([]yield.SecurityRow, len(sec))
	for i, s := range sec {
		r := yield.SecurityRow{Key: s.Security, Name: strText(s.Name), Ticker: strText(s.Ticker),
			AssetClass: strText(s.AssetClass), Sector: strText(s.Sector), Currency: strText(s.Ccy), Funds: s.Funds,
			SharePct: s.SharePct}
		r.Value, _ = parseFloatPtr(s.Value)
		r.Direct, _ = parseFloatPtr(s.DirectValue)
		r.ViaFunds, _ = parseFloatPtr(s.ViaFundsValue)
		if r.AssetClass == "public_equity" {
			keys[s.Security] = true
		}
		rows[i] = r
	}
	if err := YieldSecurityFacts(ctx, db, b.In, keys); err != nil {
		return nil, fmt.Errorf("YieldSecurities: %w", err)
	}
	return b.Securities(rows), nil
}

// periodFact is one reported figure for a period.
type periodFact struct {
	start, end time.Time
	value      float64
}

// readSecurities reads the reported figures of each stock held directly
// and gives it the trailing twelve months of each: the last four
// quarters, a quarter a year-to-date figure implies taken as the
// difference from the one before it, the fourth as the fiscal year less
// the nine months; a year earlier too, for growth. A stock an earlier
// currency's inputs read is not read again.
func (b *yieldBase) readSecurities(ctx context.Context, db querier, in *yield.Inputs) error {
	keys := map[string]bool{}
	for _, h := range in.Holdings {
		if h.Vehicle == "stock" && h.SecurityID != "" && !b.asked[h.SecurityID] {
			keys[h.SecurityID], b.asked[h.SecurityID] = true, true
		}
	}
	return YieldSecurityFacts(ctx, db, in, keys)
}

// YieldSecurityFacts gives each security of keys its trailing twelve
// months of each reported figure (yield.Facts), into in.Securities. A
// figure counts by its period's end: one a later filing restated reads
// restated, on any day.
func YieldSecurityFacts(ctx context.Context, db querier, in *yield.Inputs, keys map[string]bool) error {
	if len(keys) == 0 {
		return nil
	}
	facts := map[string]map[string][]periodFact{}
	units := map[string]string{}
	// The key column is named by its table: exposure_key's subquery
	// would read a bare security_key in a WHERE condition as
	// security_aliases' own.
	list, args := sqlList(slices.Sorted(maps.Keys(keys)), "")
	if err := scanAll(ctx, db, `
SELECT exposure_key(f.security_key) AS key, f.field, f.period_start, f.as_of, f.value::DOUBLE, COALESCE(f.unit, '')
  FROM security_period_facts f
 WHERE list_contains(`+list+`, exposure_key(f.security_key)) AND f.as_of <= ?
   AND f.field IN ('eps_diluted', 'dividends_per_share', 'operating_income', 'interest_expense')
 ORDER BY 1, 2, 4, 3`, func(rows *sql.Rows) error {
		var key, field, unit string
		var start time.Time
		var end int64
		var v float64
		if err := rows.Scan(&key, &field, &start, &end, &v, &unit); err != nil {
			return err
		}
		if facts[key] == nil {
			facts[key] = map[string][]periodFact{}
		}
		facts[key][field] = append(facts[key][field], periodFact{start: start.UTC(), end: time.Unix(end, 0).UTC(), value: v})
		if field == "eps_diluted" && unit != "" {
			units[key] = unit
		}
		return nil
	}, args(in.AsOf)...); err != nil {
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
	monthsBetween := func(a, b time.Time) int { return int(math.Round(b.Sub(a).Hours() / 24 / 30.44)) }
	months := func(p periodFact) int { return monthsBetween(p.start, p.end) }
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
			var prevEnd time.Time
			var prevValue float64
			q, hasQ := quarters[ps[i].start.AddDate(0, 3, -1)]
			switch {
			case i > 0:
				prevEnd, prevValue = ps[i-1].end, ps[i-1].value
			case hasQ && months(ps[i]) == 6:
				prevEnd, prevValue = ps[i].start.AddDate(0, 3, -1), q
			default:
				continue
			}
			if monthsBetween(prevEnd, ps[i].end) == 3 {
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
			if i > 0 && monthsBetween(ends[i], ends[i-1]) != 3 {
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

// readAmounts reads each position's amount on each day of the twelve
// months to the instant, one yield.Daily per currency it is valued in,
// and the instrument it names: a position as positions_at
// reads it on the day, a cash balance as cash_chosen does, a day it was
// not held as zero. Both read a day at its last second, the instant's own
// day at the instant, and take each account's active snapshot at it
// (migration 0140). So an observation's amounts hold over its span
// (pos_key_spans, cash_key_spans), from the first day read at or after it
// to the first day read at or after its end, and one read of the
// observations whose span meets the window gives every day.
func (b *yieldBase) readAmounts(ctx context.Context, db querier) error {
	w := newDayWindow(b.in)
	b.amounts, b.instrument = map[amountKey]*yield.Daily{}, map[yield.HoldingKey]string{}
	if err := w.readSpans(ctx, db, `
SELECT p.silver_source_id, p.snapshot_at, s.until, p.account_external_id, p.position_key,
       MAX(COALESCE(p.instrument_external_id, '')), MAX(COALESCE(p.currency, '')), SUM(p.market_value)::DOUBLE
  FROM positions p
  JOIN pos_key_spans() s ON s.src = p.silver_source_id AND s.acct = p.account_external_id
   AND s.snap = p.snapshot_at
 WHERE p.snapshot_at <= $1 AND (s.until IS NULL OR s.until > $2)
 GROUP BY ALL
 ORDER BY 1, 2`, b.amounts, b.instrument); err != nil {
		return err
	}
	return w.readSpans(ctx, db, `
SELECT cb.silver_source_id, cb.snapshot_at, s.until, cb.account_external_id, 'cash:' || cb.currency, '',
       cb.currency, cb.amount::DOUBLE
  FROM cash_balances cb
  JOIN cash_key_spans() s ON s.src = cb.silver_source_id AND s.acct = cb.account_external_id
   AND s.ccy = cb.currency AND s.snap = cb.snapshot_at
 WHERE cb.snapshot_at <= $1 AND (s.until IS NULL OR s.until > $2)
QUALIFY row_number() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                           ORDER BY `+cashKindRank+`, cb.balance_kind) = 1
 ORDER BY 1, 2`, b.amounts, b.instrument)
}

// cashKindRank orders a snapshot's balances of one account and currency
// as cash_chosen does: the current balance first, then the closing one,
// and so on.
const cashKindRank = `CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                   WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7
                   ELSE 99 END`

// dayWindow is the days of the twelve months to an instant, each read at
// its last second, the instant's own day at the instant.
type dayWindow struct {
	// first is the first day's last second.
	first, asOf int64
	days        int
}

func newDayWindow(in *yield.Inputs) dayWindow {
	start := in.Day.AddDate(-1, 0, 0).AddDate(0, 0, 1)
	return dayWindow{first: start.Unix() + 86399, asOf: in.AsOf, days: int(in.Day.Sub(start).Hours()/24) + 1}
}

// instant is the instant day i of the window is read at.
func (w dayWindow) instant(i int) int64 {
	if i == w.days-1 {
		return w.asOf
	}
	return w.first + int64(i)*86400
}

// from is the first day of the window an observation taken at at holds
// on: the first day read at or after it.
func (w dayWindow) from(at int64) int {
	return sort.Search(w.days, func(i int) bool { return w.instant(i) >= at })
}

// amountKey names a position's amounts in one currency.
type amountKey struct {
	yield.HoldingKey
	ccy string
}

// readSpans adds to out the amounts q reads: every holding's amount in
// each observation whose span meets the window, the span's end NULL while
// it holds on, ordered by source and observation, beside the instrument
// it names ("" for none), which instrument records, and its currency. q
// takes the instant and the first day's instant.
func (w dayWindow) readSpans(ctx context.Context, db querier, q string, out map[amountKey]*yield.Daily,
	instrument map[yield.HoldingKey]string) error {
	return scanAll(ctx, db, q, func(rows *sql.Rows) error {
		var k yield.HoldingKey
		var at int64
		var until sql.NullInt64
		var iid, ccy string
		var v sql.NullFloat64
		if err := rows.Scan(&k.Source, &at, &until, &k.Account, &k.PositionKey, &iid, &ccy, &v); err != nil {
			return err
		}
		if iid != "" {
			instrument[k] = iid
		}
		from, to := w.from(at), w.days
		if until.Valid {
			to = w.from(until.Int64)
		}
		if a := (amountKey{k, ccy}); from < to && v.Float64 != 0 {
			out[a] = addSpan(out[a], w.days, ccy, yield.Span{From: from, To: to, Amount: v.Float64})
		}
		return nil
	}, w.asOf, w.first)
}

// addSpan adds a span to a holding's daily amounts in ccy; one that goes
// on from the last at its amount extends it.
func addSpan(d *yield.Daily, days int, ccy string, s yield.Span) *yield.Daily {
	if d == nil {
		d = &yield.Daily{Days: days, Currency: ccy}
	}
	if n := len(d.Spans); n > 0 && d.Spans[n-1].To == s.From && d.Spans[n-1].Amount == s.Amount {
		d.Spans[n-1].To = s.To
		return d
	}
	d.Spans = append(d.Spans, s)
	return d
}

// daily gives each holding of in its daily amounts, in its own currency,
// and each instrument a holding pools (yield.Inputs.FormerDaily) the
// amounts of its positions that are no holding of in, one Daily per
// currency: an instrument held, by the id the matching folds it into, or
// one of a security held, at any source. A holding's amounts in another
// currency than its own, as a statement and a feed may value one
// position, convert at the instant's rates; where none converts them they
// count nothing.
func (b *yieldBase) daily(in *yield.Inputs) {
	own, held, securities := map[yield.HoldingKey]string{}, map[yield.InstrumentKey]bool{}, map[string]bool{}
	for _, h := range in.Holdings {
		own[h.HoldingKey] = h.Currency
		if h.Instrument != "" {
			held[yield.InstrumentKey{Source: h.Source, Instrument: in.Fold(h.Source, h.Account, h.Instrument)}] = true
		}
		if h.SecurityID != "" {
			securities[h.SecurityID] = true
		}
	}
	keys := slices.SortedFunc(maps.Keys(b.amounts), func(x, y amountKey) int {
		return cmp.Or(cmp.Compare(x.Source, y.Source), cmp.Compare(x.Account, y.Account),
			cmp.Compare(x.PositionKey, y.PositionKey), cmp.Compare(x.ccy, y.ccy))
	})
	for _, k := range keys {
		a := b.amounts[k]
		if ccy, ok := own[k.HoldingKey]; ok {
			rate := 1.0
			if k.ccy != ccy && k.ccy != "" && ccy != "" {
				if rate = in.FXOf(k.ccy) / in.FXOf(ccy); math.IsNaN(rate) || math.IsInf(rate, 0) || rate == 0 {
					continue
				}
			}
			d := in.Daily[k.HoldingKey]
			d.Days, d.Currency = a.Days, ccy
			for _, s := range a.Spans {
				d.Spans = append(d.Spans, yield.Span{From: s.From, To: s.To, Amount: s.Amount * rate})
			}
			in.Daily[k.HoldingKey] = d
			continue
		}
		iid := b.instrument[k.HoldingKey]
		if iid == "" {
			continue
		}
		ik := yield.InstrumentKey{Source: k.Source, Instrument: in.Fold(k.Source, k.Account, iid)}
		if !held[ik] && !securities[in.HeldSecurity[ik]] {
			continue
		}
		ds := in.FormerDaily[ik]
		i := slices.IndexFunc(ds, func(d yield.Daily) bool { return d.Currency == a.Currency })
		if i < 0 {
			i, ds = len(ds), append(ds, yield.Daily{Days: a.Days, Currency: a.Currency})
		}
		ds[i].Spans = append(ds[i].Spans, a.Spans...)
		in.FormerDaily[ik] = ds
	}
}

// readYieldAliases reads the ids the matching folds into others
// (yield.Inputs.Aliases), as holdingAliasJoins reads a holding's key: a
// holding alias for one account, else for every account of its source,
// else, for the CUSIP id of an instrument its source also keys by its
// ticker, the ticker (instrument_aliases, migration 0120).
func readYieldAliases(ctx context.Context, db querier, in *yield.Inputs) error {
	return scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, from_key, to_key FROM holding_aliases
UNION ALL
SELECT ia.src, '', ia.alias, ia.k
  FROM instrument_aliases() ia
  ANTI JOIN holding_aliases hd ON hd.silver_source_id = ia.src AND hd.account_external_id = ''
       AND hd.from_key = ia.alias`, func(rows *sql.Rows) error {
		var k yield.AliasKey
		var to string
		if err := rows.Scan(&k.Source, &k.Account, &k.ID, &to); err != nil {
			return err
		}
		in.Aliases[k] = to
		return nil
	})
}

// readYieldHeld reads, for each instrument a position held at some
// snapshot to the instant, by the id the matching folds it into
// (yield.Inputs.Fold): the accounts that held it (Held); its security, as
// exposure_holdings keys a holding (HeldSecurity); and into lags the lag
// of the record day of its dividends (yield.RecordLag). The lag reads
// each position's situs at the instant and its basis (exposure_situs_at),
// as the withholding table reads a holding's: the situs of a receipt of
// basis underlying is that of the share it stands for. Any position the
// situs rule reads as a receipt is one (receiptPositions): one whose
// situs is declared reads the declared country as its share's, one whose
// share is unknown has no lag. The currency is the one its instrument is
// listed in, where gold states one, else the one its newest snapshot
// values it in. Of the positions folded into one instrument, the one
// seen newest gives the lag; two seen at that instant that disagree give
// none.
func readYieldHeld(ctx context.Context, db querier, in *yield.Inputs, lags map[yield.InstrumentKey]yield.Lag,
	shared *yieldShared) error {
	type reading struct {
		k                      yield.InstrumentKey
		position               [3]string
		situs, basis, currency string
		share                  bool
		seen                   int64
	}
	var readings []reading
	maybe := false
	if err := scanAll(ctx, db, `
WITH p AS (
    SELECT silver_source_id AS src, account_external_id AS acct, position_key, instrument_external_id AS iid,
           COALESCE(arg_max(vehicle = 'stock' AND asset_class = 'public_equity', snapshot_at), FALSE) AS share,
           COALESCE(arg_max(currency, snapshot_at), '') AS currency, MAX(snapshot_at) AS seen
      FROM positions
     WHERE snapshot_at <= $1 AND instrument_external_id IS NOT NULL
     GROUP BY ALL),
k AS (
    SELECT p.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
      FROM p`+holdingAliasJoins("p.src", "p.acct", "p.iid", "p.position_key")+`)
SELECT k.src, k.acct, k.position_key, k.iid,
       COALESCE(exposure_key(COALESCE(ik1.security_key, ik2.security_key)), ''), COALESCE(st.situs, ''),
       COALESCE(st.basis, ''), k.share, COALESCE(NULLIF(i.currency, ''), k.currency), k.seen
  FROM k
  LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
  LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid
  LEFT JOIN instruments i ON i.silver_source_id = k.src AND i.instrument_external_id = k.iid
  LEFT JOIN exposure_situs_at($1) st ON st.silver_source_id = k.src AND st.account_external_id = k.acct
       AND st.position_key = k.position_key
 ORDER BY 1, 4, 2, 3`, func(rows *sql.Rows) error {
		var r reading
		var iid, security string
		if err := rows.Scan(&r.position[0], &r.position[1], &r.position[2], &iid, &security, &r.situs, &r.basis,
			&r.share, &r.currency, &r.seen); err != nil {
			return err
		}
		r.k = yield.InstrumentKey{Source: r.position[0], Instrument: in.Fold(r.position[0], r.position[1], iid)}
		in.Held[r.k] = append(in.Held[r.k], r.position[1])
		if security != "" && in.HeldSecurity[r.k] == "" {
			in.HeldSecurity[r.k] = security
		}
		readings, maybe = append(readings, r), maybe || r.basis == situsDeclared || r.basis == situsUnknown || r.basis == ""
		return nil
	}, in.AsOf); err != nil {
		return err
	}
	for k, accts := range in.Held {
		slices.Sort(accts)
		in.Held[k] = slices.Compact(accts)
	}
	if maybe && shared.receipts == nil {
		var err error
		if shared.receipts, err = receiptPositions(ctx, db); err != nil {
			return err
		}
	}
	receipts := shared.receipts
	type newest struct {
		seen  int64
		lag   yield.Lag
		mixed bool
	}
	picks := map[yield.InstrumentKey]*newest{}
	for _, r := range readings {
		receipt := r.basis == situsUnderlying || receipts[r.position]
		lag := yield.RecordLag(r.situs, r.currency, receipt, r.share)
		switch p := picks[r.k]; {
		case p == nil || r.seen > p.seen:
			picks[r.k] = &newest{seen: r.seen, lag: lag}
		case r.seen == p.seen && lag != p.lag:
			p.mixed = true
		}
	}
	for k, p := range picks {
		if !p.mixed && p.lag.Days > 0 {
			lags[k] = p.lag
		}
	}
	return nil
}

// receiptPositions are the positions the situs rule reads as depositary
// receipts (situsFacts.receipt), by source, account and position key. A
// declared situs comes first in the rule, and a receipt whose share is
// unknown has basis unknown: underlying says a position is one, declared
// and unknown say nothing, and any other basis says it is none.
func receiptPositions(ctx context.Context, db querier) (map[[3]string]bool, error) {
	out := map[[3]string]bool{}
	return out, scanAll(ctx, db, `
SELECT f.src, f.acct, f.position_key
  FROM (`+situsHoldingsSQL+`) f(src, acct, position_key, iid, coin, receipt, share, isin, domicile, banked, custody,
       private)
 WHERE f.receipt`, func(rows *sql.Rows) error {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			return err
		}
		out[k] = true
		return nil
	}, receiptWording.String())
}

func readWithholding(ctx context.Context, db querier, in *yield.Inputs) error {
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
func readYieldFX(ctx context.Context, db querier, in *yield.Inputs) error {
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
