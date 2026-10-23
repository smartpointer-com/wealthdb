package main

import (
	"context"
	"database/sql"
	"io"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// yieldDetailReport is one of the list views: securities, funds, cash,
// curve and coverage.
func yieldDetailReport(req request) *report {
	ccy := req.currency
	switch req.view {
	case "securities":
		kindOf, loadKinds := sourceKinds()
		return withLoads(bookView(req, buildYieldSecurityColumns(ccy, kindOf), defaultYieldSecurityColumns,
			gold.YieldSecurities), loadKinds)
	case "funds":
		kindOf, loadKinds := sourceKinds()
		return withLoads(bookView(req, buildYieldFundColumns(ccy, kindOf), defaultYieldFundColumns,
			func(_ context.Context, _ *sql.DB, b *yield.Book) ([]yield.FundRow, error) {
				return b.Funds(), nil
			}), loadKinds)
	case "cash":
		kindOf, loadKinds := sourceKinds()
		return withLoads(bookView(req, buildYieldCashColumns(ccy, kindOf), defaultYieldCashColumns,
			func(_ context.Context, _ *sql.DB, b *yield.Book) ([]yield.CashRow, error) {
				return b.Cash(), nil
			}), loadKinds)
	case "curve":
		return seriesView(req, buildYieldCurveColumns(ccy), defaultYieldCurveColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]yield.CurveRow, error) {
				b, err := yieldBook(ctx, db, at, ccy)
				if err != nil {
					return nil, err
				}
				return b.Curve(), nil
			})
	default: // coverage
		kindOf, loadKinds := sourceKinds()
		return withLoads(bookView(req, buildYieldCoverageColumns(ccy, kindOf), defaultYieldCoverageColumns,
			func(_ context.Context, _ *sql.DB, b *yield.Book) ([]yield.CoverageRow, error) {
				return b.Coverage(), nil
			}), loadKinds)
	}
}

// bookView is a view whose rows the book of req.asOf in req.currency
// gives.
func bookView[T any](req request, registry []columnSpec[T], defaults []string,
	rows func(context.Context, *sql.DB, *yield.Book) ([]T, error)) *report {
	return newReport(registry, defaults, func(ctx context.Context, db *sql.DB) ([]T, error) {
		b, err := yieldBook(ctx, db, req.asOf, req.currency)
		if err != nil {
			return nil, err
		}
		return rows(ctx, db, b)
	})
}

func buildYieldSecurityColumns(outCcy string, kindOf func(string) string) []columnSpec[yield.SecurityRow] {
	type row = yield.SecurityRow
	keyCol, nameCol := holdingKeyCols("security", kindOf, func(r row) string { return r.Key },
		func(r row) string { return r.Name })
	return []columnSpec[row]{
		keyCol,
		nameCol,
		strCol("ticker", func(r row) string { return r.Ticker }),
		strCol("asset_class", func(r row) string { return r.AssetClass }),
		strCol("sector", func(r row) string { return r.Sector }),
		strCol("currency", func(r row) string { return r.Currency }),
		floatValCol("value", outCcy, func(r row) float64 { return r.Value }),
		floatValCol("direct_value", outCcy, func(r row) float64 { return r.Direct }),
		floatValCol("via_funds_value", outCcy, func(r row) float64 { return r.ViaFunds }),
		countCol("funds", func(r row) int64 { return r.Funds }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		pctCol("coupon", func(r row) *float64 { return r.Coupon }),
		strCol("maturity_date", func(r row) string { return dateOrBlank(r.MaturityDate) }),
		strCol("coupon_kind", func(r row) string { return r.CouponKind }),
		priceCol(func(r row) *float64 { return r.Price }),
		pctCol("implied_yield", func(r row) *float64 { return r.Implied }),
		decimalCol("duration", func(r row) *float64 { return r.Duration }),
		strCol("rating", func(r row) string { return r.Rating }),
		pctCol("earnings_yield", func(r row) *float64 { return r.EarningsYield }),
		decimalCol("pe", func(r row) *float64 { return r.PriceEarnings }),
		pctCol("dividend_yield", func(r row) *float64 { return r.DividendYield }),
		pctCol("payout", func(r row) *float64 { return r.Payout }),
		pctCol("dividend_growth", func(r row) *float64 { return r.DividendGrowth }),
		pctCol("earnings_growth", func(r row) *float64 { return r.EarningsGrowth }),
		decimalCol("coverage", func(r row) *float64 { return r.Coverage }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultYieldSecurityColumns = []string{"security", "name", "asset_class", "value", "share", "coupon",
	"maturity_date", "implied_yield", "earnings_yield", "dividend_yield", "quality"}

func buildYieldFundColumns(outCcy string, kindOf func(string) string) []columnSpec[yield.FundRow] {
	type row = yield.FundRow
	keyCol, nameCol := holdingKeyCols("fund", kindOf, func(r row) string { return r.Key },
		func(r row) string { return r.Name })
	return []columnSpec[row]{
		keyCol,
		nameCol,
		strCol("symbol", func(r row) string { return r.Symbol }),
		strCol("kind", func(r row) string { return string(r.Kind) }),
		strCol("currency", func(r row) string { return r.Currency }),
		floatValCol("value", outCcy, func(r row) float64 { return r.Value }),
		pctCol("implied_yield", func(r row) *float64 { return r.Implied }),
		pctCol("lines_yield", func(r row) *float64 { return r.LinesYield }),
		decimalCol("lines_duration", func(r row) *float64 { return r.LinesDuration }),
		decimalCol("lines_maturity", func(r row) *float64 { return r.LinesMaturity }),
		strCol("lines_as_of", func(r row) string { return dateOrBlank(r.LinesAsOf) }),
		pctCol("lines_covered", func(r row) *float64 { return r.CoveredPct }),
		pctCol("floating", func(r row) *float64 { return r.FloatingPct }),
		pctCol("in_default", func(r row) *float64 { return r.DefaultPct }),
		pctCol("paid_in_kind", func(r row) *float64 { return r.PaidInKindPct }),
		pctCol("yield_to_maturity", func(r row) *float64 { return r.YieldToMaturity }),
		decimalCol("duration", func(r row) *float64 { return r.Duration }),
		decimalCol("maturity", func(r row) *float64 { return r.Maturity }),
		pctCol("distribution_yield", func(r row) *float64 { return r.DistributionYield }),
		pctCol("sec_yield", func(r row) *float64 { return r.SECYield }),
		pctCol("seven_day_yield", func(r row) *float64 { return r.SevenDayYield }),
		pctCol("trailing_yield", func(r row) *float64 { return r.TrailingYield }),
		floatMoneyCol("dv01", outCcy, func(r row) *float64 { return r.DV01 }),
		strCol("risk_as_of", func(r row) string { return dateOrBlank(r.RiskAsOf) }),
		strCol("as_of", func(r row) string { return dateOrBlank(r.FiguresAsOf) }),
		strCol("source", func(r row) string { return r.FiguresFrom }),
		qualityCol(func(r row) string { return r.Quality }),
	}
}

var defaultYieldFundColumns = []string{"fund", "name", "kind", "currency", "value", "implied_yield", "duration",
	"dv01", "distribution_yield", "sec_yield", "as_of", "source", "quality"}

func buildYieldCashColumns(outCcy string, kindOf func(string) string) []columnSpec[yield.CashRow] {
	type row = yield.CashRow
	ref := func(r row) accountRef {
		return accountRef{source: r.Source, name: &r.Account.Name,
			nickname: &r.Account.Nickname, id: r.HoldingKey.Account}
	}
	return []columnSpec[row]{
		strCol("silver_source", func(r row) string { return r.Source }),
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		strCol("tax_wrapper", func(r row) string { return r.Account.TaxWrapper }),
		strCol("kind", func(r row) string { return string(r.Kind) }),
		strCol("symbol", func(r row) string { return r.Symbol }),
		strCol("currency", func(r row) string { return r.Currency }),
		floatValCol("value", outCcy, func(r row) float64 { return r.Value }),
		pctCol("rate", func(r row) *float64 { return r.Stated }),
		countCol("tiers", func(r row) int64 { return int64(r.Tiers) }),
		{Name: "tier_up_to", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r row) string { return fixedOrBlank(r.UpTo, 2) }},
		strCol("valid_from", func(r row) string { return dateOrBlank(r.ValidFrom) }),
		strCol("term_end", func(r row) string { return dateOrBlank(r.TermEnd) }),
		pctCol("realized_yield", func(r row) *float64 { return r.Realized }),
		pctCol("benchmark", func(r row) *float64 { return r.Benchmark }),
		pctCol("gap", func(r row) *float64 { return r.Gap }),
		floatMoneyCol("income_12m", outCcy, func(r row) *float64 { return r.Income12 }),
		floatMoneyCol("income_100bp", outCcy, func(r row) *float64 { return r.Income100bp }),
		qualityCol(func(r row) string { return positionQuality(r.Position) }),
	}
}

var defaultYieldCashColumns = []string{"silver_source", "account", "kind", "currency", "value", "rate", "valid_from",
	"term_end", "realized_yield", "benchmark", "gap", "quality"}

func buildYieldCurveColumns(outCcy string) []columnSpec[yield.CurveRow] {
	type row = yield.CurveRow
	return []columnSpec[row]{
		strCol("currency", func(r row) string { return r.Currency }),
		strCol("tenor", func(r row) string { return r.Tenor }),
		floatValCol("dv01", outCcy, func(r row) float64 { return r.DV01 }),
		floatValCol("dv100", outCcy, func(r row) float64 { return r.DV100 }),
		floatValCol("funds", outCcy, func(r row) float64 { return r.Funds }),
		floatValCol("bonds", outCcy, func(r row) float64 { return r.Bonds }),
		floatValCol("cash", outCcy, func(r row) float64 { return r.Cash }),
		floatValCol("liabilities", outCcy, func(r row) float64 { return r.Liabilities }),
	}
}

var defaultYieldCurveColumns = []string{"currency", "tenor", "dv01", "dv100", "funds", "bonds", "cash", "liabilities"}

func buildYieldCoverageColumns(outCcy string, kindOf func(string) string) []columnSpec[yield.CoverageRow] {
	type row = yield.CoverageRow
	holding := func(r row) (string, string) { return r.Source, r.Instrument }
	return []columnSpec[row]{
		strCol("section", func(r row) string { return r.Section }),
		strCol("reason", func(r row) string { return r.Reason }),
		floatValCol("value", outCcy, func(r row) float64 { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		countCol("holdings", func(r row) int64 { return int64(r.Holdings) }),
		// Only a record_day line counts payments: blank on the others.
		{Name: "payments", Align: output.AlignRight, Extract: func(r row) string {
			if r.Payments == 0 {
				return ""
			}
			return strconv.Itoa(r.Payments)
		}},
		strCol("silver_source", func(r row) string { return r.Source }),
		typedHoldingCol(columnSpec[row]{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r row) string { return r.Instrument }}, kindOf, holding),
		// A holding's line names its account by id alone.
		accountIDCol("account", kindOf, func(r row) accountRef { return accountRef{source: r.Source, id: r.Account} }),
		typedHoldingCol(strCol("name", func(r row) string { return r.Name }), kindOf, holding),
	}
}

var defaultYieldCoverageColumns = []string{"section", "reason", "value", "share", "holdings", "payments",
	"silver_source", "instrument", "name"}

// writeYieldSkeletons writes the ledger record that fills each holding
// coverage lists, once: its terms, its account's rate, its rating, its
// dividend, a fund's factsheet figures. Each loads once filled in.
func writeYieldSkeletons(ctx context.Context, open func() (*sql.DB, error), asOf int64, ccy string, w io.Writer) error {
	db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()
	b, err := yieldBook(ctx, db, asOf, ccy)
	if err != nil {
		return err
	}
	out := newNDJSONWriter(w)
	for _, r := range b.Coverage() {
		if r.Skeleton == nil {
			continue
		}
		if err := out.write(r.Skeleton); err != nil {
			return err
		}
	}
	return nil
}
