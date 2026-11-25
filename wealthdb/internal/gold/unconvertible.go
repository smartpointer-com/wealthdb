package gold

import (
	"context"
	"fmt"
	"slices"
)

// Unconvertible are the currencies gold holds amounts in that no
// exchange rate connects to target, sorted: an amount in one of them is
// missing from every figure a report converts into target.
//
// The currencies are those of positions, cash_balances and
// transactions, the three tables every converted figure starts from:
// lots, realized lots and the derived tables carry the currencies of
// these rows.
//
// A currency converts when the reports' own conversion finds a rate for
// it (derived/10_fx.sql): it is target, or fx_norm holds the pair in
// either direction, or holds both legs of the path through CHF, or both
// legs of the path through USD. A pair with a rate on any day converts
// on every day, since a report reads the nearest rate at or before a
// line's day and each pair's earliest rate reaches back to day 0
// (fx_daily). So a day with no rate is no case of this; a pair with no
// rate at all is.
func Unconvertible(ctx context.Context, db rowQuerier, target string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT currency, NULL FROM (
    SELECT DISTINCT currency FROM positions
    UNION SELECT DISTINCT currency FROM cash_balances
    UNION SELECT DISTINCT currency FROM transactions)
UNION ALL
SELECT from_ccy, to_ccy FROM (SELECT DISTINCT from_ccy, to_ccy FROM fx_norm)`)
	if err != nil {
		return nil, fmt.Errorf("gold: read the currencies and their rates: %w", err)
	}
	defer rows.Close()
	var held []string
	pairs := map[[2]string]bool{}
	for rows.Next() {
		var from string
		var to *string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("gold: read the currencies and their rates: %w", err)
		}
		if to == nil {
			held = append(held, from)
		} else {
			pairs[[2]string{from, *to}] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gold: read the currencies and their rates: %w", err)
	}
	// rate says a rate from a to b is there: the identity, or the pair.
	rate := func(a, b string) bool { return a == b || pairs[[2]string{a, b}] }
	var out []string
	for _, c := range held {
		if c == "" || rate(c, target) || rate(c, "CHF") && rate("CHF", target) || rate(c, "USD") && rate("USD", target) {
			continue
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return out, nil
}
