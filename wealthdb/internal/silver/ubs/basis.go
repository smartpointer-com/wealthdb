package ubs

import (
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The basis stamps this adapter writes (docs/DESIGN.md §7.4). UBS keeps
// a holding at its weighted average cost and states that cost without
// the purchase fees, on every surface: the MT535 BOOK amount, the
// statement of assets' cost value and the transaction list's cost of
// a sale. A private-markets fund's units carry no cost on any of them;
// their basis is the capital its calls took.
var (
	statedAverageBasis = canonical.Basis{
		Origin: canonical.BasisStated, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesExcluded,
	}
	derivedAverageBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodAverage, Fees: canonical.BasisFeesExcluded,
	}
	paidInBasis = canonical.Basis{
		Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesExcluded,
	}
)

// holdingCost is the cost an MT535 holding states, as ubs-psn promotes
// it (collector migration 0005): the BOOK amount and its currency, and
// the average acquisition rate (AEXR) from the instrument's currency to
// the reference currency. One unit of `from` is `rate` units of `to`.
type holdingCost struct {
	basis    sql.NullFloat64
	currency sql.NullString
	rate     sql.NullFloat64
	from, to sql.NullString
}

// psnBookValue returns an MT535 holding's book value in the position's
// currency, its stamp and the payload the position carries.
//
// BOOK in the position's currency is taken as is. BOOK in another
// currency converts only at the rate the holding itself states: AEXR,
// when it runs from BOOK's currency to the position's. Any other
// holding keeps no book value, and its stated cost travels in the
// payload so nothing the source said is lost.
func psnBookValue(c holdingCost, positionCcy, payload string) (*canonical.Decimal, canonical.Basis, json.RawMessage) {
	if !c.basis.Valid {
		return nil, canonical.Basis{}, json.RawMessage(payload)
	}
	book := canonical.NewDecimalFromFloat(c.basis.Float64)
	if c.currency.String == positionCcy {
		return &book, statedAverageBasis, json.RawMessage(payload)
	}
	if c.rate.Valid && c.rate.Float64 > 0 &&
		c.from.String == c.currency.String && c.to.String == positionCcy {
		v := book.Mul(canonical.NewDecimalFromFloat(c.rate.Float64))
		return &v, derivedAverageBasis, json.RawMessage(payload)
	}
	out := payload
	for _, f := range []struct{ key, value string }{
		{costBasisKey, formatFloat(c.basis)},
		{costCurrencyKey, c.currency.String},
		{acquisitionFxRateKey, formatFloat(c.rate)},
		{acquisitionFxFromKey, c.from.String},
		{acquisitionFxToKey, c.to.String},
	} {
		out = string(spliceStringField(out, f.key, f.value))
	}
	return nil, canonical.Basis{}, json.RawMessage(out)
}

// statementBookValue returns a statement-of-assets holding's book value
// and the payload its position carries.
//
// The statement's cost value is the units at their average cost and
// average buy rate, printed in the portfolio's currency, which is the
// position's: the book value as stated. A holding printed without one
// keeps no book value. Its cost price, in the instrument's currency,
// travels in the payload instead. Multiplying it out would be wrong for
// a bond, whose cost price is a percent of the nominal.
func statementBookValue(costBasis, costPrice sql.NullFloat64, instrumentCcy, portfolioCcy, payload string) (*canonical.Decimal, canonical.Basis, json.RawMessage) {
	if costBasis.Valid && portfolioCcy != "" {
		v := canonical.NewDecimalFromFloat(costBasis.Float64)
		return &v, statedAverageBasis, json.RawMessage(payload)
	}
	if !costPrice.Valid {
		return nil, canonical.Basis{}, json.RawMessage(payload)
	}
	out := spliceStringField(payload, costPriceKey, formatFloat(costPrice))
	return nil, canonical.Basis{}, spliceStringField(string(out), costCurrencyKey, instrumentCcy)
}

// The payload keys a stated cost travels under when it cannot become a
// book value.
const (
	costBasisKey         = `"cost_basis":`
	costPriceKey         = `"cost_price":`
	costCurrencyKey      = `"cost_currency":`
	acquisitionFxRateKey = `"acquisition_fx_rate":`
	acquisitionFxFromKey = `"acquisition_fx_from":`
	acquisitionFxToKey   = `"acquisition_fx_to":`
)

// formatFloat spells a silver REAL the shortest way that reads back
// exactly, or "" for NULL (which spliceStringField then leaves out).
func formatFloat(f sql.NullFloat64) string {
	if !f.Valid {
		return ""
	}
	return strconv.FormatFloat(f.Float64, 'f', -1, 64)
}
