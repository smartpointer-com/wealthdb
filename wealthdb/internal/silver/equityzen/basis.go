package equityzen

import (
	"database/sql"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// stake is what a deal's book value is computed from, as silver states
// it: the SPV's cost of the shares still held at the price paid
// (`cost_basis_remaining`, the collector's shares held × price paid), the
// capital paid in (`offerings.basis`, the investment size), the shares
// held and bought, and the execution fee charged on the purchase.
type stake struct {
	spv                             bool
	cost, paidIn, held, bought, fee sql.NullFloat64
}

// bookValue is a deal's book value and its stamp (docs/DESIGN.md §7.4),
// with the part of the execution fee it adds (nil where none is added).
// Purchase fees are part of basis.
//
//   - An SPV's is the shares still held at the price paid, plus the fee
//     in the same proportion: fee × shares held ÷ shares bought, the
//     ratio held at 1. A partial sale takes the same share of the fee as
//     it takes of the cost. Stamped average.
//   - A fund's is the capital paid in, gross, plus the whole fee: the
//     private-market definition. The collector replays a fund's
//     distributions like an SPV's sales, so its remaining cost would fall
//     with capital paid back. Stamped paid_in.
//
// Both are collector or adapter arithmetic over stated figures, so the
// origin is derived. A purchase that states no fee leaves the fee out
// and its treatment unknown. An SPV whose shares bought are not stated
// cannot apportion a stated fee, so it is excluded.
func bookValue(d stake) (v *canonical.Decimal, b canonical.Basis, feeShare *canonical.Decimal) {
	b = canonical.Basis{Origin: canonical.BasisDerived, Method: canonical.BasisMethodAverage}
	base := d.cost
	if !d.spv {
		b.Method, base = canonical.BasisMethodPaidIn, d.paidIn
	}
	c := silver.DecimalPtrFromNullFloat(base)
	if c == nil {
		return nil, canonical.Basis{}, nil
	}
	if !d.fee.Valid {
		b.Fees = canonical.BasisFeesUnknown
		return c, b, nil
	}
	share := decimal.NewFromFloat(d.fee.Float64)
	if d.spv {
		if !d.held.Valid || !d.bought.Valid || d.bought.Float64 <= 0 {
			b.Fees = canonical.BasisFeesExcluded
			return c, b, nil
		}
		ratio := decimal.NewFromFloat(d.held.Float64).Div(decimal.NewFromFloat(d.bought.Float64))
		share = share.Mul(decimal.Min(ratio, decimal.NewFromInt(1)))
	}
	share = share.Round(2)
	sum := c.Add(share)
	b.Fees = canonical.BasisFeesIncluded
	return &sum, b, &share
}
