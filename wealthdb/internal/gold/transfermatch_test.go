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

// refLeg is a matcher input carrying the reference its source stamped on both
// halves of one movement, in a currency of its own.
func refLeg(owner, id string, day int64, ccy string, amt float64, ref string) TransferLeg {
	l := leg("bank", owner, id, day, amt)
	l.Ccy, l.Ref = ccy, ref
	return l
}

// A shared reference pairs two legs the amount phase can never see, because
// they are denominated differently. An FX conversion between two accounts of
// one holder is booked as a debit in one currency and a credit in another,
// the two figures differ by the rate, and the currency partition that keeps a
// report's display currency from deciding what counts as spending puts them
// in separate pools forever. The bank stamped one transaction number on both,
// and that is an identity rather than a guess, so it crosses the partition.
func TestSharedReferencePairsAcrossCurrencies(t *testing.T) {
	legs := []TransferLeg{
		refLeg("usd-account", "out", 100, "USD", -1000, "TXN-1"),
		refLeg("chf-account", "in", 100, "CHF", 987.65, "TXN-1"),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true})
	if len(got) != 1 || got[0].Debit.ID != "out" || got[0].Credit.ID != "in" {
		t.Fatalf("cross-currency reference pair = %+v, want out→in", got)
	}
	// Nothing about the pair was inferred from the figures, so the two legs
	// come back exactly as they went in, differing amounts and all.
	if got[0].Debit.Ccy == got[0].Credit.Ccy {
		t.Errorf("the pair's legs share a currency: the fixture no longer tests what it claims")
	}
}

// The reference spends neither the tolerance nor the amount pass's window,
// because neither bounds an identity: two legs orders of magnitude apart, on
// days no window would admit, still pair when the source says they are one
// movement. What DOES bound it is the reference space going stale — a "pair"
// resolving across a span no settlement takes is a reused string, not a
// movement.
func TestSharedReferenceSpendsNoToleranceAndOnlyAStalenessBound(t *testing.T) {
	within := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -25, "TXN-2"),
		refLeg("savings", "in", 100+referenceMatchMaxDays, "USD", 900000, "TXN-2"),
	}
	if got := MatchTransferLegs(within, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}); len(got) != 1 {
		t.Errorf("a reference pair must not be bounded by the amount band or the amount window, got %+v", got)
	}
	stale := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -25, "TXN-2b"),
		refLeg("savings", "in", 100+referenceMatchMaxDays+1, "USD", 900000, "TXN-2b"),
	}
	if got := MatchTransferLegs(stale, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}); len(got) != 0 {
		t.Errorf("a reference resolving past the staleness bound must pair nothing, got %+v", got)
	}
}

// Every pair says which phase asserted it, because an audit of the matcher
// cannot be read without it: a correct reference pair's two legs are supposed
// to disagree in amount and currency, which is exactly what an over-eager
// amount pair looks like.
func TestEveryPairNamesThePhaseThatAssertedIt(t *testing.T) {
	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "checking", "manual-out"}, Credit: LegRef{"bank", "wallet", "manual-in"},
	}})
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	legs := []TransferLeg{
		leg("bank", "checking", "manual-out", 100, -11),
		leg("bank", "wallet", "manual-in", 300, 999),
		refLeg("checking", "ref-out", 100, "USD", -500, "TXN-12"),
		refLeg("savings", "ref-in", 100, "CHF", 440, "TXN-12"),
		leg("bank", "brokerage", "plain-out", 100, -75),
		leg("bank", "custody", "plain-in", 100, 75),
	}
	want := map[string]TransferMatchPhase{
		"manual-out": MatchedByOverride,
		"ref-out":    MatchedByReference,
		"plain-out":  MatchedByAmount,
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, Overrides: forced})
	if len(got) != len(want) {
		t.Fatalf("matched %d pair(s), want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		if p.By != want[p.Debit.ID] {
			t.Errorf("%s was asserted by %q, want %q", p.Debit.ID, p.By, want[p.Debit.ID])
		}
	}
}

// A reference that does not name exactly one debit and one credit names no
// movement this pool can resolve, and the phase refuses rather than guessing
// which two legs were meant. Refusing is not a lost match: a false pair
// withdraws BOTH legs, so the spending line the debit stood for is not
// mislabelled but deleted.
func TestSharedReferenceRefusesWhatItCannotResolve(t *testing.T) {
	opts := TransferMatchOpts{AllowSameOwner: true}
	cases := []struct {
		name string
		legs []TransferLeg
	}{
		{"three legs under one reference", []TransferLeg{
			refLeg("checking", "out", 100, "USD", -500, "TXN-3"),
			refLeg("savings", "in", 100, "CHF", 440, "TXN-3"),
			refLeg("brokerage", "also-in", 100, "CHF", 440, "TXN-3"),
		}},
		{"two legs in the same direction", []TransferLeg{
			refLeg("checking", "out", 100, "USD", -500, "TXN-4"),
			refLeg("savings", "also-out", 100, "CHF", -440, "TXN-4"),
		}},
		{"two legs of one account", []TransferLeg{
			refLeg("checking", "charge", 100, "USD", -500, "TXN-5"),
			refLeg("checking", "credit", 100, "CHF", 440, "TXN-5"),
		}},
		{"a leg with no amount to move", []TransferLeg{
			refLeg("checking", "out", 100, "USD", -500, "TXN-6"),
			refLeg("savings", "in", 100, "CHF", 0, "TXN-6"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchTransferLegs(append([]TransferLeg(nil), tc.legs...), opts); len(got) != 0 {
				t.Errorf("paired %+v", got)
			}
		})
	}
}

// A reference is an identity only inside one source's id space. Two banks can
// mint the same string, so a pair drawn across the seam would be a
// coincidence wearing the clothes of a fact — and under CrossGroupOnly, which
// forbids same-group pairing outright, the phase has nothing left to do at
// all.
func TestSharedReferenceNeverCrossesSources(t *testing.T) {
	legs := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -500, "TXN-7"),
		{Group: "other-bank", Owner: "acct", ID: "in", Day: 100, Ccy: "CHF", Amt: 440, Ref: "TXN-7"},
	}
	if got := MatchTransferLegs(append([]TransferLeg(nil), legs...), TransferMatchOpts{AllowSameOwner: true}); len(got) != 0 {
		t.Errorf("a bare reference must not pair across sources, got %+v", got)
	}
	sameSource := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -500, "TXN-8"),
		refLeg("savings", "in", 100, "CHF", 440, "TXN-8"),
	}
	if got := MatchTransferLegs(append([]TransferLeg(nil), sameSource...),
		TransferMatchOpts{CrossGroupOnly: true}); len(got) != 0 {
		t.Errorf("CrossGroupOnly must leave the reference phase with nothing to pair, got %+v", got)
	}
}

// The holder outranks the clerk. An `unmatch` says these two rows are not one
// movement whatever is stamped on them, and it is the one claim about a pair
// that beats an identity; a `match` is asserted first and takes its legs out
// of the pool, so the reference phase finds one half already spoken for and
// refuses the rest.
func TestManualOverridesOutrankASharedReference(t *testing.T) {
	legs := func() []TransferLeg {
		return []TransferLeg{
			refLeg("checking", "out", 100, "USD", -500, "TXN-9"),
			refLeg("savings", "in", 100, "CHF", 440, "TXN-9"),
			leg("bank", "brokerage", "coincidence", 100, 500),
		}
	}
	unmatched, err := newTransferOverrides(nil, [][2]LegRef{{
		{"bank", "checking", "out"}, {"bank", "savings", "in"},
	}}, nil)
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	got := MatchTransferLegs(legs(), TransferMatchOpts{Overrides: unmatched})
	if len(got) != 1 || got[0].Credit.ID != "coincidence" {
		t.Errorf("an unmatched reference pair = %+v, want the debit left to the amount phase", got)
	}

	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "checking", "out"}, Credit: LegRef{"bank", "brokerage", "coincidence"},
	}})
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	got = MatchTransferLegs(legs(), TransferMatchOpts{Overrides: forced})
	if len(got) != 1 || got[0].Credit.ID != "coincidence" {
		t.Errorf("a forced pair over a reference twin = %+v, want the forced pair alone", got)
	}
}

// A `match` line reaches a movement no amount test can: the phases that
// ASSERT a pair run ahead of the currency partition and never consult a
// currency, so the holder can state a conversion's two legs as one movement
// even where the source stamped nothing on them.
func TestAForcedPairCrossesTheCurrencyPartition(t *testing.T) {
	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "usd-account", "out"}, Credit: LegRef{"bank", "chf-account", "in"},
	}})
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	legs := []TransferLeg{
		refLeg("usd-account", "out", 100, "USD", -1000, ""),
		refLeg("chf-account", "in", 104, "CHF", 987.65, ""),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, Overrides: forced})
	if len(got) != 1 || got[0].By != MatchedByOverride {
		t.Errorf("a stated cross-currency pair = %+v, want one pair asserted by the ledger", got)
	}
}

// The reference phase runs BEFORE the amount phase and withdraws what it
// claims, so a leg the amount phase would have taken on size and date alone
// goes to the leg the source named instead. That ordering is the point: an
// identity is better evidence than a coincidence of figures, and the
// coincidence is left one-legged, which is what it is.
func TestSharedReferenceIsSettledBeforeAmountsAre(t *testing.T) {
	legs := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -500, "TXN-10"),
		refLeg("savings", "in", 100, "CHF", 440, "TXN-10"),
		leg("bank", "brokerage", "exact-coincidence", 100, 500),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5})
	if len(got) != 1 || got[0].Credit.ID != "in" {
		t.Errorf("reference pair versus an exact same-day amount = %+v, want out→in", got)
	}
}

// Legs carrying no reference pair exactly as they did before the phase
// existed, and the phase's verdict does not depend on the order legs arrive
// in.
func TestSharedReferenceLeavesUnreferencedLegsAlone(t *testing.T) {
	plain := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "savings", "in", 101, 5000),
	}
	if got := MatchTransferLegs(append([]TransferLeg(nil), plain...),
		TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}); len(got) != 1 {
		t.Errorf("legs with no reference must match as they always did, got %+v", got)
	}

	mixed := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -500, "TXN-11"),
		refLeg("savings", "in", 100, "CHF", 440, "TXN-11"),
		leg("bank", "brokerage", "plain-out", 100, -75),
		leg("bank", "wallet", "plain-in", 100, 75),
	}
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true}
	want := pairIDs(MatchTransferLegs(append([]TransferLeg(nil), mixed...), opts))
	for i := len(mixed) - 1; i >= 0; i-- {
		shuffled := append([]TransferLeg(nil), mixed[i:]...)
		shuffled = append(shuffled, mixed[:i]...)
		if got := pairIDs(MatchTransferLegs(shuffled, opts)); !reflect.DeepEqual(got, want) {
			t.Errorf("rotation by %d changed the pairing: %v, want %v", i, got, want)
		}
	}
}

// pairIDs reduces a match to the set of debit→credit ids it asserted, so two
// runs can be compared without depending on the order the pairs came back in.
func pairIDs(pairs []TransferMatchPair) map[string]string {
	out := map[string]string{}
	for _, p := range pairs {
		out[p.Debit.ID] = p.Credit.ID
	}
	return out
}

// counterLeg is a matcher input that describes the other leg of its movement.
func counterLeg(owner, id string, day int64, ccy string, amt float64, statedCcy string, statedAmt float64) TransferLeg {
	l := leg("bank", owner, id, day, amt)
	l.Ccy, l.CounterCcy, l.CounterAmt = ccy, statedCcy, statedAmt
	return l
}

// A currency conversion between two own accounts pairs on the leg the bank
// described, which is the only link two legs of different currencies and
// different figures have when the source stamped no reference on either.
func TestAStatedCounterPairsAConversion(t *testing.T) {
	legs := []TransferLeg{
		counterLeg("gbp-account", "pays", 100, "GBP", -1234.50, "", 0),
		counterLeg("usd-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true})
	if len(got) != 1 || got[0].By != MatchedByStatedCounter ||
		got[0].Debit.ID != "pays" || got[0].Credit.ID != "gets" {
		t.Fatalf("stated-counter pair = %+v, want pays→gets", got)
	}
}

// A description is matched rather than merely read, so anything it cannot
// resolve pairs nothing. Unlike a reference it also keeps the day: a figure
// repeating in another month is a coincidence, not a movement.
func TestAStatedCounterRefusesWhatItCannotResolve(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 30, TolerancePct: 0.5, AllowSameOwner: true}
	cases := []struct {
		name string
		legs []TransferLeg
	}{
		{"two legs answer the description", []TransferLeg{
			counterLeg("usd-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
			counterLeg("gbp-account", "pays", 100, "GBP", -1234.50, "", 0),
			counterLeg("other-gbp", "also-pays", 100, "GBP", -1234.50, "", 0),
		}},
		{"two descriptions name one leg", []TransferLeg{
			counterLeg("usd-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
			counterLeg("other-usd", "also-gets", 100, "USD", 1587.75, "GBP", 1234.50),
			counterLeg("gbp-account", "pays", 100, "GBP", -1234.50, "", 0),
		}},
		{"the described leg is on the same account", []TransferLeg{
			counterLeg("one-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
			counterLeg("one-account", "pays", 100, "GBP", -1234.50, "", 0),
		}},
		{"the described leg is a day away", []TransferLeg{
			counterLeg("usd-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
			counterLeg("gbp-account", "pays", 101, "GBP", -1234.50, "", 0),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchTransferLegs(append([]TransferLeg(nil), tc.legs...), opts); len(got) != 0 {
				t.Errorf("paired %+v", got)
			}
		})
	}
}

// A leg the source places on ANOTHER account is not half of a same-account
// round trip, and the amount pass is told so. Without that, a conversion leg
// the description could not resolve falls through and takes the credit
// sitting on its own account — which the source has already contradicted, and
// which steals that credit from the transfer it really belongs to.
func TestADescribedLegDoesNotPairWithItsOwnAccount(t *testing.T) {
	// One relationship, one day: a transfer from `far` into `hub`, and then a
	// conversion out of `hub` into `eur`. Three legs answer "USD 135790", so
	// the description resolves none of them — and the hub's own two legs must
	// still not fuse into a round trip.
	legs := []TransferLeg{
		counterLeg("far", "transfer-out", 100, "USD", -135790, "", 0),
		counterLeg("hub", "transfer-in", 100, "USD", 135790, "", 0),
		counterLeg("hub", "converts-out", 100, "USD", -135790, "", 0),
		counterLeg("eur", "converts-in", 100, "EUR", 123456.78, "USD", 135790),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true})
	if len(got) != 1 {
		t.Fatalf("matched %d pair(s), want 1: %+v", len(got), got)
	}
	if got[0].Debit.ID != "transfer-out" || got[0].Credit.ID != "transfer-in" {
		t.Errorf("paired %s→%s, want transfer-out→transfer-in", got[0].Debit.ID, got[0].Credit.ID)
	}
	if got[0].Debit.Owner == got[0].Credit.Owner {
		t.Error("the hub's own two legs were fused into a round trip")
	}
}

// Two legs of one account pair only as a round trip: the same signature on
// both, or a reversal on either. Two unrelated movements that merely share an
// account and a size — a payment out, a transfer in — must not delete each
// other.
func TestSameAccountLegsPairOnlyAsARoundTrip(t *testing.T) {
	on := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, AllowSameOwner: true}
	pair := func(dSig, cSig string, rev bool) []TransferMatchPair {
		d := leg("bank", "checking", "out", 100, -500)
		c := leg("bank", "checking", "in", 101, 500)
		d.Signature, c.Signature, c.Reversal = dSig, cSig, rev
		return MatchTransferLegs([]TransferLeg{d, c}, on)
	}
	if got := pair("EXAMPLE PLAN CONTRIBUTION", "EXAMPLE BANK TRANSFER", false); len(got) != 0 {
		t.Errorf("unrelated same-account legs paired: %+v", got)
	}
	if got := pair("EXAMPLE TRANSFER", "EXAMPLE TRANSFER", false); len(got) != 1 {
		t.Errorf("a movement out and back under one signature did not pair: %+v", got)
	}
	if got := pair("EXAMPLE WIRE", "CANCELLED WIRE", true); len(got) != 1 {
		t.Errorf("an entry and its reversal did not pair: %+v", got)
	}
}

// namedLeg is a matcher input whose narrative names other accounts.
func namedLeg(group, owner, id string, day int64, amt float64, names ...string) TransferLeg {
	l := leg(group, owner, id, day, amt)
	l.Names = names
	return l
}

// A debit whose narrative names where the money went takes that credit ahead
// of an earlier, silent debit a day nearer to it, which is left to the credit
// that is really its own.
func TestANamedPairIsClaimedBeforeANearerSilentOne(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	got := MatchTransferLegs([]TransferLeg{
		leg("bank", "checking", "silent", 100, -1000),
		namedLeg("bank", "checking", "named", 102, -1000, "exchange"),
		leg("exchange", "wallet", "deposit", 100, 1000),
		leg("broker", "cash", "in", 103, 1000),
	}, opts)
	want := map[string]string{"named": "deposit", "silent": "in"}
	if len(got) != 2 {
		t.Fatalf("pairs = %+v, want two", got)
	}
	for _, p := range got {
		if want[p.Debit.ID] != p.Credit.ID {
			t.Errorf("%s paired with %s, want %s", p.Debit.ID, p.Credit.ID, want[p.Debit.ID])
		}
	}
	if got[0].By != MatchedByName || got[1].By != MatchedByAmount {
		t.Errorf("phases = %s, %s; want named, then amount", got[0].By, got[1].By)
	}
}

// A leg that names its other side pairs with nothing else, however well
// amount and day agree, and a name for a whole group admits any account in it
// while a name for one account admits that account alone.
func TestANamedLegPairsOnlyWithWhatItNames(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	if got := MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "out", 100, -1000, "exchange"),
		leg("broker", "cash", "in", 100, 1000),
	}, opts); len(got) != 0 {
		t.Errorf("a leg naming the exchange paired with the broker: %+v", got)
	}
	if got := MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "out", 100, -1000, NamedAccount("exchange", "wallet-a")),
		leg("exchange", "wallet-b", "in", 100, 1000),
	}, opts); len(got) != 0 {
		t.Errorf("a leg naming one wallet paired with another: %+v", got)
	}
	if got := MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "out", 100, -1000, "exchange"),
		leg("exchange", "wallet-b", "in", 100, 1000),
	}, opts); len(got) != 1 {
		t.Errorf("a leg naming the exchange did not pair with one of its wallets: %+v", got)
	}
}

// A run of identical named transfers between two accounts pairs every leg: a
// debit whose nearest credit is also the only one another debit can reach
// moves to its other candidate rather than stranding both.
func TestNamedPairsStrandNoLegANearerChoiceWouldStrand(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 3, TolerancePct: 0.5}
	got := MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "first", 105, -1000, "exchange"),
		namedLeg("bank", "checking", "second", 108, -1000, "exchange"),
		leg("exchange", "wallet", "early", 103, 1000),
		leg("exchange", "wallet", "late", 106, 1000),
	}, opts)
	pairs := map[string]string{}
	for _, p := range got {
		pairs[p.Debit.ID] = p.Credit.ID
	}
	if pairs["first"] != "early" || pairs["second"] != "late" {
		t.Errorf("pairs = %v, want first→early and second→late", pairs)
	}
}

// Where both narratives name each other, that pair is claimed before a credit
// that names only a group can be taken by a silent debit of that group.
func TestMutuallyNamedPairsOutrankOneSidedOnes(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	got := MatchTransferLegs([]TransferLeg{
		leg("bank", "savings", "internal", 100, -5000),
		namedLeg("bank", "checking", "to-broker", 101, -5000, "broker"),
		namedLeg("broker", "cash", "from-bank", 100, 5000, "bank"),
		leg("bank", "checking", "from-savings", 100, 5000),
	}, opts)
	pairs := map[string]string{}
	for _, p := range got {
		pairs[p.Debit.ID] = p.Credit.ID
	}
	if pairs["to-broker"] != "from-bank" || pairs["internal"] != "from-savings" {
		t.Errorf("pairs = %v, want to-broker→from-bank and internal→from-savings", pairs)
	}
}

// On an exact tie of amount and day, a leg of the debit's own source wins
// over another source's: moves inside one institution are the commoner kind.
func TestATieGoesToTheDebitsOwnSource(t *testing.T) {
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	// The other source sorts first, so a first-found tie-break would take it.
	got := MatchTransferLegs([]TransferLeg{
		leg("savings-bank", "account-a", "journal-out", 100, -1000),
		leg("brokerage", "cash", "deposit", 100, 1000),
		leg("savings-bank", "account-b", "journal-in", 100, 1000),
	}, opts)
	if len(got) != 1 || got[0].Credit.ID != "journal-in" {
		t.Errorf("pairs = %+v, want journal-out→journal-in", got)
	}
}
