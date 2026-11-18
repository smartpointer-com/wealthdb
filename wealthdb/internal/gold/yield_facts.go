package gold

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

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
	if err := yieldSecurityFacts(ctx, db, b.In, keys); err != nil {
		return nil, fmt.Errorf("YieldSecurities: %w", err)
	}
	return b.Securities(rows), nil
}

// readSecurities reads the reported figures of each stock some currency
// of the base holds directly and gives it the trailing twelve months of
// each: the last four quarters, a quarter a year-to-date figure implies
// taken as the difference from the one before it, the fourth as the
// fiscal year less the nine months; a year earlier too, for growth.
func (b *yieldBase) readSecurities(ctx context.Context, db querier) error {
	keys := map[string]bool{}
	for _, v := range b.valued {
		for _, h := range v.holdings {
			if h.Vehicle == "stock" && h.SecurityID != "" {
				keys[h.SecurityID] = true
			}
		}
	}
	return yieldSecurityFacts(ctx, db, b.in, keys)
}

// yieldSecurityFacts gives each security of keys its trailing twelve
// months of each reported figure (yield.Facts), into in.Securities. A
// figure counts by its period's end: one a later filing restated reads
// restated, on any day.
func yieldSecurityFacts(ctx context.Context, db querier, in *yield.Inputs, keys map[string]bool) error {
	if len(keys) == 0 {
		return nil
	}
	facts := map[string]map[string][]yield.Period{}
	units := map[string]string{}
	// The key column is named by its table: exposure_key's subquery
	// would read a bare security_key in a WHERE condition as
	// security_aliases' own.
	list, args := sqlList(slices.Sorted(maps.Keys(keys)))
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
			facts[key] = map[string][]yield.Period{}
		}
		facts[key][field] = append(facts[key][field], yield.Period{Start: start.UTC(), End: time.Unix(end, 0).UTC(), Value: v})
		if field == "eps_diluted" && unit != "" {
			units[key] = unit
		}
		return nil
	}, args(in.AsOf)...); err != nil {
		return err
	}
	for key, byField := range facts {
		var f yield.Facts
		ttm := func(field string, years int) *float64 {
			if v, ok := yield.Trailing(byField[field], in.Day, years); ok {
				return &v
			}
			return nil
		}
		f.Currency, _, _ = strings.Cut(units[key], "/")
		if f.EPS = ttm("eps_diluted", 0); f.EPS != nil {
			f.EPSYearAgo = ttm("eps_diluted", 1)
		}
		if f.DPS = ttm("dividends_per_share", 0); f.DPS != nil {
			f.DPSYearAgo = ttm("dividends_per_share", 1)
		}
		f.OperatingIncome, f.InterestExpense = ttm("operating_income", 0), ttm("interest_expense", 0)
		in.Securities[key] = f
	}
	return nil
}
