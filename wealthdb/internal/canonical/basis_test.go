package canonical

import "testing"

func TestValidateBookValuePairsValueAndStamp(t *testing.T) {
	v := NewDecimalFromInt(100)
	full := Basis{Origin: BasisStated, Method: BasisMethodLots, Fees: BasisFeesIncluded}
	cases := []struct {
		name    string
		v       *Decimal
		b       Basis
		wantErr bool
	}{
		{"neither", nil, Basis{}, false},
		{"both", &v, full, false},
		{"value without stamp", &v, Basis{}, true},
		{"stamp without value", nil, full, true},
		{"partial stamp", &v, Basis{Origin: BasisStated, Method: BasisMethodLots}, true},
		{"unknown origin", &v, Basis{Origin: "guessed", Method: BasisMethodLots, Fees: BasisFeesNone}, true},
	}
	for _, c := range cases {
		if err := ValidateBookValue(c.v, c.b); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
	}
}

func TestSetBookValueDropsTheStampWithTheValue(t *testing.T) {
	full := Basis{Origin: BasisDerived, Method: BasisMethodAverage, Fees: BasisFeesExcluded}
	var p PositionChange
	v := NewDecimalFromInt(5)
	p.SetBookValue(&v, full)
	if p.BookValue == nil || p.Basis != full {
		t.Fatalf("set: got %v %+v", p.BookValue, p.Basis)
	}
	p.SetBookValue(nil, full)
	if p.BookValue != nil || !p.Basis.IsZero() {
		t.Fatalf("nil value kept %v %+v", p.BookValue, p.Basis)
	}
}

func TestLotEnumsAdmitOnlyTheirValues(t *testing.T) {
	for _, k := range []RealizedDocKind{RealizedForm1099B, RealizedYearEndSummary,
		RealizedGainLossReport, RealizedClosedPositions, RealizedStatement, RealizedTrade} {
		if !k.Valid() {
			t.Errorf("%q not valid", k)
		}
	}
	if RealizedDocKind("1099b").Valid() {
		t.Error("silver's old kind name accepted")
	}
	if !LotTermShort.Valid() || !LotTermLong.Valid() || LotTerm("SHORT").Valid() {
		t.Error("term vocabulary")
	}
}
