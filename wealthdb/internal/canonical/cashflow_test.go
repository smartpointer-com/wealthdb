package canonical

import "testing"

// TestDefaultWrapperSideCoversEveryWrapper is the boundary's
// completeness pin: every value of the TaxWrapper enum has a side, and
// nothing in the table names a wrapper the enum does not hold.
//
// It is the test that matters most here. A wrapper added for a third
// jurisdiction and left out of the table would silently read as
// household — its contributions invisible, its own trades counted as
// the household's investing — and nothing downstream could see the
// absence, because the crossing would be missing rather than wrong.
func TestDefaultWrapperSideCoversEveryWrapper(t *testing.T) {
	for w := range taxWrapperValues {
		if _, ok := defaultWrapperSides[w]; !ok {
			t.Errorf("tax wrapper %q has no cashflow side", w)
		}
	}
	for w := range defaultWrapperSides {
		if !w.Valid() {
			t.Errorf("defaultWrapperSides names %q, which is not a tax wrapper", w)
		}
	}
	if got, want := len(defaultWrapperSides), len(taxWrapperValues); got != want {
		t.Errorf("boundary table has %d wrappers, the enum has %d", got, want)
	}
}

// TestDefaultWrapperSideShape holds every row to the invariant the
// stamped table's CHECK constraints express: a side is a recognised
// one, and a class is present exactly on the vehicle side.
func TestDefaultWrapperSideShape(t *testing.T) {
	for w := range taxWrapperValues {
		side, class := DefaultWrapperSide(w)
		if !side.Valid() {
			t.Errorf("%s: side %q is not a recognised side", w, side)
		}
		switch side {
		case SideVehicle:
			if !class.ValidVehicleClass() {
				t.Errorf("%s is a vehicle but names class %q", w, class)
			}
		default:
			if class != "" {
				t.Errorf("%s is %s and should name no class, got %q", w, side, class)
			}
		}
	}
}

// TestDefaultWrapperSideDecisions pins the boundary's load-bearing
// answers — the ones a reading of "is it reachable?" rather than "is
// it earmarked?" would get wrong, and the pair that lands on opposite
// sides of the same child.
func TestDefaultWrapperSideDecisions(t *testing.T) {
	for _, tc := range []struct {
		wrapper TaxWrapper
		side    WrapperSide
		class   CashflowClass
		why     string
	}{
		{TaxWrapperTaxablePersonal, SideHousehold, "", "a taxable account is the household"},
		{TaxWrapperTrustGrantor, SideHousehold, "", "a grantor trust is tax-transparent and as a rule revocable"},
		{TaxWrapperOther, SideHousehold, "", "an unrecognised wrapper reads as household; the remedy is a new enum value"},
		{TaxWrapperRothIRA, SideVehicle, ClassRetirement, "reachable with a penalty is not this month's money"},
		{TaxWrapperPillar3a, SideVehicle, ClassRetirement, "the Swiss private pension is a retirement pool like the US ones"},
		{TaxWrapper529, SideVehicle, ClassEducation, "an education plan is earmarked, and its owner can take the money back"},
		{TaxWrapperHSA, SideVehicle, ClassHealth, "a health account is earmarked for a cost, not for spending"},
		{TaxWrapperTrustNonGrantor, SideVehicle, ClassTrusts, "a separate taxpayer is not the household"},
		{TaxWrapperCustodialUTMA, SideGiving, "", "a custodial account is legally the minor's the moment it is funded"},
		{TaxWrapperCharitable, SideGiving, "", "an irrevocable charitable transfer is giving"},
		{TaxWrapperFoundation, SideGiving, "", "a foundation's assets are no longer the household's"},
	} {
		side, class := DefaultWrapperSide(tc.wrapper)
		if side != tc.side || class != tc.class {
			t.Errorf("%s = (%q, %q), want (%q, %q): %s",
				tc.wrapper, side, class, tc.side, tc.class, tc.why)
		}
	}
}

// TestDefaultWrapperSideOfUnknown pins the fallback direction. An
// unset wrapper — which gold's render-time default already reads as
// taxable — must land on the household side: the safe direction,
// because nothing is silently removed from the statement.
func TestDefaultWrapperSideOfUnknown(t *testing.T) {
	side, class := DefaultWrapperSide(TaxWrapper(""))
	if side != SideHousehold || class != "" {
		t.Errorf("unset wrapper = (%q, %q), want (household, \"\")", side, class)
	}
	if side, _ := DefaultWrapperSide(TaxWrapper("pillar_4")); side != SideHousehold {
		t.Errorf("unknown wrapper = %q, want household", side)
	}
}

// TestParseWrapperDestination pins the config vocabulary against the
// pairs gold stores, and that `untracked` is refused: it is where a
// crossing goes when there is no far account to read a wrapper off, so
// no wrapper can be moved to it.
func TestParseWrapperDestination(t *testing.T) {
	for _, tc := range []struct {
		in    string
		side  WrapperSide
		class CashflowClass
	}{
		{WrapperDestHousehold, SideHousehold, ""},
		{WrapperDestGiving, SideGiving, ""},
		{WrapperDestRetirement, SideVehicle, ClassRetirement},
		{WrapperDestEducation, SideVehicle, ClassEducation},
		{WrapperDestHealth, SideVehicle, ClassHealth},
		{WrapperDestTrusts, SideVehicle, ClassTrusts},
	} {
		side, class, ok := ParseWrapperDestination(tc.in)
		if !ok || side != tc.side || class != tc.class {
			t.Errorf("ParseWrapperDestination(%q) = (%q, %q, %v), want (%q, %q, true)",
				tc.in, side, class, ok, tc.side, tc.class)
		}
	}
	for _, bad := range []string{"untracked", "vehicle", "Retirement", "", "cash"} {
		if _, _, ok := ParseWrapperDestination(bad); ok {
			t.Errorf("ParseWrapperDestination(%q) was accepted", bad)
		}
	}
	if got, want := len(WrapperDestinations), 6; got != want {
		t.Errorf("WrapperDestinations lists %d words, want %d", got, want)
	}
	for _, d := range WrapperDestinations {
		if _, _, ok := ParseWrapperDestination(d); !ok {
			t.Errorf("WrapperDestinations lists %q, which does not parse", d)
		}
	}
}

// TestVehicleTransferDetailed pins each pool to the delta that stands
// for a crossing the product cannot see the far side of, and that
// `untracked` has none: nothing places a row there, only the absence
// of a far account puts one there.
func TestVehicleTransferDetailed(t *testing.T) {
	for _, tc := range []struct {
		class    CashflowClass
		detailed string
	}{
		{ClassRetirement, DetailedRetirementTransfer},
		{ClassEducation, DetailedEducationTransfer},
		{ClassHealth, DetailedHealthTransfer},
		{ClassTrusts, DetailedTrustTransfer},
	} {
		got, ok := VehicleTransferDetailed(tc.class)
		if !ok || got != tc.detailed {
			t.Errorf("VehicleTransferDetailed(%q) = (%q, %v), want (%q, true)",
				tc.class, got, ok, tc.detailed)
		}
		// Both families read the crossing, because one movement has a
		// leg on each side.
		if !ValidSpendDetailed(tc.detailed) {
			t.Errorf("%q is not a spending value", tc.detailed)
		}
		if !ValidIncomeDetailed(tc.detailed) {
			t.Errorf("%q is not an income value", tc.detailed)
		}
	}
	if _, ok := VehicleTransferDetailed(ClassUntracked); ok {
		t.Error("untracked has a transfer delta; no rule can place one there")
	}
}

// TestCashflowDeltasAreOutOfReachOfTheModel is the fence. Every value
// cashflow adds is decided from structure a counterparty's name cannot
// reveal — which institution holds the far account, and what it is for
// — so a merchant- or payer-keyed verdict must never carry one. A
// model emitting `retirement_transfer` would remove a real receipt
// from the income report; one emitting `debt_repayment` would remove a
// real outflow from spending.
func TestCashflowDeltasAreOutOfReachOfTheModel(t *testing.T) {
	added := []string{
		SpendDetailedDebtRepayment,
		DetailedRetirementTransfer, DetailedEducationTransfer,
		DetailedHealthTransfer, DetailedTrustTransfer,
	}
	for _, v := range added {
		if ModelSpendDetailed(v) {
			t.Errorf("the spend gauntlet admits %q", v)
		}
		if ModelIncomeDetailed(v) {
			t.Errorf("the income gauntlet admits %q", v)
		}
		if CatchAllSpendDetailed(v) || CatchAllIncomeDetailed(v) {
			t.Errorf("%q reads as a catch-all; a delta is a verdict", v)
		}
	}
	// `debt_repayment` is an outflow and belongs to one family alone.
	if !ValidSpendDetailed(SpendDetailedDebtRepayment) {
		t.Error("debt_repayment is not a spending value")
	}
	if ValidIncomeDetailed(SpendDetailedDebtRepayment) {
		t.Error("debt_repayment is an income value; a loan instalment is an outflow")
	}
}

// TestCashflowSectionAndClassVocabulary pins the two enums' shapes:
// six sections, and a class that names a vehicle pool only where a
// wrapper can send a crossing.
func TestCashflowSectionAndClassVocabulary(t *testing.T) {
	sections := []CashflowSection{
		SectionOperatingIn, SectionOperatingOut, SectionInvesting,
		SectionFinancing, SectionVehicles, SectionCash,
	}
	for _, s := range sections {
		if !s.Valid() {
			t.Errorf("section %q is not valid", s)
		}
	}
	if got, want := len(cashflowSectionValues), len(sections); got != want {
		t.Errorf("%d sections declared, %d enumerated", got, want)
	}
	for _, bad := range []CashflowSection{"operating", "vehicle", "", "Cash"} {
		if bad.Valid() {
			t.Errorf("section %q was accepted", bad)
		}
	}
	for _, c := range []CashflowClass{ClassRetirement, ClassEducation, ClassHealth, ClassTrusts} {
		if !c.ValidVehicleClass() {
			t.Errorf("%q is not a vehicle class", c)
		}
	}
	for _, c := range []CashflowClass{ClassUntracked, ClassYield, ClassCash, ""} {
		if c.ValidVehicleClass() {
			t.Errorf("%q was accepted as a vehicle class", c)
		}
	}
}

// TestWrapperSideValid pins the third enum, which the stamped table's
// CHECK restates.
func TestWrapperSideValid(t *testing.T) {
	for _, s := range []WrapperSide{SideHousehold, SideVehicle, SideGiving} {
		if !s.Valid() {
			t.Errorf("side %q is not valid", s)
		}
	}
	for _, s := range []WrapperSide{"", "untracked", "Household"} {
		if s.Valid() {
			t.Errorf("side %q was accepted", s)
		}
	}
}
