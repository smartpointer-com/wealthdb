package gold

import (
	"maps"
	"reflect"
	"testing"
)

// leg is a shorthand for a same-currency matcher input.
func leg(group, owner, id string, day int64, amt float64) TransferLeg {
	return TransferLeg{Group: group, Owner: owner, ID: id, Day: day, Ccy: "USD", Amt: amt}
}

// TestMatchTransferLegsWithinGroup: two accounts of the SAME group pair.
func TestMatchTransferLegsWithinGroup(t *testing.T) {
	t.Parallel()
	legs := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "savings", "in", 101, 5000),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5})
	if len(got) != 1 || got[0].Debit.ID != "out" || got[0].Credit.ID != "in" {
		t.Errorf("within-group pair = %+v, want out→in", got)
	}
}

// TestSameAccountLegsNeverPair is the matcher without the reversal phase:
// an out and a back of one size on one account, days apart and inside the
// window, stay two legs. A credit on the debit's own account is no
// candidate in any cross-account phase, so it neither wins a tie nor beats
// a far partner by being exact.
func TestSameAccountLegsNeverPair(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	roundTrip := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "back", 103, 5000),
	}
	if got := MatchTransferLegs(roundTrip, opts); len(got) != 0 {
		t.Errorf("an out and a back on one account paired: %+v", got)
	}
	// A narrative naming the leg's own account changes nothing.
	named := []TransferLeg{
		namedLeg("bank", "checking", "out", 100, -5000, NamedAccount("bank", "checking")),
		namedLeg("bank", "checking", "back", 103, 5000, NamedAccount("bank", "checking")),
	}
	if got := MatchTransferLegs(named, opts); len(got) != 0 {
		t.Errorf("legs naming their own account paired: %+v", got)
	}

	// The real receiving leg elsewhere is taken, though the credit on the
	// debit's own account is exact, same-day and in the window.
	closer := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "back", 100, 5000),
		leg("broker", "cash", "in", 100, 4990),
	}
	got := MatchTransferLegs(closer, opts)
	if len(got) != 1 || got[0].Credit.ID != "in" {
		t.Errorf("exact own-account credit versus approximate far one = %+v, want out→in", got)
	}

	// A debit on the credit's own account sorts first and takes nothing:
	// the credit stays free for the debit elsewhere that funded it.
	contest := []TransferLeg{
		leg("bank", "checking", "early", 99, -5000),
		leg("broker", "cash", "far", 100, -5000),
		leg("bank", "checking", "back", 100, 5000),
	}
	got = MatchTransferLegs(contest, opts)
	if len(got) != 1 || got[0].Debit.ID != "far" || got[0].Credit.ID != "back" {
		t.Errorf("a credit contested by its own account's debit = %+v, want far→back", got)
	}
}

// signedLeg is a matcher input with a narrative signature and a reversal
// mark.
func signedLeg(group, owner, id string, day int64, amt float64, sig string, reversal bool) TransferLeg {
	l := leg(group, owner, id, day, amt)
	l.Signature, l.Reversal = sig, reversal
	return l
}

// TestASameAccountReversalPairsLast is the spending caller's reversal
// phase. A leg no cross-account phase paired takes the opposite leg on its
// own account that reads as its reversal (one signature, or reversal
// wording on either), within the window and the tolerance, the nearest in
// day. A leg with a partner on another account keeps it, and the holder's
// `unmatch` keeps a leg out.
func TestASameAccountReversalPairsLast(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, SameAccountReversals: true}
	pairs := func(legs []TransferLeg, opts TransferMatchOpts) map[string]string {
		out := map[string]string{}
		for _, p := range MatchTransferLegs(legs, opts) {
			out[p.Debit.ID] = p.Credit.ID + "/" + string(p.By)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		legs []TransferLeg
		want map[string]string
	}{
		{"a wire out and its return", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "back", 104, 5000),
		}, map[string]string{"out": "back/reversal"}},
		{"the nearest return in day", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "late", 105, 5000),
			leg("bank", "checking", "back", 101, 5000),
		}, map[string]string{"out": "back/reversal"}},
		{"a partner on another account first", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "back", 100, 5000),
			leg("broker", "cash", "in", 100, 4990),
		}, map[string]string{"out": "in/amount"}},
		{"outside the tolerance", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "back", 102, 4900),
		}, map[string]string{}},
		{"a payment and an unrelated credit", []TransferLeg{
			signedLeg("bank", "checking", "out", 100, -5000, "EXAMPLE RENT", false),
			signedLeg("bank", "checking", "back", 101, 5000, "EXAMPLE PAYROLL", false),
		}, map[string]string{}},
		{"a credit worded as a reversal", []TransferLeg{
			signedLeg("bank", "checking", "out", 100, -5000, "OUTGOING WIRE", false),
			signedLeg("bank", "checking", "back", 103, 5000, "RETURNED WIRE", true),
		}, map[string]string{"out": "back/reversal"}},
		{"outside the window", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "checking", "back", 106, 5000),
		}, map[string]string{}},
		{"another account's credit is no reversal", []TransferLeg{
			leg("bank", "checking", "out", 100, -5000),
			leg("bank", "savings", "back", 102, 5000),
		}, map[string]string{"out": "back/amount"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pairs(tc.legs, opts); !maps.Equal(got, tc.want) {
				t.Errorf("pairs = %v, want %v", got, tc.want)
			}
		})
	}

	legs := func() []TransferLeg {
		return []TransferLeg{leg("bank", "checking", "out", 100, -5000), leg("bank", "checking", "back", 103, 5000)}
	}
	if got := pairs(legs(), TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}); len(got) != 0 {
		t.Errorf("without the phase, a reversal paired: %v", got)
	}
	isolated, err := newTransferOverrides([]LegRef{{"bank", "checking", "back"}}, nil, nil)
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	withUnmatch := opts
	withUnmatch.Overrides = isolated
	if got := pairs(legs(), withUnmatch); len(got) != 0 {
		t.Errorf("an unmatched leg paired as a reversal: %v", got)
	}
}

// A forced pair joins one account without the reversal phase: the holder
// stating that an out and its back on one account are one movement. The
// ledger is read ahead of every phase, so the rule that keeps the banded
// passes off one account does not reach it.
func TestAForcedPairCanJoinOneAccount(t *testing.T) {
	t.Parallel()
	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "checking", "out"}, Credit: LegRef{"bank", "checking", "back"},
	}})
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	legs := []TransferLeg{
		leg("bank", "checking", "out", 100, -5000),
		leg("bank", "checking", "back", 103, 5000),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, Overrides: forced})
	if len(got) != 1 || got[0].By != MatchedByOverride ||
		got[0].Debit.ID != "out" || got[0].Credit.ID != "back" {
		t.Errorf("a forced same-account pair = %+v, want out→back asserted by the ledger", got)
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
	t.Parallel()
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
				TransferMatchOpts{WindowDays: 5})
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
	t.Parallel()
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
	t.Parallel()
	legs := []TransferLeg{
		refLeg("usd-account", "out", 100, "USD", -1000, "TXN-1"),
		refLeg("chf-account", "in", 100, "CHF", 987.65, "TXN-1"),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	opts := TransferMatchOpts{}
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
// coincidence wearing the clothes of a fact.
func TestSharedReferenceNeverCrossesSources(t *testing.T) {
	t.Parallel()
	legs := []TransferLeg{
		refLeg("checking", "out", 100, "USD", -500, "TXN-7"),
		{Group: "other-bank", Owner: "acct", ID: "in", Day: 100, Ccy: "CHF", Amt: 440, Ref: "TXN-7"},
	}
	if got := MatchTransferLegs(append([]TransferLeg(nil), legs...), TransferMatchOpts{}); len(got) != 0 {
		t.Errorf("a bare reference must not pair across sources, got %+v", got)
	}
}

// The holder outranks the clerk. An `unmatch` says these two rows are not one
// movement whatever is stamped on them, and it is the one claim about a pair
// that beats an identity; a `match` is asserted first and takes its legs out
// of the pool, so the reference phase finds one half already spoken for and
// refuses the rest.
func TestManualOverridesOutrankASharedReference(t *testing.T) {
	t.Parallel()
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
// currency or the window, so the holder can state a conversion's two legs as
// one movement even where the source stamped nothing on them. The credit
// here sits outside WindowDays.
func TestAForcedPairCrossesTheCurrencyPartition(t *testing.T) {
	t.Parallel()
	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "usd-account", "out"}, Credit: LegRef{"bank", "chf-account", "in"},
	}})
	if err != nil {
		t.Fatalf("newTransferOverrides: %v", err)
	}
	legs := []TransferLeg{
		refLeg("usd-account", "out", 100, "USD", -1000, ""),
		refLeg("chf-account", "in", 120, "CHF", 987.65, ""),
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
	t.Parallel()
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
	t.Parallel()
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
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
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
	t.Parallel()
	legs := []TransferLeg{
		counterLeg("gbp-account", "pays", 100, "GBP", -1234.50, "", 0),
		counterLeg("usd-account", "gets", 100, "USD", 1587.75, "GBP", 1234.50),
	}
	got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5})
	if len(got) != 1 || got[0].By != MatchedByStatedCounter ||
		got[0].Debit.ID != "pays" || got[0].Credit.ID != "gets" {
		t.Fatalf("stated-counter pair = %+v, want pays→gets", got)
	}
}

// A description is matched rather than merely read, so anything it cannot
// resolve pairs nothing. Unlike a reference it also keeps the day: a figure
// repeating in another month is a coincidence, not a movement.
func TestAStatedCounterRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 30, TolerancePct: 0.5}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A named debit takes a free deposit dated on or after its own day over an
// earlier one, though the earlier one is as near or nearer, and the deposit
// it leaves goes back to the pool for a later phase.
func TestANamedPairPrefersADepositOnOrAfterItsWithdrawal(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	for _, tc := range []struct {
		name         string
		early, later int64
	}{
		{"a tie in days", 99, 101},
		{"the earlier nearer", 99, 102},
		{"the later on the same day", 98, 100},
	} {
		got := pairIDs(MatchTransferLegs([]TransferLeg{
			namedLeg("bank", "checking", "named", 100, -1000, "exchange"),
			leg("exchange", "wallet", "early", tc.early, 1000),
			leg("exchange", "wallet", "later", tc.later, 1000),
			leg("bank", "savings", "silent", tc.early, -1000),
		}, opts))
		want := map[string]string{"named": "later", "silent": "early"}
		if !maps.Equal(got, want) {
			t.Errorf("%s: pairs = %v, want %v", tc.name, got, want)
		}
	}
}

// The preference moves a debit only to a deposit no other debit holds, and
// never to one further off in amount: a pair without such an alternative
// stays as it was.
func TestTheDateOrderPreferenceMovesNoOtherPair(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	// The later deposit is the other debit's own.
	held := pairIDs(MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "first", 100, -1000, "exchange"),
		namedLeg("bank", "checking", "second", 101, -1000, "exchange"),
		leg("exchange", "wallet", "early", 98, 1000),
		leg("exchange", "wallet", "later", 102, 1000),
	}, opts))
	if want := map[string]string{"first": "early", "second": "later"}; !maps.Equal(held, want) {
		t.Errorf("held: pairs = %v, want %v", held, want)
	}
	// The later deposit is short by a fee, the earlier one exact.
	gap := pairIDs(MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "named", 100, -1000, "exchange"),
		leg("exchange", "wallet", "exact", 99, 1000),
		leg("exchange", "wallet", "short", 101, 998),
	}, opts))
	if want := map[string]string{"named": "exact"}; !maps.Equal(gap, want) {
		t.Errorf("gap: pairs = %v, want %v", gap, want)
	}
}

// A debit moves only to its best candidate dated on or after it, never past
// a held one to a farther one, and a move frees a deposit an earlier debit,
// already passed over, then takes: no two pairs cross.
func TestTheDateOrderPreferenceRepeatsUntilNoDebitMoves(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	got := pairIDs(MatchTransferLegs([]TransferLeg{
		namedLeg("bank", "checking", "first", 100, -1000, "exchange"),
		namedLeg("bank", "checking", "second", 103, -1000, "exchange"),
		leg("exchange", "wallet", "a", 99, 1000),
		leg("exchange", "wallet", "b", 101, 1000),
		leg("exchange", "wallet", "c", 105, 1000),
	}, opts))
	if want := map[string]string{"first": "b", "second": "c"}; !maps.Equal(got, want) {
		t.Errorf("pairs = %v, want %v", got, want)
	}
}

// Where both narratives name each other, that pair is claimed before a credit
// that names only a group can be taken by a silent debit of that group.
func TestMutuallyNamedPairsOutrankOneSidedOnes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A leg whose amount is zero moved nothing: it pairs in no phase, neither
// as the credit a small debit would take inside the cent's floor nor as
// half of a forced pair.
func TestAZeroAmountLegPairsInNoPhase(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, SameAccountReversals: true}
	if got := MatchTransferLegs([]TransferLeg{
		leg("bank", "checking", "small", 100, -0.005),
		leg("broker", "cash", "zero", 100, 0),
		namedLeg("bank", "savings", "named", 100, -0.004, "broker"),
		signedLeg("bank", "checking", "back", 101, 0, "", true),
	}, opts); len(got) != 0 {
		t.Errorf("pairs with a zero-amount leg: %+v", got)
	}
	forced, err := newTransferOverrides(nil, nil, []ForcedPair{{
		Debit: LegRef{"bank", "checking", "out"}, Credit: LegRef{"broker", "cash", "zero"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	opts.Overrides = forced
	if got := MatchTransferLegs([]TransferLeg{
		leg("bank", "checking", "out", 100, -500),
		leg("broker", "cash", "zero", 100, 0),
	}, opts); len(got) != 0 {
		t.Errorf("a forced pair with a zero-amount leg: %+v", got)
	}
}

// coinLeg is a matcher input that moves a coin, its amount a quantity of
// it, at usd dollars a coin (0 for no price that day).
func coinLeg(owner, id string, day int64, amt, usd float64) TransferLeg {
	return TransferLeg{Group: "wallets", Owner: owner, ID: id, Day: day, Ccy: "BTC", Amt: amt, Coin: true, USD: usd}
}

// TestCoinLegsPairByTheCoinRule pins the coin rule in the banded phases:
// within two days either way, at most 0.1% more received, a fee up to the
// larger of 0.1% and 100 USD's worth and under a quarter, 1% where the coin
// has no price. A cent and the 40-unit cap are no bound in a coin's unit.
func TestCoinLegsPairByTheCoinRule(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, ToleranceMaxAbs: DefaultTransferFeeCap}
	for _, c := range []struct {
		name    string
		out, in TransferLeg
		pairs   bool
	}{
		{"a small transfer short by a fee worth under 100 USD",
			coinLeg("a", "out", 100, -0.002, 30000), coinLeg("b", "in", 100, 0.0017, 30000), true},
		{"a fee worth over 100 USD",
			coinLeg("a", "out", 100, -0.1, 30000), coinLeg("b", "in", 100, 0.095, 30000), false},
		{"a token worth fractions of a cent short by a tenth",
			coinLeg("a", "out", 100, -1e6, 0.0001), coinLeg("b", "in", 100, 9e5, 0.0001), true},
		{"a token short by three tenths, worth under 100 USD",
			coinLeg("a", "out", 100, -1e6, 0.0001), coinLeg("b", "in", 100, 7e5, 0.0001), false},
		{"a 30% loss",
			coinLeg("a", "out", 100, -0.01, 30000), coinLeg("b", "in", 100, 0.007, 30000), false},
		{"a surplus within 0.1%",
			coinLeg("a", "out", 100, -1, 30000), coinLeg("b", "in", 100, 1.0005, 30000), true},
		{"a surplus over 0.1%",
			coinLeg("a", "out", 100, -1, 30000), coinLeg("b", "in", 100, 1.002, 30000), false},
		{"three days apart",
			coinLeg("a", "out", 100, -1, 30000), coinLeg("b", "in", 103, 1, 30000), false},
		{"two days apart, the receipt first",
			coinLeg("a", "out", 102, -1, 30000), coinLeg("b", "in", 100, 1, 30000), true},
		{"no price: a fee of half a percent",
			coinLeg("a", "out", 100, -1, 0), coinLeg("b", "in", 100, 0.995, 0), true},
		{"no price: a fee of 2%",
			coinLeg("a", "out", 100, -1, 0), coinLeg("b", "in", 100, 0.98, 0), false},
	} {
		got := MatchTransferLegs([]TransferLeg{c.out, c.in}, opts)
		if (len(got) == 1) != c.pairs {
			t.Errorf("%s: pairs %v, want a pair %t", c.name, got, c.pairs)
		}
	}
	// A currency that is money pairs as before: the window and the cent.
	cash := []TransferLeg{leg("bank", "a", "out", 100, -0.01), leg("bank", "b", "in", 103, 0.005)}
	if got := MatchTransferLegs(cash, opts); len(got) != 1 {
		t.Errorf("cash legs three days apart within a cent = %v, want one pair", got)
	}
	// The coin's days hold under a narrower window too.
	narrow := opts
	narrow.WindowDays = 0
	if got := MatchTransferLegs([]TransferLeg{coinLeg("a", "out", 100, -1, 30000), coinLeg("b", "in", 102, 1, 30000)}, narrow); len(got) != 1 {
		t.Errorf("coin legs two days apart at a window of 0 = %v, want one pair", got)
	}
}

// TestCoinDustNeverPairsCrosswise: a small leg sorts first and the larger
// leg's partner is within a cent of it, but a receipt of twice what it
// sent is no transfer of it. The larger leg takes its own partner, and the
// small one stays unpaired. Of two receipts of nearby sizes, each sender
// takes the one of the smallest shortfall.
func TestCoinDustNeverPairsCrosswise(t *testing.T) {
	t.Parallel()
	opts := TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5, ToleranceMaxAbs: DefaultTransferFeeCap}
	got := MatchTransferLegs([]TransferLeg{
		coinLeg("a1", "small", 100, -0.01, 30000),
		coinLeg("a2", "large", 100, -0.02, 30000),
		coinLeg("b", "in", 101, 0.0199, 30000),
	}, opts)
	if want := map[string]string{"large": "in"}; !reflect.DeepEqual(pairIDs(got), want) {
		t.Errorf("pairs = %v, want %v", pairIDs(got), want)
	}
	got = MatchTransferLegs([]TransferLeg{
		coinLeg("a1", "first", 100, -0.0110, 30000),
		coinLeg("a2", "second", 100, -0.0100, 30000),
		coinLeg("b", "in-second", 101, 0.0099, 30000),
		coinLeg("c", "in-first", 101, 0.0108, 30000),
	}, opts)
	if want := map[string]string{"first": "in-first", "second": "in-second"}; !reflect.DeepEqual(pairIDs(got), want) {
		t.Errorf("pairs = %v, want %v", pairIDs(got), want)
	}
}

// TestALedgerLineBindsCoinLegsAtAnyGap: a coin staked and returned weeks
// later with its reward pairs only as the ledger states it, and the
// ledger's selector names a coin's leg by the coin rule, not by a cent: a
// cent of a coin would also name the smaller leg beside it.
func TestALedgerLineBindsCoinLegsAtAnyGap(t *testing.T) {
	t.Parallel()
	legs := []TransferLeg{
		coinLeg("wallet", "stake", 100, -1.2, 30000),
		coinLeg("wallet", "small", 100, -1.195, 30000),
		coinLeg("staking", "returned", 121, 1.25, 30000),
	}
	opts := TransferMatchOpts{WindowDays: 5}
	if got := MatchTransferLegs(append([]TransferLeg(nil), legs...), opts); len(got) != 0 {
		t.Fatalf("fixture is wrong: three weeks and a surplus pair nothing unaided, got %v", got)
	}
	sel := func(owner string, day int64, amount float64) *TransferOverrideSelector {
		return &TransferOverrideSelector{Source: "wallets", Account: owner, Day: day * SecondsPerDay, Amount: amount, Currency: "BTC"}
	}
	o, unresolved, err := ResolveTransferOverrides([]TransferOverrideRule{
		{Verb: VerbMatch, A: *sel("wallet", 100, -1.2), B: sel("staking", 121, 1.25)},
	}, legs)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("resolve: %v, unresolved %v", err, unresolved)
	}
	opts.Overrides = o
	got := MatchTransferLegs(legs, opts)
	if want := map[string]string{"stake": "returned"}; !reflect.DeepEqual(pairIDs(got), want) || got[0].By != MatchedByOverride {
		t.Errorf("pairs = %v, want %v by the ledger", got, want)
	}
	// The selector names the leg a person reads off, rounded: the coin
	// rule's share, not the coin's sixth decimal.
	idx := indexLegs(legs)
	if got := idx.legsAt(*sel("wallet", 100, -1.2001)); len(got) != 1 || got[0].ID != "stake" {
		t.Errorf("a rounded amount names %v, want the stake alone", got)
	}
	if got := idx.legsAt(*sel("wallet", 100, 1.2)); len(got) != 0 {
		t.Errorf("an amount of the other sign names %v, want none", got)
	}
}

// TestALedgerLineNamesTheNearestCoinLeg: two withdrawals of one coin on one
// day lie inside each other's coin-rule bound, so a selector answers to
// both. It names the nearer alone: an exact amount binds its own leg, never
// its neighbour, for a match and for a one-legged unmatch alike.
func TestALedgerLineNamesTheNearestCoinLeg(t *testing.T) {
	t.Parallel()
	legs := []TransferLeg{
		coinLeg("wallet", "near", 100, -1.002, 30000),
		coinLeg("wallet", "exact", 100, -1.0, 30000),
		coinLeg("vault", "in", 101, 0.999, 30000),
	}
	sel := func(owner string, day int64, amount float64) *TransferOverrideSelector {
		return &TransferOverrideSelector{Source: "wallets", Account: owner, Day: day * SecondsPerDay, Amount: amount, Currency: "BTC"}
	}
	if !legs[0].answers(-1.0) {
		t.Fatal("fixture is wrong: the neighbour must answer to the exact amount too")
	}
	idx := indexLegs(legs)
	if got := idx.legsAt(*sel("wallet", 100, -1.0)); len(got) != 1 || got[0].ID != "exact" {
		t.Errorf("an exact amount names %v, want its own leg alone", got)
	}
	o, unresolved, err := ResolveTransferOverrides([]TransferOverrideRule{
		{Verb: VerbMatch, A: *sel("wallet", 100, -1.0), B: sel("vault", 101, 0.999)},
	}, legs)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("resolve: %v, unresolved %v", err, unresolved)
	}
	got := MatchTransferLegs(append([]TransferLeg(nil), legs...), TransferMatchOpts{WindowDays: 5, Overrides: o})
	if want := map[string]string{"exact": "in"}; !reflect.DeepEqual(pairIDs(got), want) || got[0].By != MatchedByOverride {
		t.Errorf("pairs = %v, want %v by the ledger", got, want)
	}
	o, _, err = ResolveTransferOverrides([]TransferOverrideRule{
		{Verb: VerbUnmatch, A: *sel("wallet", 100, -1.0)},
	}, legs)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got = MatchTransferLegs(append([]TransferLeg(nil), legs...), TransferMatchOpts{WindowDays: 5, Overrides: o})
	if want := map[string]string{"near": "in"}; !reflect.DeepEqual(pairIDs(got), want) {
		t.Errorf("pairs = %v, want %v: the unmatch isolates the exact leg alone", got, want)
	}
}

// TestACoinReversalPairsByTheCoinRule: the reversal phase pairs a coin sent
// and returned to its own wallet by the coin rule, as every banded phase
// does: within two days whatever the window, short by a fee it allows.
func TestACoinReversalPairsByTheCoinRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		window int
		back   TransferLeg
		paired bool
	}{
		{"short by a fee, two days on, past the window", 1, coinLeg("wallet", "back", 102, 0.999, 30000), true},
		{"three days on, inside the window", 5, coinLeg("wallet", "back", 103, 1.0, 30000), false},
		{"short by more than the fee bound", 5, coinLeg("wallet", "back", 101, 0.99, 30000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legs := []TransferLeg{coinLeg("wallet", "out", 100, -1.0, 30000), tc.back}
			got := MatchTransferLegs(legs, TransferMatchOpts{WindowDays: tc.window, TolerancePct: 0.5, SameAccountReversals: true})
			if paired := len(got) == 1 && got[0].By == MatchedByReversal; paired != tc.paired || len(got) > 1 {
				t.Errorf("pairs = %v, want a reversal %t", got, tc.paired)
			}
		})
	}
}
