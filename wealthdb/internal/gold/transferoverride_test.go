package gold

import "testing"

// The three manual decisions, against a pool where the matcher would
// otherwise do the wrong thing on its own. Every value is synthetic.
func TestTransferOverridesSteerTheMatcher(t *testing.T) {
	// A debit with two same-amount credits nearby: the matcher takes the
	// nearest, which here is the coincidence rather than the real partner.
	pool := func() []TransferLeg {
		return []TransferLeg{
			leg("bank", "checking", "out", 100, -500),
			leg("bank", "checking", "coincidence", 100, 500),
			leg("broker", "acct", "real-partner", 103, 500),
		}
	}
	opts := TransferMatchOpts{WindowDays: 5, AllowSameOwner: true}

	got := MatchTransferLegs(pool(), opts)
	if len(got) != 1 || got[0].Credit.ID != "coincidence" {
		t.Fatalf("fixture is wrong: unaided the matcher should take the coincidence, got %v", got)
	}

	t.Run("forbidding one pairing frees the leg for the right one", func(t *testing.T) {
		o, err := newTransferOverrides(nil, [][2]LegRef{{
			{"bank", "checking", "out"}, {"bank", "checking", "coincidence"},
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts := opts
		opts.Overrides = o
		got := MatchTransferLegs(pool(), opts)
		if len(got) != 1 || got[0].Credit.ID != "real-partner" {
			t.Errorf("got %v, want the debit paired with real-partner", got)
		}
	})

	t.Run("isolating a leg keeps it out of every pair", func(t *testing.T) {
		o, err := newTransferOverrides([]LegRef{{"bank", "checking", "out"}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts := opts
		opts.Overrides = o
		if got := MatchTransferLegs(pool(), opts); len(got) != 0 {
			t.Errorf("got %v, want no pair at all", got)
		}
	})

	// The false-negative direction: two halves too far apart for any window a
	// person would set, asserted by hand.
	t.Run("a forced pair beats the window, the tolerance and the greedy order", func(t *testing.T) {
		far := []TransferLeg{
			leg("bank", "checking", "out", 100, -10000),
			leg("exchange", "wallet", "credited-first", 94, 10000),
		}
		if got := MatchTransferLegs(append([]TransferLeg(nil), far...), opts); len(got) != 0 {
			t.Fatalf("fixture is wrong: 6 days apart should not pair at a 5-day window, got %v", got)
		}
		o, err := newTransferOverrides(nil, nil, []ForcedPair{{
			Debit:  LegRef{"bank", "checking", "out"},
			Credit: LegRef{"exchange", "wallet", "credited-first"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		opts := opts
		opts.Overrides = o
		got := MatchTransferLegs(far, opts)
		if len(got) != 1 || got[0].Credit.ID != "credited-first" {
			t.Errorf("got %v, want the asserted pair", got)
		}
	})
}

// A rule nobody can match is the holder's typo or a row that has since gone.
// Reporting it is the whole point: an override that quietly does nothing is
// worse than one that says so.
func TestResolveTransferOverridesReportsWhatItCannotFind(t *testing.T) {
	legs := []TransferLeg{leg("bank", "checking", "out", 100, -500)}
	day := int64(100) * 86400
	rules := []TransferOverrideRule{
		{Verb: "unmatch", A: TransferOverrideSelector{
			Source: "bank", Account: "checking", Day: day, Amount: -500, Currency: "USD"}},
		{Verb: "unmatch", A: TransferOverrideSelector{
			Source: "bank", Account: "no-such-account", Day: day, Amount: -500, Currency: "USD"}},
	}
	o, unresolved, err := ResolveTransferOverrides(rules, legs)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].A.Account != "no-such-account" {
		t.Errorf("unresolved = %v, want exactly the rule naming a missing account", unresolved)
	}
	if !o.isolated[LegRef{"bank", "checking", "out"}.key()] {
		t.Error("the rule that DID resolve must still take effect")
	}
}

// A leg cannot be both asserted into a pair and declared not to be half of
// one; the contradiction is the holder's to resolve, not this file's.
func TestTransferOverridesRejectContradictions(t *testing.T) {
	l := LegRef{"bank", "checking", "out"}
	if _, err := newTransferOverrides([]LegRef{l}, nil,
		[]ForcedPair{{Debit: l, Credit: LegRef{"bank", "checking", "in"}}}); err == nil {
		t.Error("forced and isolated at once must be rejected")
	}
	if _, err := newTransferOverrides(nil, nil, []ForcedPair{{Debit: l, Credit: l}}); err == nil {
		t.Error("a leg forced to pair with itself must be rejected")
	}
}
