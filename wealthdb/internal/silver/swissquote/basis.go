package swissquote

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// averageCostBasis stamps a position's book value: the quantity at the
// average cost Swissquote states per unit. That average leaves the
// purchase fees out (docs/DESIGN.md §7.4).
var averageCostBasis = canonical.Basis{
	Origin: canonical.BasisDerived, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesExcluded,
}

// quotePercent is silver's price_quote for a price in percent of
// nominal: a statement prints a bond's prices that way.
const quotePercent = "percent"

// atQuote turns quantity × price into a value. A percent quote is a
// percent of the nominal the quantity counts, so the product is
// divided by 100.
func atQuote(qty, price canonical.Decimal, quote string) canonical.Decimal {
	v := qty.Mul(price)
	if quote == quotePercent {
		return v.Shift(-2)
	}
	return v
}

// bookValue is the quantity at the average cost, in the row's
// currency, or nil where either is not stated.
func bookValue(qty, averageCost *canonical.Decimal, quote string) *canonical.Decimal {
	if qty == nil || averageCost == nil {
		return nil
	}
	v := atQuote(*qty, *averageCost, quote)
	return &v
}
