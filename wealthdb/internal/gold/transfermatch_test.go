package gold

import (
	"reflect"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// leg is a shorthand for a same-currency matcher input.
func leg(group, owner, id string, day int64, amt float64) TransferLeg {
	return TransferLeg{Group: group, Owner: owner, ID: id, Day: day, Ccy: "USD", Amt: amt}
}

// TestMatchCrossTransfersIndexesTransferLike pins the returns wrapper's SECOND
// side effect: linking an account must also index its transferLike slice, or
// crossMatchedDrops silently reads every linked leg as a nonTransfer one and
// applies the wrong subsumption rule. Accounts without links stay unindexed.
func TestMatchCrossTransfersIndexesTransferLike(t *testing.T) {
	byKey := map[string]*accountData{
		acctKey("s1", "A"): {src: "s1", acct: "A", transferLike: []returns.Flow{
			{Day: 100, Amount: -5000, ID: "t1"},
			{Day: 100, Amount: -25, ID: "fee"},
		}},
		acctKey("s2", "B"): {src: "s2", acct: "B", nonTransfer: []returns.Flow{
			{Day: 102, Amount: 5000, ID: "t2"},
		}},
		acctKey("s2", "C"): {src: "s2", acct: "C", transferLike: []returns.Flow{
			{Day: 100, Amount: 42, ID: "unlinked"},
		}},
	}
	matchCrossTransfers([]crossCandidate{
		{src: "s1", acct: "A", txID: "t1", day: 100, ccy: "USD", amt: -5000},
		{src: "s2", acct: "B", txID: "t2", day: 102, ccy: "USD", amt: 5000},
	}, &TransferMatching{WindowDays: 5, TolerancePct: 0.5}, byKey)

	want := map[string]bool{"t1": true, "fee": true}
	if got := byKey[acctKey("s1", "A")].transferLikeIDs; !reflect.DeepEqual(got, want) {
		t.Errorf("sender index = %v, want the whole transferLike slice %v", got, want)
	}
	// The receiver's leg is a nonTransfer one, but the account is linked, so it
	// still gets an (empty, non-nil) index — crossMatchedDrops distinguishes
	// "not transfer-like" from "never indexed".
	if got := byKey[acctKey("s2", "B")].transferLikeIDs; got == nil || len(got) != 0 {
		t.Errorf("receiver index = %v, want empty and non-nil", got)
	}
	if got := byKey[acctKey("s2", "C")].transferLikeIDs; got != nil {
		t.Errorf("unlinked account index = %v, want nil (the map stays sparse)", got)
	}
}

// TestMatchTransferLegsWithinGroup pins the knob the returns path never sets:
// with CrossGroupOnly off, two accounts of the SAME group pair — but a single
// account still does not pair with itself until AllowSameOwner says so.
func TestMatchTransferLegsWithinGroup(t *testing.T) {
	legs := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "savings", "in", 101, 5000),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5})
	if len(got) != 1 || got[0].Debit.ID != "out" || got[0].Credit.ID != "in" {
		t.Errorf("within-group pair = %+v, want out→in", got)
	}
	if got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, CrossGroupOnly: true}); len(got) != 0 {
		t.Errorf("CrossGroupOnly must reject the same-group pair, got %+v", got)
	}

	self := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "in", 101, 5000),
	}
	if got := MatchTransferLegs(self, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}); len(got) != 0 {
		t.Errorf("an account must not pair with itself while AllowSameOwner is off, got %+v", got)
	}
}

// TestMatchTransferLegsSameOwner pins AllowSameOwner, the knob the spending
// caller sets and the returns caller never does: a same-account round trip
// pairs only under it, CrossGroupOnly still forbids the whole group first,
// and on an exact tie the partner on ANOTHER account beats the one on the
// debit's own.
func TestMatchTransferLegsSameOwner(t *testing.T) {
	on := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true}
	roundTrip := func() []TransferLeg {
		return []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "back", 100, 5000),
		}
	}
	got := MatchTransferLegs(roundTrip(), on)
	if len(got) != 1 || got[0].Debit.ID != "out" || got[0].Credit.ID != "back" {
		t.Errorf("same-account round trip under AllowSameOwner = %+v, want out→back", got)
	}
	off := on
	off.AllowSameOwner = false
	if got := MatchTransferLegs(roundTrip(), off); len(got) != 0 {
		t.Errorf("with AllowSameOwner off the round trip must not pair, got %+v", got)
	}
	crossOnly := on
	crossOnly.CrossGroupOnly = true
	if got := MatchTransferLegs(roundTrip(), crossOnly); len(got) != 0 {
		t.Errorf("CrossGroupOnly must still refuse the same-group pair, got %+v", got)
	}

	// A withdrawal with two exact, same-day candidates: a credit on its own
	// account and the real receiving leg elsewhere. The far side wins; the
	// own-account credit is left for what it is.
	tie := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "payroll", 100, 5000),
		leg("broker", "cash", "in", 100, 5000),
	}
	got = MatchTransferLegs(tie, on)
	if len(got) != 1 || got[0].Credit.Group != "broker" || got[0].Credit.ID != "in" {
		t.Errorf("tie between own-account and other-account partner = %+v, want out→broker/in", got)
	}
	// The preference is a tie-break only: a strictly better own-account
	// partner (exact amount versus one inside the tolerance) still wins.
	closer := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "back", 100, 5000),
		leg("broker", "cash", "in", 100, 4990),
	}
	got = MatchTransferLegs(closer, on)
	if len(got) != 1 || got[0].Credit.ID != "back" {
		t.Errorf("exact own-account partner versus approximate far one = %+v, want out→back", got)
	}
}
