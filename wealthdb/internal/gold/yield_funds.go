package gold

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// fundRead is what gold says of one fund at an instant: its figures, and
// its composition's lines with what each says of its security
// (yield.ReadLine). None of it depends on the output currency.
type fundRead struct {
	fund  *yield.Fund
	lines yield.FundLines
}

// heldFunds are the keys of the funds the inputs hold directly, sorted.
func heldFunds(in *yield.Inputs) []string {
	keys := map[string]bool{}
	for _, h := range in.Holdings {
		if (h.Vehicle == "etf" || h.Vehicle == "fund") && h.SecurityID != "" {
			keys[h.SecurityID] = true
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// readFunds reads the funds some currency of the base holds directly,
// once (readYieldFunds): every currency's inputs share their figures,
// which yield.Compute does not change.
func (b *yieldBase) readFunds(ctx context.Context, db querier) error {
	held := map[string]bool{}
	for _, v := range b.valued {
		for _, k := range heldFunds(&yield.Inputs{Holdings: v.holdings}) {
			held[k] = true
		}
	}
	if len(held) == 0 {
		return nil
	}
	read, err := readYieldFunds(ctx, db, b.in, slices.Sorted(maps.Keys(held)))
	if err != nil {
		return err
	}
	b.funds = read
	return nil
}

// pickFunds gives in the funds it holds directly (readFunds) and the
// lines of its own holdings (yield.PickLines).
func (b *yieldBase) pickFunds(in *yield.Inputs) {
	keys := heldFunds(in)
	if len(keys) == 0 {
		return
	}
	for _, k := range keys {
		in.Funds[k] = b.funds[k].fund
	}
	lines := make(map[string]*yield.FundLines, len(keys))
	for _, k := range keys {
		lines[k] = &b.funds[k].lines
	}
	yield.PickLines(in, lines, keys)
}

// readYieldFunds reads, for each fund of keys, its newest figure of each
// field on or before the instant (within the exposure's max_gap_days),
// its newest filed rate sensitivity in the report's sign
// (fund_risk_signed) with the net assets of that filing, what the lines
// of its composition at the instant say, and the share of its filing's
// lines that float, with the name the filing gives it.
func readYieldFunds(ctx context.Context, db querier, in *yield.Inputs, keys []string) (map[string]*fundRead, error) {
	out, err := readFundFigures(ctx, db, in, keys)
	if err != nil {
		return nil, err
	}
	list, args := sqlList(keys, "")
	if err := readFundFilings(ctx, db, in, out, list, args); err != nil {
		return nil, err
	}
	if err := readFundLines(ctx, db, in, out, list, args); err != nil {
		return nil, err
	}
	return out, nil
}

// readFundFigures is readYieldFunds' figures and rate sensitivities
// alone: neither the filing's lines nor the composition's.
func readFundFigures(ctx context.Context, db querier, in *yield.Inputs, keys []string) (map[string]*fundRead, error) {
	out := make(map[string]*fundRead, len(keys))
	for _, k := range keys {
		out[k] = &fundRead{fund: &yield.Fund{}}
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
		if p, day := fundFields[field](out[key].fund), time.Unix(asOf, 0).UTC(); *p == nil || (*p).Day.Before(day) {
			*p = &yield.Dated{Value: v, Day: day}
		}
		return nil
	}, args(in.AsOf, in.AsOf)...); err != nil {
		return nil, err
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
		f := out[key].fund
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
		return nil, err
	}
	return out, nil
}

// readFundFilings reads, for each fund of list, the share of its filing's
// lines at the instant (fund_filings_at) that float, by weight, a line's
// kind read as the coupon-kind label map says (couponKindsSQL), and the
// name the filing gives it.
func readFundFilings(ctx context.Context, db querier, in *yield.Inputs, out map[string]*fundRead, list string,
	args func(...any) []any) error {
	return scanAll(ctx, db, `
SELECT f.fund_ref, COALESCE(f.fund_name, ''),
       100 * COALESCE(SUM(abs(l.weight)) FILTER (WHERE ck.value = 'floating'),
                      0)::DOUBLE / NULLIF(SUM(abs(l.weight)), 0)::DOUBLE
  FROM fund_filings_at(?) f
  JOIN fund_lines l ON l.silver_source_id = f.silver_source_id AND l.fund_ref = f.stored_ref AND l.as_of = f.as_of
       AND l.basis = f.basis
  LEFT JOIN (`+couponKindsSQL+`) ck ON ck.label = exposure_norm(l.coupon_kind)
 WHERE list_contains(`+list+`, f.fund_ref)
 GROUP BY ALL`, func(rows *sql.Rows) error {
		var key, name string
		var floating sql.NullFloat64
		if err := rows.Scan(&key, &name, &floating); err != nil {
			return err
		}
		f := out[key].fund
		f.FilingFloatingPct, f.FilingName = nullFloatToPtr(floating), name
		return nil
	}, append([]any{in.AsOf}, args()...)...)
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
// lines they are states of its security (fund_filing_kinds),
// as exposure_leaves reads its coupon kind. It keeps each fund's
// lines and what each says of its security (yield.ReadLine) in out, for
// every currency's yield.PickLines.
func readFundLines(ctx context.Context, db querier, in *yield.Inputs, out map[string]*fundRead, list string,
	args func(...any) []any) error {
	type comp struct {
		asOf  int64
		aged  bool
		lines []yield.FundLine
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
       COALESCE(ln.paid_in_kind, FALSE), COALESCE(lt.value, ''), COALESCE(ck.value = 'floating', FALSE)
  FROM ln
  LEFT JOIN fund_line_labels lt ON lt.provider = ln.provider AND lt.field = 'asset_type' AND lt.raw = ln.asset_type
  LEFT JOIN fund_filing_kinds(?) k ON k.fund_ref = ln.own_ref AND k.key = ln.key
  LEFT JOIN (`+couponKindsSQL+`) ck ON ck.label = exposure_norm(COALESCE(ln.coupon_kind, k.coupon_kind))
 ORDER BY ln.fund_ref, ln.line_no`, func(rows *sql.Rows) error {
		var key string
		var c comp
		var l yield.FundLine
		var maturity sql.NullTime
		var coupon, rate, ytm, ytw, dur, mod sql.NullFloat64
		if err := rows.Scan(&key, &c.asOf, &c.aged, &l.Key, &l.Weight, &l.Value, &l.Quantity, &l.Currency, &maturity, &coupon,
			&rate, &ytm, &ytw, &dur, &mod, &l.Kind, &l.Defaulted, &l.InKind, &l.Class, &l.Floats); err != nil {
			return err
		}
		l.Maturity, l.Coupon, l.Rate = nullDate(maturity), nullFloatToPtr(coupon), nullFloatToPtr(rate)
		l.YTM, l.YTW, l.Duration, l.Modified = nullFloatToPtr(ytm), nullFloatToPtr(ytw), nullFloatToPtr(dur),
			nullFloatToPtr(mod)
		if comps[key] == nil {
			comps[key] = &comp{asOf: c.asOf, aged: c.aged}
		}
		comps[key].lines = append(comps[key].lines, l)
		return nil
	}, append(append([]any{in.AsOf}, args()...), in.AsOf)...); err != nil {
		return err
	}
	for key, c := range comps {
		day := time.Unix(c.asOf, 0).UTC()
		infos := make([]yield.LineInfo, len(c.lines))
		for i, l := range c.lines {
			infos[i] = yield.ReadLine(in, l, day)
		}
		f := out[key]
		f.lines = yield.FundLines{Lines: c.lines, Infos: infos}
		f.fund.Lines = yield.SummarizeLines(c.lines, infos, day, c.aged)
	}
	return nil
}

// couponKindsSQL is the coupon-kind label map (canonical.LabelCouponKind):
// a fund line's word for its coupon's kind, normalized, and the kind it
// reads as. A line floats where its word reads as floating, as
// exposure_yield_leaves reads it.
const couponKindsSQL = `SELECT label, value FROM exposure_labels WHERE provider = '' AND field = 'coupon_kind'`
