package canonical

import (
	"testing"
	"time"
)

// An interest line that pays a bond's coupon names its coupon and its
// maturity, the coupon glued to the maturity's month and year, and names
// the issue; a cash balance's interest names neither. A figure that is
// no coupon (100%), an impossible or unmarked date, and a month and year
// that are not the maturity's read nothing; a CD's line reads, but names
// no bond issue. The lines are invented, in a broker's shapes.
func TestReadCouponLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		desc      string
		ok, issue bool
		coupon    float64
		maturity  string
	}{
		{"US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99", true, true, 9.875, "2099-09-09"},
		{"US TREASURY 9.75%08/98UST BOND DUE 08/08/98", true, true, 9.75, "2098-08-08"},
		{"Bond Interest US TREASURY 1.125%07/97UST NOTE DUE 07/07/2097", true, true, 1.125, "2097-07-07"},
		{"US TREASU NT .375%03/97UST NOTE DUE 03/03/97", true, true, 0.375, "2097-03-03"},
		{"EXAMPLE BANK CD 9.25% DUE 09/09/99", true, false, 9.25, "2099-09-09"},
		{"US TREASU NT 9.875%08/99UST NOTE DUE 09/09/99", false, false, 0, ""},
		{"US TREASU NT 100%09/99UST NOTE DUE 09/09/99", false, false, 0, ""},
		{"US TREASU NT 9.875%02/99UST NOTE DUE 02/30/99", false, false, 0, ""},
		{"US TREASU NT 9.875% UST NOTE DUE: 09/09/99", false, false, 0, ""},
		{"SCHWAB1 INT 01/01-02/01", false, false, 0, ""},
		{"CREDIT INTEREST SCHWAB1 INT 01/01-02/01", false, false, 0, ""},
		{"BANK INTEREST BANK INT 010100-020100 SCHWAB BANK", false, false, 0, ""},
		{"BANK INT 010100-020100 SCHWAB BANK", false, false, 0, ""},
		{"EXAMPLE CORP 9.5% NOTE", false, false, 0, ""},
	} {
		got, ok := ReadCouponLine(c.desc)
		if ok != c.ok || (ok && (got.Coupon != c.coupon || got.Issue != c.issue ||
			got.Maturity.Format(time.DateOnly) != c.maturity)) {
			t.Errorf("%q: %+v %v, want %v %v %v %s", c.desc, got, ok, c.ok, c.issue, c.coupon, c.maturity)
		}
	}
}

// A line pays the bond of its coupon, as far as it prints it, and of its
// maturity to the day; of several bonds, the one that it pays, and none
// where two keys do.
func TestACouponLinePaysOneBond(t *testing.T) {
	t.Parallel()
	line, _ := ReadCouponLine("US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99")
	short, _ := ReadCouponLine("US TREASU NT 9.87%09/99UST NOTE DUE 09/09/99")
	coupon, other := 9.875, 9.75
	maturity, later := time.Date(2099, 9, 9, 0, 0, 0, 0, time.UTC), time.Date(2099, 9, 10, 0, 0, 0, 0, time.UTC)
	if !line.Pays(&coupon, &maturity) || line.Pays(&other, &maturity) || line.Pays(&coupon, &later) ||
		line.Pays(nil, &maturity) || line.Pays(&coupon, nil) || !short.Pays(&coupon, &maturity) {
		t.Error("pays the wrong bonds")
	}
	type bond struct {
		key      string
		coupon   float64
		maturity time.Time
	}
	one := func(bonds ...bond) (string, bool) {
		return line.OnePaying(len(bonds), func(i int) (string, *float64, *time.Time) {
			return bonds[i].key, &bonds[i].coupon, &bonds[i].maturity
		})
	}
	if key, ok := one(bond{"A", 9.875, maturity}, bond{"B", 9.75, maturity}, bond{"A", 9.875, maturity}); key != "A" ||
		!ok {
		t.Errorf("one bond: %q %v", key, ok)
	}
	if key, ok := one(bond{"A", 9.875, maturity}, bond{"C", 9.875, maturity}); ok {
		t.Errorf("two bonds: %q", key)
	}
	if _, ok := one(bond{"B", 9.75, maturity}); ok {
		t.Error("no bond")
	}
}
