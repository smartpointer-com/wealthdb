package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
		return newReport(buildYieldSecurityColumns(ccy), defaultYieldSecurityColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.SecurityRow, error) {
				return yieldSecurities(ctx, db, req.asOf, ccy)
			})
	case "funds":
		return newReport(buildYieldFundColumns(ccy), defaultYieldFundColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.FundRow, error) {
				b, err := yieldBook(ctx, db, req.asOf, ccy)
				if err != nil {
					return nil, err
				}
				return b.Funds(), nil
			})
	case "cash":
		return newReport(buildYieldCashColumns(ccy), defaultYieldCashColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.CashRow, error) {
				b, err := yieldBook(ctx, db, req.asOf, ccy)
				if err != nil {
					return nil, err
				}
				return b.Cash(), nil
			})
	case "curve":
		return exposureView(req, buildYieldCurveColumns(ccy), defaultYieldCurveColumns,
			func(ctx context.Context, db *sql.DB, at int64) ([]yield.CurveRow, error) {
				b, err := yieldBook(ctx, db, at, ccy)
				if err != nil {
					return nil, err
				}
				return b.Curve(), nil
			})
	default: // coverage
		return newReport(buildYieldCoverageColumns(ccy), defaultYieldCoverageColumns,
			func(ctx context.Context, db *sql.DB) ([]yield.CoverageRow, error) {
				b, err := yieldBook(ctx, db, req.asOf, ccy)
				if err != nil {
					return nil, err
				}
				return b.Coverage(), nil
			})
	}
}

// yieldSecurities is exposure's securities with the yield figures: the
// security master's rows, the book of the instant, and the reported
// figures of every equity it lists.
func yieldSecurities(ctx context.Context, db *sql.DB, asOf int64, ccy string) ([]yield.SecurityRow, error) {
	b, err := yieldBook(ctx, db, asOf, ccy)
	if err != nil {
		return nil, err
	}
	sec, err := gold.ExposureSecurities(ctx, db, asOf, ccy, "risk")
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	rows := make([]yield.SecurityRow, len(sec))
	for i, s := range sec {
		rows[i] = yield.SecurityRow{Key: s.Security, Name: strOrEmpty(s.Name), Ticker: strOrEmpty(s.Ticker),
			AssetClass: strOrEmpty(s.AssetClass), Sector: strOrEmpty(s.Sector), Currency: strOrEmpty(s.Ccy),
			Value: parseFloat(s.Value), Direct: parseFloat(s.DirectValue), ViaFunds: parseFloat(s.ViaFundsValue),
			Funds: s.Funds, SharePct: s.SharePct}
		if rows[i].AssetClass == "public_equity" {
			keys[s.Security] = true
		}
	}
	if err := gold.YieldSecurityFacts(ctx, db, b.In, keys); err != nil {
		return nil, err
	}
	return b.Securities(rows), nil
}

func parseFloat(s *string) float64 {
	if s == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(*s, 64)
	return v
}

func valueCol[T any](outCcy string, get func(T) float64) columnSpec[T] {
	return floatMoneyCol("value", outCcy, func(r T) *float64 { v := get(r); return &v })
}

func textOf[T any](name string, get func(T) string) columnSpec[T] {
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Extract: get}
}

func buildYieldSecurityColumns(outCcy string) []columnSpec[yield.SecurityRow] {
	type row = yield.SecurityRow
	return []columnSpec[row]{
		holdingKeyCol("security", func(r row) string { return r.Key }),
		textOf("name", func(r row) string { return r.Name }),
		textOf("ticker", func(r row) string { return r.Ticker }),
		textOf("asset_class", func(r row) string { return r.AssetClass }),
		textOf("sector", func(r row) string { return r.Sector }),
		textOf("currency", func(r row) string { return r.Currency }),
		valueCol(outCcy, func(r row) float64 { return r.Value }),
		floatMoneyCol("direct_value", outCcy, func(r row) *float64 { v := r.Direct; return &v }),
		floatMoneyCol("via_funds_value", outCcy, func(r row) *float64 { v := r.ViaFunds; return &v }),
		countCol("funds", func(r row) int64 { return r.Funds }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		pctCol("coupon", func(r row) *float64 { return r.Coupon }),
		textOf("maturity_date", func(r row) string { return dateOrBlank(r.MaturityDate) }),
		textOf("coupon_kind", func(r row) string { return r.CouponKind }),
		{Name: "price", Align: output.AlignRight, Privacy: PrivacyMoney, Extract: func(r row) string { return fixed4(r.Price) }},
		pctCol("implied_yield", func(r row) *float64 { return r.Implied }),
		yearsCol("duration", func(r row) *float64 { return r.Duration }),
		textOf("rating", func(r row) string { return r.Rating }),
		pctCol("earnings_yield", func(r row) *float64 { return r.EarningsYield }),
		yearsCol("pe", func(r row) *float64 { return r.PriceEarnings }),
		pctCol("dividend_yield", func(r row) *float64 { return r.DividendYield }),
		pctCol("payout", func(r row) *float64 { return r.Payout }),
		pctCol("dividend_growth", func(r row) *float64 { return r.DividendGrowth }),
		pctCol("earnings_growth", func(r row) *float64 { return r.EarningsGrowth }),
		yearsCol("coverage", func(r row) *float64 { return r.Coverage }),
		textOf("quality", func(r row) string { return r.Quality }),
	}
}

var defaultYieldSecurityColumns = []string{"security", "name", "asset_class", "value", "share", "coupon",
	"maturity_date", "implied_yield", "earnings_yield", "dividend_yield", "quality"}

func fixed4(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%.4f", *p)
}

func buildYieldFundColumns(outCcy string) []columnSpec[yield.FundRow] {
	type row = yield.FundRow
	return []columnSpec[row]{
		holdingKeyCol("fund", func(r row) string { return r.Key }),
		textOf("name", func(r row) string { return r.Name }),
		textOf("symbol", func(r row) string { return r.Symbol }),
		textOf("kind", func(r row) string { return string(r.Kind) }),
		textOf("currency", func(r row) string { return r.Currency }),
		valueCol(outCcy, func(r row) float64 { return r.Value }),
		pctCol("implied_yield", func(r row) *float64 { return r.Implied }),
		pctCol("lines_yield", func(r row) *float64 { return r.LinesYield }),
		yearsCol("lines_duration", func(r row) *float64 { return r.LinesDuration }),
		yearsCol("lines_maturity", func(r row) *float64 { return r.LinesMaturity }),
		textOf("lines_as_of", func(r row) string { return dateOrBlank(r.LinesAsOf) }),
		pctCol("lines_covered", func(r row) *float64 { return r.CoveredPct }),
		pctCol("floating", func(r row) *float64 { return r.FloatingPct }),
		pctCol("in_default", func(r row) *float64 { return r.DefaultPct }),
		pctCol("paid_in_kind", func(r row) *float64 { return r.PaidInKindPct }),
		pctCol("yield_to_maturity", func(r row) *float64 { return r.YieldToMaturity }),
		yearsCol("duration", func(r row) *float64 { return r.Duration }),
		yearsCol("maturity", func(r row) *float64 { return r.Maturity }),
		pctCol("distribution_yield", func(r row) *float64 { return r.DistributionYield }),
		pctCol("sec_yield", func(r row) *float64 { return r.SECYield }),
		pctCol("seven_day_yield", func(r row) *float64 { return r.SevenDayYield }),
		pctCol("trailing_yield", func(r row) *float64 { return r.TrailingYield }),
		floatMoneyCol("dv01", outCcy, func(r row) *float64 { return r.DV01 }),
		textOf("risk_as_of", func(r row) string { return dateOrBlank(r.RiskAsOf) }),
		textOf("as_of", func(r row) string { return dateOrBlank(r.FiguresAsOf) }),
		textOf("source", func(r row) string { return r.FiguresFrom }),
		textOf("quality", func(r row) string { return r.Quality }),
	}
}

var defaultYieldFundColumns = []string{"fund", "name", "kind", "currency", "value", "implied_yield", "duration",
	"distribution_yield", "sec_yield", "as_of", "source", "quality"}

func buildYieldCashColumns(outCcy string) []columnSpec[yield.CashRow] {
	type row = yield.CashRow
	return []columnSpec[row]{
		textOf("silver_source", func(r row) string { return r.Source }),
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(r row) string {
			name := r.Account.Name
			return accountLabel(&name, r.HoldingKey.Account)
		}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.HoldingKey.Account }},
		textOf("tax_wrapper", func(r row) string { return r.Account.TaxWrapper }),
		textOf("kind", func(r row) string { return string(r.Kind) }),
		textOf("symbol", func(r row) string { return r.Symbol }),
		textOf("currency", func(r row) string { return r.Currency }),
		valueCol(outCcy, func(r row) float64 { return r.Value }),
		pctCol("rate", func(r row) *float64 { return r.Stated }),
		countCol("tiers", func(r row) int64 { return int64(r.Tiers) }),
		{Name: "tier_up_to", Align: output.AlignRight, Privacy: PrivacyMoney, Extract: func(r row) string {
			if r.UpTo == nil {
				return ""
			}
			return fmt.Sprintf("%.2f", *r.UpTo)
		}},
		textOf("valid_from", func(r row) string { return dateOrBlank(r.ValidFrom) }),
		textOf("term_end", func(r row) string { return dateOrBlank(r.TermEnd) }),
		pctCol("realized_yield", func(r row) *float64 { return r.Realized }),
		pctCol("benchmark", func(r row) *float64 { return r.Benchmark }),
		pctCol("gap", func(r row) *float64 { return r.Gap }),
		floatMoneyCol("income_12m", outCcy, func(r row) *float64 { return r.Income12 }),
		floatMoneyCol("income_100bp", outCcy, func(r row) *float64 { return r.Income100bp }),
		textOf("quality", func(r row) string { return positionQuality(r.Position) }),
	}
}

var defaultYieldCashColumns = []string{"silver_source", "account", "kind", "currency", "value", "rate", "valid_from",
	"term_end", "realized_yield", "benchmark", "gap", "quality"}

func buildYieldCurveColumns(outCcy string) []columnSpec[yield.CurveRow] {
	type row = yield.CurveRow
	money := func(name string, get func(row) float64) columnSpec[row] {
		return floatMoneyCol(name, outCcy, func(r row) *float64 { v := get(r); return &v })
	}
	return []columnSpec[row]{
		textOf("currency", func(r row) string { return r.Currency }),
		textOf("tenor", func(r row) string { return r.Tenor }),
		money("dv01", func(r row) float64 { return r.DV01 }),
		money("dv100", func(r row) float64 { return r.DV100 }),
		money("funds", func(r row) float64 { return r.Funds }),
		money("bonds", func(r row) float64 { return r.Bonds }),
		money("cash", func(r row) float64 { return r.Cash }),
		money("liabilities", func(r row) float64 { return r.Liabilities }),
	}
}

var defaultYieldCurveColumns = []string{"currency", "tenor", "dv01", "dv100", "funds", "bonds", "cash", "liabilities"}

func buildYieldCoverageColumns(outCcy string) []columnSpec[yield.CoverageRow] {
	type row = yield.CoverageRow
	return []columnSpec[row]{
		textOf("section", func(r row) string { return r.Section }),
		textOf("reason", func(r row) string { return r.Reason }),
		valueCol(outCcy, func(r row) float64 { return r.Value }),
		pctCol("share", func(r row) *float64 { return r.SharePct }),
		countCol("holdings", func(r row) int64 { return int64(r.Holdings) }),
		textOf("silver_source", func(r row) string { return r.Source }),
		{Name: "instrument", Align: output.AlignLeft, Privacy: PrivacyFreeText,
			Extract: func(r row) string { return r.Instrument }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r row) string { return r.Account }},
		textOf("name", func(r row) string { return r.Name }),
	}
}

var defaultYieldCoverageColumns = []string{"section", "reason", "value", "share", "holdings", "silver_source",
	"instrument", "name"}

// writeYieldSkeletons writes the ledger record that fills each holding
// coverage lists: its terms, its account's rate, its rating, its
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
	seen := map[string]bool{}
	for _, r := range b.Coverage() {
		if r.Skeleton == nil {
			continue
		}
		line, err := json.Marshal(r.Skeleton)
		if err != nil {
			return err
		}
		if seen[string(line)] {
			continue
		}
		seen[string(line)] = true
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return err
		}
	}
	return nil
}
