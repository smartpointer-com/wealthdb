package canonical

// LotsAddUp reports whether lots add up to a position of quantity qty
// and book value cost: the lots' quantities sum to qty within a
// millionth of it (at least 1e-6), and their costs to cost within a cent
// per lot where cost and every lot's cost are stated. figures reads a
// lot's quantity and cost. magnitudes sums the lots' magnitudes against
// the magnitude of qty, for a source that prints a short lot unsigned;
// otherwise the signed quantities sum against qty. A position or a lot
// that states no quantity cannot be shown to add up.
func LotsAddUp[L any](qty, cost *Decimal, lots []L, figures func(L) (qty, cost *Decimal), magnitudes bool) bool {
	if qty == nil || len(lots) == 0 {
		return false
	}
	var sumQty, sumCost Decimal
	costsStated := cost != nil
	for _, l := range lots {
		q, c := figures(l)
		if q == nil {
			return false
		}
		if magnitudes {
			sumQty = sumQty.Add(q.Abs())
		} else {
			sumQty = sumQty.Add(*q)
		}
		if c == nil {
			costsStated = false
		} else {
			sumCost = sumCost.Add(*c)
		}
	}
	want := *qty
	if magnitudes {
		want = want.Abs()
	}
	scale := qty.Abs()
	if one := NewDecimalFromInt(1); scale.LessThan(one) {
		scale = one
	}
	if sumQty.Sub(want).Abs().GreaterThan(NewDecimalFromFloat(1e-6).Mul(scale)) {
		return false
	}
	perLot := NewDecimalFromFloat(0.01).Mul(NewDecimalFromInt(int64(len(lots))))
	return !costsStated || sumCost.Sub(*cost).Abs().LessThanOrEqual(perLot)
}
