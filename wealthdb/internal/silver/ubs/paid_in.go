package ubs

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The basis of a private-markets fund's units: the capital its calls
// took (docs/DESIGN.md §7.4, private markets).
//
// Neither surface that states a holding's cost states one for these
// units. MT535 carries no BOOK for them, and the statement of assets
// prints a NAV where a listed holding prints its cost. The capital
// calls are the record of what was paid in: ubs-web reads each call
// notice into `advices` (collector migration 0013) with the fund's
// ISIN, the called amount and the value date.
//
// A call names the fund and no account. It is booked onto a position
// only where the ISIN is held on exactly one account across both
// feeds, and only in the calls' own currency; anything else would be a
// guess at which holding paid. The figure is gross: a distribution
// paid back does not reduce it. Charges travel apart from the called
// amount, so the stamp says the fees are excluded.

// paidInSeries is the capital called on one fund over time.
type paidInSeries struct {
	account  string
	currency string
	dates    []int64             // call dates, ascending
	totals   []canonical.Decimal // capital called up to and including dates[i]
}

// at returns the capital called on or before t, or nil before the
// first call: nothing paid in is not a basis of zero, it is no basis.
func (s paidInSeries) at(t int64) *canonical.Decimal {
	i := sort.Search(len(s.dates), func(i int) bool { return s.dates[i] > t })
	if i == 0 {
		return nil
	}
	v := s.totals[i-1]
	return &v
}

// paidInByISIN reads the capital calls and returns, per fund ISIN, the
// series for the one account that holds it. A fund whose calls are
// not all dated and stated in one currency, or that more than one
// account holds, has none.
//
// Silver keys an advice by its document, so a notice that arrives
// twice is two rows. A call is therefore read once per fund, value
// date, currency and amount.
func (r *webReader) paidInByISIN(ctx context.Context, psn *psnReader, safekeepingByPortfolio map[string]string) (map[string]paidInSeries, error) {
	ok, err := r.hasTable(ctx, "advices")
	if err != nil || !ok {
		return nil, err
	}
	series := map[string]*paidInSeries{}
	refused := map[string]bool{}
	if err := silver.ScanRows(ctx, r.db, "ubs-web capital calls", `
SELECT DISTINCT instrument_isin, value_date, currency_iso, amount
  FROM advices
 WHERE kind = 'capital_call' AND instrument_isin IS NOT NULL AND instrument_isin <> ''
 ORDER BY value_date`, func(rows *sql.Rows) error {
		var (
			isin     string
			at       sql.NullInt64
			currency sql.NullString
			amount   sql.NullFloat64
		)
		if err := rows.Scan(&isin, &at, &currency, &amount); err != nil {
			return fmt.Errorf("ubs-web capital calls scan: %w", err)
		}
		s := series[isin]
		if s == nil {
			s = &paidInSeries{currency: currency.String}
			series[isin] = s
		}
		// One call the sum cannot place or price makes every later
		// total wrong, so the fund is refused rather than understated.
		if !at.Valid || !amount.Valid || currency.String == "" || currency.String != s.currency {
			refused[isin] = true
			return nil
		}
		total := canonical.NewDecimalFromFloat(amount.Float64)
		if n := len(s.totals); n > 0 {
			total = total.Add(s.totals[n-1])
		}
		if n := len(s.dates); n > 0 && s.dates[n-1] == at.Int64 {
			s.totals[n-1] = total
			return nil
		}
		s.dates = append(s.dates, at.Int64)
		s.totals = append(s.totals, total)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}

	holders, err := r.fundHolders(ctx, psn, safekeepingByPortfolio, series)
	if err != nil {
		return nil, err
	}
	out := make(map[string]paidInSeries, len(series))
	for isin, s := range series {
		if refused[isin] || len(holders[isin]) != 1 {
			continue
		}
		for account := range holders[isin] {
			s.account = account
		}
		out[isin] = *s
	}
	return out, nil
}

// fundHolders returns, per ISIN in funds, every account a position in
// it is emitted on: the PSN safekeeping accounts that report it, and
// the account each statement portfolio holding it maps to.
func (r *webReader) fundHolders(ctx context.Context, psn *psnReader, safekeepingByPortfolio map[string]string, funds map[string]*paidInSeries) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	add := func(isin, account string) {
		if _, ok := funds[isin]; !ok {
			return
		}
		if out[isin] == nil {
			out[isin] = map[string]bool{}
		}
		out[isin][account] = true
	}
	pairs, err := psn.holdingPairs(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range pairs {
		add(h.isin, h.safekeeping)
	}
	printed, err := r.printedPositions(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range printed {
		if p.isin.Valid {
			account, _ := statementSecuritiesAccount(safekeepingByPortfolio, p.portfolio.String)
			add(p.isin.String, account)
		}
	}
	return out, nil
}

// eachPair runs a query of two text columns and calls fn on each row.
func eachPair(ctx context.Context, db *sql.DB, q string, fn func(a, b string)) error {
	return silver.ScanRows(ctx, db, "", q, func(rows *sql.Rows) error {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		fn(a, b)
		return nil
	})
}

// paidInFold sets the paid-in basis on every position in a fund the
// calls join: same ISIN, same account, same currency, and no book
// value the holding states itself.
type paidInFold struct {
	series map[string]paidInSeries
}

func (s paidInFold) apply(batch *canonical.SnapshotBatch) {
	for i := range batch.Positions {
		p := &batch.Positions[i]
		if p.BookValue != nil {
			continue
		}
		ser, ok := s.series[p.PositionKey]
		if !ok || ser.account != p.AccountExternalID || ser.currency != p.Currency {
			continue
		}
		p.SetBookValue(ser.at(p.SnapshotAt), paidInBasis)
	}
}
