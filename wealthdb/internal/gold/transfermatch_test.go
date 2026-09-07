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

// railLeg is a matcher input that announces a rail and demands one.
func railLeg(owner, id string, day int64, amt float64, rail, partner string) TransferLeg {
	l := leg("bank", owner, id, day, amt)
	l.Rail, l.RailPartner = rail, partner
	return l
}

// A leg that names its rail refuses a partner that is not the rail it needs.
// A card's record of being paid is the shape that needs this: on amount and
// date alone it will pair with any debit of about the right size in the
// window — a utility bill, a payment to a person — and the pair then removes
// BOTH legs from spending, so the debit is real spending that disappears.
func TestMatchTransferLegsHonoursRequiredPartnerRail(t *testing.T) {
	const (
		payment = "card_payment"
		receipt = "card_receipt"
	)
	cases := []struct {
		name  string
		debit TransferLeg
		want  string // id of the credit it should take, "" for no pair
	}{
		{"a receipt refuses a debit that is not a card payment",
			railLeg("checking", "utility-bill", 10, -100, "", ""), ""},
		{"a receipt takes the card payment",
			railLeg("checking", "card-pmt", 10, -100, payment, ""), "receipt"},
		// The demand is one-directional by design: the bank side of a card
		// payment often carries only the issuer's or the holder's name, so a
		// payment must still pair with a credit that announces nothing.
		{"a card payment still pairs with a silent credit",
			railLeg("checking", "card-pmt", 10, -50, payment, ""), "silent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credits := []TransferLeg{
				railLeg("card", "receipt", 10, 100, receipt, payment),
				railLeg("savings", "silent", 10, 50, "", ""),
			}
			got := MatchTransferLegs(append(credits, tc.debit),
				TransferMatchOpts{WindowDays: 5, AllowSameOwner: true})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("paired %q with %q; the rail it demands was not offered",
						got[0].Debit.ID, got[0].Credit.ID)
				}
				return
			}
			if len(got) != 1 || got[0].Credit.ID != tc.want {
				t.Fatalf("got %v, want the debit paired with %q", got, tc.want)
			}
		})
	}
}

// The percentage tolerance absorbs a fee deducted in transit, and such a fee
// is FLAT. Uncapped, the percentage grows with the transfer until it spans
// the band where coincidences live.
func TestMatchTransferLegsCapsToleranceInAbsoluteTerms(t *testing.T) {
	// 0.5% of 10000 is 50, so uncapped this pairs; a real wire fee never is.
	legs := []TransferLeg{
		leg("bank", "checking", "out", 10, -10000),
		leg("broker", "acct", "unrelated-credit", 11, 9954),
	}
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	if got := MatchTransferLegs(append([]TransferLeg(nil), legs...), opts); len(got) != 1 {
		t.Fatalf("fixture is wrong: uncapped, 46 apart on 10000 should pair; got %v", got)
	}
	opts.ToleranceMaxAbs = DefaultTransferFeeCap
	if got := MatchTransferLegs(append([]TransferLeg(nil), legs...), opts); len(got) != 0 {
		t.Errorf("paired %.2f with %.2f: %.0f apart is past the fee cap",
			got[0].Debit.Amt, got[0].Credit.Amt, DefaultTransferFeeCap)
	}
	// A real wire fee still pairs.
	fee := []TransferLeg{
		leg("bank", "checking", "out", 10, -40000),
		leg("broker", "acct", "in", 10, 39980),
	}
	if got := MatchTransferLegs(fee, opts); len(got) != 1 {
		t.Errorf("a 20 wire fee on 40000 must still pair; got %v", got)
	}
}
