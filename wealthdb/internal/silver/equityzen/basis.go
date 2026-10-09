package equityzen

import (
	"database/sql"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The stamps on equityzen's book value (docs/DESIGN.md §7.4).
var (
	// feeBasis: the cost of the stake still held at the price paid, plus
	// the same share of the execution fee EquityZen charged on top of the
	// purchase.
	feeBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesIncluded,
	}
	// costBasis: the cost of the stake still held at the price paid, for a
	// deal whose purchase states no execution fee. The fee is not in it.
	costBasis = canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesExcluded,
	}
)

// bookValue is a deal's book value: `cost_basis_remaining`, the shares
// still held at the price paid, plus the execution fee paid on the
// purchase in the same proportion (purchase fees are part of basis). A
// partial sale takes the same share of the fee as it takes of the cost,
// so an SPV keeps fee × shares held ÷ shares bought. A fund has no share
// count and nothing sold piecemeal, so it keeps the whole fee while it is
// held. A purchase that states no fee, or an SPV with no shares bought to
// divide by, leaves the cost as stated, without the fee. feeShare is the
// part of the fee added, nil where none is.
func bookValue(spv bool, cost, held, bought, fee sql.NullFloat64) (v *canonical.Decimal, b canonical.Basis, feeShare *canonical.Decimal) {
	c := silver.DecimalPtrFromNullFloat(cost)
	if c == nil {
		return nil, canonical.Basis{}, nil
	}
	if !fee.Valid {
		return c, costBasis, nil
	}
	share := decimal.NewFromFloat(fee.Float64)
	if spv {
		if !held.Valid || !bought.Valid || bought.Float64 <= 0 {
			return c, costBasis, nil
		}
		share = share.Mul(decimal.NewFromFloat(held.Float64)).Div(decimal.NewFromFloat(bought.Float64))
	}
	share = share.Round(2)
	sum := c.Add(share)
	return &sum, feeBasis, &share
}
