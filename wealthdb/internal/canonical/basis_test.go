package canonical

import (
	"testing"
	"time"
)

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
		t.Error("a kind outside the vocabulary accepted")
	}
	if !LotTermShort.Valid() || !LotTermLong.Valid() || LotTerm("SHORT").Valid() {
		t.Error("term vocabulary")
	}
}

func TestParseLotTermReadsAnyCase(t *testing.T) {
	for in, want := range map[string]LotTerm{
		"SHORT": LotTermShort, " long ": LotTermLong, "Short": LotTermShort,
		"": "", "ST": "", "various": "",
	} {
		if got := ParseLotTerm(in); got != want {
			t.Errorf("ParseLotTerm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLotStampsTravelWithTheValue(t *testing.T) {
	v := NewDecimalFromInt(3)
	var l PositionLotChange
	l.SetBookValue(&v, BasisStated)
	if err := ValidateLotOrigin(l.BookValue, l.BasisOrigin); err != nil || l.BasisOrigin != BasisStated {
		t.Errorf("set: %v %q", err, l.BasisOrigin)
	}
	l.SetBookValue(nil, BasisStated)
	if l.BookValue != nil || l.BasisOrigin != "" {
		t.Errorf("nil value kept %v %q", l.BookValue, l.BasisOrigin)
	}
	if ValidateLotOrigin(&v, "") == nil || ValidateLotOrigin(nil, BasisStated) == nil {
		t.Error("a mismatched origin passed")
	}
	var r RealizedLotChange
	r.SetBookValue(nil, Basis{Origin: BasisStated, Method: BasisMethodLots, Fees: BasisFeesIncluded})
	if !r.Basis.IsZero() {
		t.Errorf("realized stamp without a value: %+v", r.Basis)
	}
}

func TestEarliestLotDate(t *testing.T) {
	early := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	late := time.Date(2021, 1, 2, 0, 0, 0, 0, time.UTC)
	lots := []PositionLotChange{{AcquisitionDate: &late}, {}, {AcquisitionDate: &early}}
	if got := EarliestLotDate(lots); got == nil || !got.Equal(early) {
		t.Errorf("EarliestLotDate = %v", got)
	}
	if EarliestLotDate([]PositionLotChange{{}}) != nil {
		t.Error("a lot set with no dates gave one")
	}
}
