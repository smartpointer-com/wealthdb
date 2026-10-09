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
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT instrument_isin, value_date, currency_iso, amount
  FROM advices
 WHERE kind = 'capital_call' AND instrument_isin IS NOT NULL AND instrument_isin <> ''
 ORDER BY value_date`)
	if err != nil {
		return nil, fmt.Errorf("ubs-web capital calls: %w", err)
	}
	defer rows.Close()
	series := map[string]*paidInSeries{}
	refused := map[string]bool{}
	for rows.Next() {
		var (
			isin     string
			at       sql.NullInt64
			currency sql.NullString
			amount   sql.NullFloat64
		)
		if err := rows.Scan(&isin, &at, &currency, &amount); err != nil {
			return nil, fmt.Errorf("ubs-web capital calls scan: %w", err)
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
			continue
		}
		total := canonical.NewDecimalFromFloat(amount.Float64)
		if n := len(s.totals); n > 0 {
			total = total.Add(s.totals[n-1])
		}
		if n := len(s.dates); n > 0 && s.dates[n-1] == at.Int64 {
			s.totals[n-1] = total
			continue
		}
		s.dates = append(s.dates, at.Int64)
		s.totals = append(s.totals, total)
	}
	if err := rows.Err(); err != nil {
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
	if psn != nil && psn.db != nil {
		if err := eachPair(ctx, psn.db, `SELECT DISTINCT isin, safekeeping_external_id FROM holdings`,
			func(isin, account string) { add(isin, account) }); err != nil {
			return nil, fmt.Errorf("ubs psn fund holders: %w", err)
		}
	}
	ok, err := r.hasTable(ctx, "historical_position_snapshots")
	if err != nil {
		return nil, err
	}
	if ok {
		if err := eachPair(ctx, r.db, `
SELECT DISTINCT instrument_isin, portfolio_external_id
  FROM historical_position_snapshots
 WHERE instrument_isin IS NOT NULL`,
			func(isin, portfolio string) {
				account, _ := statementSecuritiesAccount(safekeepingByPortfolio, portfolio)
				add(isin, account)
			}); err != nil {
			return nil, fmt.Errorf("ubs-web fund holders: %w", err)
		}
	}
	return out, nil
}

// eachPair runs a query of two text columns and calls fn on each row.
func eachPair(ctx context.Context, db *sql.DB, q string, fn func(a, b string)) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		fn(a, b)
	}
	return rows.Err()
}

// paidInStream sets the paid-in basis on every position in a fund the
// calls join: same ISIN, same account, same currency, and no book
// value the holding states itself.
type paidInStream struct {
	inner  silver.SnapshotStream
	series map[string]paidInSeries
}

func (s *paidInStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	batch, more, err := s.inner.Next(ctx)
	if err != nil {
		return batch, more, err
	}
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
	return batch, more, nil
}

func (s *paidInStream) Close() error { return s.inner.Close() }
