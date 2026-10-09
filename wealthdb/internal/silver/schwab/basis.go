package schwab

import (
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Schwab's basis is the sum of a holding's tax lots, with the
// commissions in it: the statements print it, and the Trader API's
// open P/L is measured against it (docs/DESIGN.md §7.4).

// statementBasis stamps a basis Schwab prints: a statement holding's
// cost basis, an open lot's, a realized lot's.
var statementBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesIncluded,
}

// derivedBasis stamps a basis computed as market value minus the open
// P/L Schwab states beside it.
var derivedBasis = canonical.Basis{
	Origin: canonical.BasisDerived, Method: canonical.BasisMethodLots, Fees: canonical.BasisFeesIncluded,
}

// basisFromOpenPL returns the cost basis behind a market value and its
// open P/L, signed like the position: positive for a long holding,
// negative for a short one, as the statements print a short holding's
// basis. For a long the basis is what was paid, market value − P/L.
// For a short it is what the short sale raised, the cost to cover plus
// the P/L; taking the cost to cover as |market value| holds whichever
// sign the market value carries. nil when either figure is missing.
func basisFromOpenPL(marketValue *canonical.Decimal, openPL sql.NullFloat64, short bool) *canonical.Decimal {
	if marketValue == nil || !openPL.Valid {
		return nil
	}
	pl := canonical.NewDecimalFromFloat(openPL.Float64)
	var b canonical.Decimal
	if short {
		b = marketValue.Abs().Add(pl).Neg()
	} else {
		b = marketValue.Sub(pl)
	}
	return &b
}
