package canonical

import (
	"slices"
	"testing"
)

// Each kind table holds each kind once, and its predicates read the
// rows: a debt is a mortgage or a card, a card alone is invisible to
// returns, and the kinds that hold cash are the bank and broker kinds.
func TestTheKindTablesHoldEachKindOnce(t *testing.T) {
	seen := map[AccountKind]bool{}
	for _, r := range accountKinds {
		if seen[r.Kind] {
			t.Errorf("account kind %s is in the table twice", r.Kind)
		}
		seen[r.Kind] = true
	}
	for _, c := range []struct {
		kind                                     AccountKind
		liability, holdsCash, follows, invisible bool
	}{
		{AccountKindMortgage, true, false, true, false},
		{AccountKindCard, true, false, true, true},
		{AccountKindOverlay, false, false, true, false},
		{AccountKindCash, false, true, false, false},
		{AccountKindBrokerage, false, true, false, false},
		{AccountKindCustody, false, true, false, false},
		{AccountKindSafekeeping, false, true, false, false},
		{AccountKindCrypto, false, false, false, false},
		{"no_such_kind", false, false, false, false},
	} {
		k := c.kind
		if k.Liability() != c.liability || k.HoldsCash() != c.holdsCash || k.FollowsSource() != c.follows ||
			k.ReturnsInvisible() != c.invisible {
			t.Errorf("%s: liability %v holds cash %v follows %v invisible %v", k, k.Liability(), k.HoldsCash(),
				k.FollowsSource(), k.ReturnsInvisible())
		}
	}

	kinds := map[TxKind]bool{}
	for _, r := range txKindRows {
		if kinds[r.Kind] {
			t.Errorf("transaction kind %s is in the table twice", r.Kind)
		}
		kinds[r.Kind] = true
		// A floor is a value of its family, on a side the kind has.
		if r.IncomeFloor != "" && (!ValidIncomeDetailed(r.IncomeFloor) || r.Income == "") {
			t.Errorf("%s floors to %q on income", r.Kind, r.IncomeFloor)
		}
		if r.SpendFloor != "" && (!ValidSpendDetailed(r.SpendFloor) || r.Spending == "") {
			t.Errorf("%s floors to %q on spending", r.Kind, r.SpendFloor)
		}
		// A kind in a family's population has a canonical sign, or is
		// interest, which its side reads by the sign; cash flow excludes
		// no such kind for want of one.
		if r.Unsigned && (r.Spending != "" || r.Income != "" || r.MatcherPool || r.Trade) {
			t.Errorf("%s is unsigned and has a role that reads its sign", r.Kind)
		}
		if r.MatcherPool && canonicalSign(r.Kind) == 0 {
			t.Errorf("%s is in the matcher pool with no canonical sign", r.Kind)
		}
	}
	if !slices.Equal(cashflowUnsignedKinds, []TxKind{TxKindFx, TxKindFxForward, TxKindFxSwap,
		TxKindCorporateAction, TxKindJournal, TxKindOther}) {
		t.Errorf("unsigned kinds = %v", cashflowUnsignedKinds)
	}
}

// Every value with a role is a value of the taxonomy, and each role
// belongs to the family that reads it: an income class to an income
// value, an outflow class or a fee group to a spending one.
func TestEveryRoleIsOnAValueOfItsFamily(t *testing.T) {
	for d := range categoryRoles {
		if _, ok := primaryOf[d]; !ok {
			t.Errorf("%s has roles and is no value of the taxonomy", d)
		}
	}
	for _, c := range SpendCategories {
		r := c.Roles()
		if r.LeavesSpending && !c.Family.InFamily(FamilySpending) {
			t.Errorf("%s leaves the spending base and is no spending value", c.Detailed)
		}
		if (r.LeavesIncome || r.FlowClassIn != "" || r.ProjectedIncome) && !c.Family.InFamily(FamilyIncome) {
			t.Errorf("%s has an income role and is no income value", c.Detailed)
		}
		if (r.FlowLeafDetailed || r.FeeGroup != "") && !c.Family.InFamily(FamilySpending) {
			t.Errorf("%s has a spending role and is no spending value", c.Detailed)
		}
		if r.FlowClassOut != "" && r.FlowPlace != PlaceVehicleDelta && !c.Family.InFamily(FamilySpending) {
			t.Errorf("%s has an outflow class and is no spending value", c.Detailed)
		}
		// A value placed by its verdict leaves the bases it is read in:
		// a line that draws as a movement is no spending and no income.
		if r.FlowPlace != "" && (c.Family.InFamily(FamilySpending) && !r.LeavesSpending ||
			c.Family.InFamily(FamilyIncome) && !r.LeavesIncome) {
			t.Errorf("%s is placed as %s and stays in a base", c.Detailed, r.FlowPlace)
		}
		if (r.FeeGroup != "") != (c.Primary == BankFeesPrimary) {
			t.Errorf("%s: fee group %q under %s", c.Detailed, r.FeeGroup, c.Primary)
		}
	}
}
