package gold

import (
	"fmt"
	"math"
	"sort"
)

// Manual overrides on transfer matching.
//
// The matcher decides from amount, day, the rail a leg demands, a reference
// the source stamped on both legs, and a narrative on one leg stating the
// other's currency and figure. That is everything the data says, and it is
// sometimes not enough: two unrelated rows of the same size land in the
// window and are fused, or the two halves of one real movement sit further
// apart than any window a person would dare set, because a bank posted its
// side of an ACH a week after the other side credited.
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
// statement — the natural key the pins ledger uses, except that a match or
// unmatch line names a coin's dust by its quantity, and for the same reason: gold's transaction_external_id is opaque and adapter-specific, and
// nobody can write one down. The key is deliberately not unique; where two
// identical rows share a day, an override describes both, and anyone needing
// to separate them has a distinction gold does not carry either.
type TransferOverrideSelector struct {
	Source  string
	Account string // gold account_external_id
	Day     int64  // UTC midnight of occurred_at, Unix seconds
	// Amount: for match and unmatch, the matcher's amount (TransferLeg.Amt,
	// the pool's match_amount), within a cent, or for a coin's leg as the
	// coin rule reads it (TransferLeg.answers); for reimburse, net_amount
	// as gold stores it (the pins key).
	Amount   float64
	Currency string
}

// legDay is what a selector names a leg by beside its amount: the
// source, the account, the currency and the epoch day.
type legDay struct {
	group, owner, ccy string
	day               int64
}

// legIndex holds the legs by legDay, so a selector reads its own day's
// legs alone, not the whole pool.
type legIndex map[legDay][]TransferLeg

func indexLegs(legs []TransferLeg) legIndex {
	idx := legIndex{}
	for _, l := range legs {
		k := legDay{l.Group, l.Owner, l.Ccy, l.Day}
		idx[k] = append(idx[k], l)
	}
	return idx
}

// legsAt are the legs s names: those on its account, day and currency whose
// amount answers to s's (TransferLeg.answers).
func (idx legIndex) legsAt(s TransferOverrideSelector) []LegRef {
	var out []LegRef
	for _, l := range idx[legDay{s.Source, s.Account, s.Currency, EpochDay(s.Day)}] {
		if l.answers(s.Amount) {
			out = append(out, LegRef{l.Group, l.Owner, l.ID})
		}
	}
	return out
}

// answers says l is the leg a selector of amount names. A coin's leg
// answers where the two amounts have one sign and the smaller is what a
// transfer of the larger delivers (coinQuantities, at l's price): a cent
// is no bound in a coin's own unit, too wide for one coin and too narrow
// for another. Every other leg answers within a cent.
func (l TransferLeg) answers(amount float64) bool {
	if !l.Coin {
		diff := l.Amt - amount
		return diff <= TransferMatchMinEps && diff >= -TransferMatchMinEps
	}
	if l.Amt == amount {
		return true
	}
	if (l.Amt < 0) != (amount < 0) {
		return false
	}
	larger, smaller := math.Abs(l.Amt), math.Abs(amount)
	if smaller > larger {
		larger, smaller = smaller, larger
	}
	return coinQuantities(larger, smaller, l.USD)
}

// TransferOverrideRule is one line of the ledger before resolution.
//
// Verb "unmatch" with only A given isolates that leg: the holder says the row
// is not half of a movement at all. With both A and B given it forbids just
// that pairing, which is the narrower and commoner statement — the row IS a
// transfer, only not with that partner. Verb "match" asserts the pair
// outright, ahead of every other phase, for the movement whose halves none of
// them can reach. Verb "reimburse" names a repayment and its outflow; it pairs
// no transfer (transferoverridefile.go).
type TransferOverrideRule struct {
	Verb string // VerbMatch | VerbUnmatch | VerbReimburse
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
	var (
		isolated  []LegRef
		forbidden [][2]LegRef
		forced    []ForcedPair
		unmatched []TransferOverrideRule
		idx       legIndex
	)
	if len(rules) > 0 {
		idx = indexLegs(legs)
	}
	for _, r := range rules {
		if r.Verb == VerbReimburse {
			continue // pairs no transfer: the spending pass reads it
		}
		as := idx.legsAt(r.A)
		if len(as) == 0 {
			unmatched = append(unmatched, r)
			continue
		}
		if r.B == nil {
			if r.Verb != VerbUnmatch {
				return TransferOverrides{}, nil, fmt.Errorf(
					"transfer override: verb %q needs both legs", r.Verb)
			}
			isolated = append(isolated, as...)
			continue
		}
		bs := idx.legsAt(*r.B)
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
				case VerbUnmatch:
					forbidden = append(forbidden, [2]LegRef{a, b})
				case VerbMatch:
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

// IdleTransferMatches returns the `match` rules that name a leg on both
// sides but that no pair of the matcher answers to: no override pair joins
// a leg of the one side and a leg of the other. Such a line resolves, so
// ResolveTransferOverrides does not report it, and still does nothing. Its
// two legs read as one direction, or one of them is zero and pairs in no
// phase, or both sides name one leg, or another line claimed a leg first.
// It is reported for the reason an unresolved line is.
func IdleTransferMatches(rules []TransferOverrideRule, legs []TransferLeg, pairs []TransferMatchPair) []TransferOverrideRule {
	if len(rules) == 0 {
		return nil
	}
	idx := indexLegs(legs)
	forced := map[string]bool{}
	for _, p := range pairs {
		if p.By == MatchedByOverride {
			forced[pairKey(LegRef{p.Debit.Group, p.Debit.Owner, p.Debit.ID},
				LegRef{p.Credit.Group, p.Credit.Owner, p.Credit.ID})] = true
		}
	}
	var idle []TransferOverrideRule
	for _, r := range rules {
		if r.Verb != VerbMatch || r.B == nil {
			continue
		}
		as, bs := idx.legsAt(r.A), idx.legsAt(*r.B)
		if len(as) == 0 || len(bs) == 0 {
			continue // unresolved: ResolveTransferOverrides reports it
		}
		paired := false
		for _, a := range as {
			for _, b := range bs {
				paired = paired || forced[pairKey(a, b)]
			}
		}
		if !paired {
			idle = append(idle, r)
		}
	}
	return idle
}
