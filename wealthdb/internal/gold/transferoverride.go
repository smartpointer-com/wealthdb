package gold

import (
	"fmt"
	"sort"
)

// Manual overrides on transfer matching.
//
// The matcher decides from amount, day, the rail a leg demands and — where
// the source stamped one on both legs — a shared reference. That is everything
// the data says, and it is sometimes not enough: two unrelated rows of the
// same size land in the window and are fused, or the two halves of one real
// movement sit further apart than any window a person would dare set, because
// a bank posted its side of an ACH a week after the other side credited.
// Neither is a rule that can be tightened into existence — one is a
// coincidence, the other is a fact about a particular pair — so both need a
// place for a person to say what happened.
//
// The ledger binds EVERY phase, reference pairs included. An identity the bank
// asserted is the strongest evidence in the data, and it is still weaker than
// a person saying these two rows are not one movement: the clerk stamped a
// number, the holder was there.
//
// An override names legs the way the pins ledger names transactions: by what
// a person can read off a statement, never by gold's opaque
// transaction_external_id. Resolution from that natural key to leg identity
// belongs to the caller, which is the side that holds the legs; this file
// holds the identity type, the resolved sets, and the matcher's use of them.

// LegRef identifies one leg the way MatchTransferLegs does — the same three
// opaque strings TransferLeg carries.
type LegRef struct{ Group, Owner, ID string }

func (r LegRef) key() string { return r.Group + "\x00" + r.Owner + "\x00" + r.ID }

// TransferOverrides are the resolved manual decisions handed to the matcher.
// The zero value overrides nothing, so a caller that supports none passes it
// and matches exactly as it would without.
type TransferOverrides struct {
	// isolated legs never pair with anything: the holder says this row is not
	// half of a movement at all, whatever sits near it.
	isolated map[string]bool
	// forbidden holds unordered leg pairs that may not pair WITH EACH OTHER.
	// The narrower statement, and the commoner one: the row is a real
	// transfer, just not with THAT partner.
	forbidden map[string]bool
	// forced pairs are matched before the greedy pass and removed from it, so
	// a forced pair cannot lose its partner to a nearer coincidence.
	forced []ForcedPair
}

// ForcedPair is one manually asserted movement.
type ForcedPair struct{ Debit, Credit LegRef }

// newTransferOverrides builds the resolved set. A leg named on both sides of
// one forced pair, or forced and isolated at once, is a contradiction the
// caller should have caught; it is reported rather than silently resolved.
func newTransferOverrides(isolated []LegRef, forbidden [][2]LegRef, forced []ForcedPair) (TransferOverrides, error) {
	o := TransferOverrides{
		isolated:  map[string]bool{},
		forbidden: map[string]bool{},
		forced:    append([]ForcedPair(nil), forced...),
	}
	for _, l := range isolated {
		o.isolated[l.key()] = true
	}
	for _, p := range forbidden {
		o.forbidden[pairKey(p[0], p[1])] = true
	}
	for _, p := range forced {
		if p.Debit == p.Credit {
			return TransferOverrides{}, fmt.Errorf("transfer override: a leg forced to pair with itself: %+v", p.Debit)
		}
		if o.isolated[p.Debit.key()] || o.isolated[p.Credit.key()] {
			return TransferOverrides{}, fmt.Errorf("transfer override: leg is both forced and isolated: %+v", p)
		}
	}
	// Deterministic order, so a forced pair claiming a leg two pairs want
	// resolves the same way on every run.
	sort.SliceStable(o.forced, func(i, j int) bool {
		return pairKey(o.forced[i].Debit, o.forced[i].Credit) <
			pairKey(o.forced[j].Debit, o.forced[j].Credit)
	})
	return o, nil
}

// pairKey is order-independent, so an override written either way round
// forbids the same pairing.
func pairKey(a, b LegRef) string {
	ka, kb := a.key(), b.key()
	if ka > kb {
		ka, kb = kb, ka
	}
	return ka + "\x01" + kb
}

func (o TransferOverrides) blocks(d, c TransferLeg) bool {
	dr := LegRef{d.Group, d.Owner, d.ID}
	cr := LegRef{c.Group, c.Owner, c.ID}
	return o.isolated[dr.key()] || o.isolated[cr.key()] || o.forbidden[pairKey(dr, cr)]
}

// TransferOverrideSelector names one leg the way a person reads it off a
// statement — the same natural key the pins ledger uses, and for the same
// reason: gold's transaction_external_id is opaque and adapter-specific, and
// nobody can write one down. The key is deliberately not unique; where two
// identical rows share a day, an override describes both, and anyone needing
// to separate them has a distinction gold does not carry either.
type TransferOverrideSelector struct {
	Source   string
	Account  string  // gold account_external_id
	Day      int64   // UTC midnight of occurred_at, Unix seconds
	Amount   float64 // net_amount as gold stores it: canonical sign
	Currency string
}

// TransferOverrideRule is one line of the ledger before resolution.
//
// Verb "unmatch" with only A given isolates that leg: the holder says the row
// is not half of a movement at all. With both A and B given it forbids just
// that pairing, which is the narrower and commoner statement — the row IS a
// transfer, only not with that partner. Verb "match" asserts the pair
// outright, ahead of the amount, day and rail rules, for the movement whose
// halves those rules cannot reach.
type TransferOverrideRule struct {
	Verb string // "match" | "unmatch"
	A    TransferOverrideSelector
	B    *TransferOverrideSelector // nil only for an isolating unmatch
	Note string
}

// ResolveTransferOverrides turns natural-key rules into the leg identities the
// matcher works in, against the legs a caller actually offered.
//
// A rule that matches no leg is REPORTED, not dropped silently: an override is
// something a person wrote down about a row they believe exists, and a typo
// that quietly does nothing is worse than one that says so. The unresolved
// list is returned rather than raised, so a caller can warn and carry on
// rather than fail a whole pass over one stale line.
func ResolveTransferOverrides(rules []TransferOverrideRule, legs []TransferLeg) (TransferOverrides, []TransferOverrideRule, error) {
	find := func(s TransferOverrideSelector) []LegRef {
		var out []LegRef
		for _, l := range legs {
			if l.Group != s.Source || l.Owner != s.Account || l.Ccy != s.Currency {
				continue
			}
			if l.Day != EpochDay(s.Day) {
				continue
			}
			if diff := l.Amt - s.Amount; diff > transferMatchMinEps || diff < -transferMatchMinEps {
				continue
			}
			out = append(out, LegRef{l.Group, l.Owner, l.ID})
		}
		return out
	}
	var (
		isolated  []LegRef
		forbidden [][2]LegRef
		forced    []ForcedPair
		unmatched []TransferOverrideRule
	)
	for _, r := range rules {
		as := find(r.A)
		if len(as) == 0 {
			unmatched = append(unmatched, r)
			continue
		}
		if r.B == nil {
			if r.Verb != "unmatch" {
				return TransferOverrides{}, nil, fmt.Errorf(
					"transfer override: verb %q needs both legs", r.Verb)
			}
			isolated = append(isolated, as...)
			continue
		}
		bs := find(*r.B)
		if len(bs) == 0 {
			unmatched = append(unmatched, r)
			continue
		}
		for _, a := range as {
			for _, b := range bs {
				if a == b {
					continue
				}
				switch r.Verb {
				case "unmatch":
					forbidden = append(forbidden, [2]LegRef{a, b})
				case "match":
					// Orientation is read off the amounts, not off the file:
					// whichever selector was written first, the debit is the
					// negative leg.
					if r.A.Amount < 0 {
						forced = append(forced, ForcedPair{Debit: a, Credit: b})
					} else {
						forced = append(forced, ForcedPair{Debit: b, Credit: a})
					}
				default:
					return TransferOverrides{}, nil, fmt.Errorf(
						"transfer override: unknown verb %q", r.Verb)
				}
			}
		}
	}
	o, err := newTransferOverrides(isolated, forbidden, forced)
	return o, unmatched, err
}
