package gold

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// fateAccount is an account of the fate table tests: series is its value by
// day, carried forward, and policy its source's knobs.
func fateAccount(policy returns.ReturnsPolicy, series ...dayVal) *accountData {
	a := &accountData{src: "s", acct: "A", series: series, rpolicy: policy}
	a.cacheTail()
	return a
}

// TestFateOfCells holds fateOf to its comment table cell by cell: the
// region a leg's day falls in, then the two policy cells.
func TestFateOfCells(t *testing.T) {
	t.Parallel()
	w := daySpan{from: 50, to: 300}
	var standard returns.ReturnsPolicy
	onboardNone := returns.ReturnsPolicy{OnboardScope: returns.OnboardNone}
	ledgerExact := returns.ReturnsPolicy{ClosureScope: returns.ClosureLedgerExact}
	live := []dayVal{{100, 1000}, {300, 1100}}
	debut := []dayVal{{120, 1000}, {300, 1100}}
	// Zeroes on day 201, a day before the spine's last.
	zeroing := []dayVal{{100, 1000}, {200, 1000}, {201, 0}, {300, 0}}
	// Zeroes on the spine's last day.
	finalDay := []dayVal{{100, 1000}, {299, 1000}, {300, 0}}

	for _, tc := range []struct {
		name     string
		acct     *accountData
		day      int64
		nettable bool
		region   region
		want     fate
	}{
		{"live", fateAccount(standard, live...), 150, false, regionLive, fateFlow},
		{"live, nettable", fateAccount(standard, live...), 150, true, regionLive, fateFlow},
		{"pre-debut", fateAccount(standard, debut...), 110, false, regionPreDebut, fateDropped},
		{"pre-debut, nettable", fateAccount(standard, debut...), 110, true, regionPreDebut, fateDropped},
		{"pre-debut under OnboardNone", fateAccount(onboardNone, debut...), 110, false, regionPreDebut, fateFlow},
		{"pre-debut, nettable, under OnboardNone", fateAccount(onboardNone, debut...), 110, true, regionPreDebut,
			fateDropped},
		{"zero tail before the last day", fateAccount(standard, zeroing...), 250, false, regionZeroTail, fateExit},
		{"zero tail under ClosureLedgerExact", fateAccount(ledgerExact, zeroing...), 250, false, regionZeroTail,
			fateFlow},
		{"zero tail of a final-day zeroing", fateAccount(standard, finalDay...), 300, false, regionZeroTail,
			fateDropped},
	} {
		l := flowLeg{Flow: returns.Flow{Day: tc.day, Amount: -100, ID: "x"}, acct: tc.acct, nettable: tc.nettable,
			region: tc.acct.region(tc.day, w)}
		if l.region != tc.region {
			t.Errorf("%s: region %d, want %d", tc.name, l.region, tc.region)
		}
		if got := fateOf(l); got != tc.want {
			t.Errorf("%s: fate %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestPairFateCells holds pairFate to its three branches: a pre-debut leg
// funds the debut where its partner's value shows the money, a leg on the
// spine moved inside, and a leg before the spine with no shown partner is
// dropped.
func TestPairFateCells(t *testing.T) {
	t.Parallel()
	w := daySpan{from: 50, to: 300}
	var standard returns.ReturnsPolicy
	debuting := fateAccount(standard, dayVal{120, 1000}, dayVal{300, 1100})
	sender := fateAccount(standard, dayVal{100, 5000}, dayVal{300, 4000})
	leg := func(a *accountData, day, valuedOn int64, amt float64) flowLeg {
		return flowLeg{Flow: returns.Flow{Day: day, Amount: amt, ID: "x", ValuedOn: valuedOn}, acct: a, nettable: true,
			region: a.region(day, w)}
	}
	for _, tc := range []struct {
		name        string
		l, partner  flowLeg
		want        fate
		wantPartner fate
	}{
		{"funds a debut", leg(debuting, 110, 120, 1000), leg(sender, 110, 300, -1000), fateFundsDebut, fateInside},
		{"on the spine", leg(sender, 150, 300, -1000), leg(debuting, 150, 300, 1000), fateInside, fateInside},
		{"before the spine, partner unshown", leg(debuting, 110, 120, 1000), leg(sender, 110, 0, -1000),
			fateDropped, fateInside},
	} {
		if got := pairFate(tc.l, tc.partner, w); got != tc.want {
			t.Errorf("%s: fate %d, want %d", tc.name, got, tc.want)
		}
		if got := pairFate(tc.partner, tc.l, w); got != tc.wantPartner {
			t.Errorf("%s: partner fate %d, want %d", tc.name, got, tc.wantPartner)
		}
	}
}

// TestPassThroughCapsAtTheExits: the inside legs pass through in day order,
// as outflows on the days they came in, up to what the exit legs paid out.
func TestPassThroughCapsAtTheExits(t *testing.T) {
	t.Parallel()
	in := []returns.Flow{{Day: 260, Amount: 300, ID: "late"}, {Day: 240, Amount: 500, ID: "early"}}
	got := passThrough(in, -600)
	want := []returns.Flow{{Day: 240, Amount: -500, ID: "early"}, {Day: 260, Amount: -100, ID: "late"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("passThrough = %+v, want %+v", got, want)
	}
	if got := passThrough(in, 0); got != nil {
		t.Errorf("no exits: passThrough = %+v, want none", got)
	}
}
