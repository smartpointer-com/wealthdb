package gold

import (
	"math"
	"sort"
)

// Transfer matching, the shared core.
//
// One money movement between two accounts is booked twice — a debit on the
// sending account, a credit on the receiving one — and nothing in the data
// links the halves. This file holds the algorithm that re-pairs them: a
// deterministic, banded, greedy 1:1 matcher over same-currency amounts within
// a day window and a relative tolerance. It knows nothing about returns,
// spending, or accounts; it takes legs and hands back pairs. Callers own what
// a leg IS, which legs are offered, and what a pair MEANS:
//
//   - the returns engine (matchCrossTransfers) offers only attached external
//     flows, restricts pairing to legs from DIFFERENT sources, and reads a
//     pair as potential internality — netted only where an entity window holds
//     both legs live;
//   - a spending caller offers the transfer-eligible kinds on every account —
//     the pool is narrowed upstream, by the spend_matcher_pool macro, so no
//     kind filter is passed here — permits same-source pairing (own-account
//     moves inside one bank) and same-account pairing (a round trip that nets
//     to zero), and reads a pair as "not spending".

// TransferLeg is one signed money movement offered to the matcher.
//
// Group and Owner name the leg's account in two levels so pairing can be
// scoped: two legs of the SAME account pair only under AllowSameOwner, and
// CrossGroupOnly can forbid pairing inside one group altogether. A leg never
// pairs with itself — the sign split puts every leg on exactly one side of
// the matching — whatever the knobs say. The returns engine maps
// Group→silver_source_id, Owner→account_external_id, ID→
// transaction_external_id; the matcher treats all three as opaque and returns
// them untouched on the pair.
type TransferLeg struct {
	Group string // pairing scope (returns: silver_source_id)
	Owner string // the account holding the leg, within Group
	ID    string // leg identity, unique within (Group, Owner)

	Day int64  // epoch day
	Ccy string // NATIVE currency of Amt — the partition key (see MatchTransferLegs)

	// Amt is signed: negative is a debit (the sending leg), zero or positive a
	// credit. Zero amounts pair with nothing useful and are best filtered by
	// the caller; they are treated as credits here.
	Amt float64

	// Rail names the payment rail the leg's own narrative announces, in
	// whatever vocabulary the caller uses; empty when the narrative announces
	// none, which is the common case. RailPartner is the rail this leg
	// DEMANDS of whatever it pairs with, and is what actually constrains the
	// matching (railsCompatible). A caller that fills neither — the returns
	// engine — matches exactly as it did before these existed.
	//
	// The two are separate because the demand is not symmetric. A card's
	// record of being paid is unmistakably a card receipt and can insist its
	// partner be a payment to a card; the bank-side record of paying that card
	// often carries only the issuer's name, or the cardholder's, and can
	// insist on nothing. Amount and date alone let a receipt pair with any
	// unrelated debit of the right size — a utility bill, a payment to a
	// person — which silently deletes real spending, and this is the signal
	// that refuses it.
	Rail        string
	RailPartner string
}

// TransferMatchOpts are the matcher's knobs. The zero value pairs only
// same-day legs that agree to the cent, allows pairing inside one group, and
// refuses pairing inside one account. WHICH legs are offered is the caller's
// business, not a knob: every leg handed in is a candidate.
type TransferMatchOpts struct {
	// WindowDays is the maximum |day distance| between the two legs.
	WindowDays int
	// TolerancePct is the permitted amount gap as a percent of the larger
	// leg, floored at transferMatchMinEps absolute — so 0 means
	// exact-to-a-cent, and the returns default of 0.5 covers a wire fee
	// deducted in transit.
	TolerancePct float64
	// ToleranceMaxAbs caps the tolerance in absolute currency units, whatever
	// TolerancePct works out to; zero leaves it uncapped. The percentage
	// exists to absorb a fee deducted in transit, and such a fee is FLAT — a
	// fixed charge per wire, not a share of the sum. Uncapped, the percentage
	// therefore grows into exactly the band where coincidences live: on a
	// large transfer half a percent is more than any fee, and a debit will
	// happily pair with a credit tens of units away that has nothing to do
	// with it.
	ToleranceMaxAbs float64
	// CrossGroupOnly forbids pairing two legs of the same group. The returns
	// engine sets it (a same-source pair is the silver classifier's business,
	// not the matcher's); a spending caller pairing own-account moves inside
	// one bank leaves it false.
	CrossGroupOnly bool
	// Overrides are the holder's manual decisions about particular legs and
	// particular pairs, applied ahead of and around the greedy pass
	// (transferoverride.go). The zero value overrides nothing.
	Overrides TransferOverrides
	// AllowSameOwner permits pairing two legs of the SAME account: a
	// withdrawal and a deposit that undo each other — a transfer bounced
	// back, a reversal booked as its own line — net to zero, and a caller
	// reading a pair as "not spending" wants that round trip out. It is off
	// by default because for the returns engine an in-and-out on one account
	// is two boundary flows, not one transfer, and a same-account pair would
	// net capital that really did leave and return. Meaningless under
	// CrossGroupOnly, which forbids the whole group first. When it is on and
	// a debit finds equally good partners on its own account and on another,
	// the other account's leg wins the tie (see MatchTransferLegs).
	AllowSameOwner bool
}

// TransferMatchPair is one matched movement: the debit leg and the credit leg
// it funded, returned verbatim as the caller supplied them.
type TransferMatchPair struct {
	Debit, Credit TransferLeg
}

// transferMatchMinEps is the absolute floor on the amount tolerance, in
// currency units: a cent of rounding slack, so TolerancePct=0 still matches
// legs that agree to the cent.
const transferMatchMinEps = 0.01

// DefaultTransferFeeCap bounds the amount tolerance in absolute currency
// units (TransferMatchOpts.ToleranceMaxAbs). It is sized to the largest
// per-transfer fee a bank plausibly deducts in transit — an international
// wire fee reaches the low tens, never hundreds — and is stated in
// major-currency units, which is what the matcher partitions on in practice.
//
// The number is calibrated against what a transit fee IS, not guessed: a
// rail deducts a FLAT charge in the low tens — a correspondent or domestic
// wire fee, an exchange's withdrawal fee — and never a share of the amount.
// The coincidences the cap exists to refuse scale with the transfer instead,
// so a proportional tolerance admits them as soon as the amount is large.
const DefaultTransferFeeCap = 40.0

// MatchTransferLegs pairs debit legs with the credit legs they funded.
//
// A pair must agree on native currency, sit within opts.WindowDays of each
// other, and differ in amount by no more than the tolerance. Matching on
// NATIVE amounts is deliberate: converted amounts drift with the FX of each
// leg's day, so the same movement would pair differently per output currency.
// A debit whose currency has no credits at all is skipped, which is why
// cross-currency movements never pair — a recorded limitation of this
// matcher, not something a caller can configure away.
//
// The result is deterministic. Legs are sorted by (day, group, owner, id) —
// the slice is sorted IN PLACE — and each debit in that order takes the
// eligible credit with the smallest amount gap, then the nearest day, then —
// only reachable under AllowSameOwner — a leg on ANOTHER account over one on
// the debit's own, earliest on ties. Ranking amount before day is what keeps
// an exact-amount partner from losing to a nearer-day coincidence, the main
// false-pair pressure at loose tolerances; ranking the other account before
// the debit's own keeps a same-account coincidence (a payroll credit landing
// the day a transfer of the same size leaves) from stealing the partner that
// is really on the far side. Pairing is greedy and one-to-one: a credit
// claimed by an earlier debit is out of the pool for every later one, and the
// matcher never backtracks to a globally better assignment.
//
// Returned pairs follow the debit order. Legs that found no partner are
// simply absent — the caller keeps whatever meaning an unmatched leg has for
// it.
func MatchTransferLegs(legs []TransferLeg, opts TransferMatchOpts) []TransferMatchPair {
	if len(legs) < 2 {
		return nil
	}
	sort.Slice(legs, func(i, j int) bool {
		a, b := legs[i], legs[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		return a.ID < b.ID
	})
	var debits, credits []TransferLeg
	for _, l := range legs {
		if l.Amt < 0 {
			debits = append(debits, l)
		} else {
			credits = append(credits, l)
		}
	}
	if len(debits) == 0 || len(credits) == 0 {
		return nil
	}

	// Per-currency credit index, preserving the global sorted order, so each
	// debit scans only its currency's day band instead of every credit.
	type ccyPart struct {
		idx  []int   // indices into credits, day-ascending
		days []int64 // credits[idx[k]].Day, for the band search
	}
	parts := map[string]*ccyPart{}
	for i, c := range credits {
		cp := parts[c.Ccy]
		if cp == nil {
			cp = &ccyPart{}
			parts[c.Ccy] = cp
		}
		cp.idx = append(cp.idx, i)
		cp.days = append(cp.days, c.Day)
	}

	used := make([]bool, len(credits))
	var out []TransferMatchPair
	// Forced pairs are asserted first and their legs withdrawn from the pool,
	// so a manually stated movement cannot lose either half to a nearer
	// coincidence — which is the whole reason to state it. A forced pair whose
	// legs are not both in the pool is silently absent: the pool is the
	// caller's business, and a leg it never offered is not this file's to
	// complain about.
	if len(opts.Overrides.forced) > 0 {
		debitAt := make(map[string]int, len(debits))
		for i, d := range debits {
			debitAt[LegRef{d.Group, d.Owner, d.ID}.key()] = i
		}
		creditAt := make(map[string]int, len(credits))
		for i, c := range credits {
			creditAt[LegRef{c.Group, c.Owner, c.ID}.key()] = i
		}
		forcedDebit := make([]bool, len(debits))
		for _, p := range opts.Overrides.forced {
			di, dok := debitAt[p.Debit.key()]
			ci, cok := creditAt[p.Credit.key()]
			if !dok || !cok || used[ci] || forcedDebit[di] {
				continue
			}
			used[ci] = true
			forcedDebit[di] = true
			out = append(out, TransferMatchPair{Debit: debits[di], Credit: credits[ci]})
		}
		if len(out) > 0 {
			kept := debits[:0]
			for i, d := range debits {
				if !forcedDebit[i] {
					kept = append(kept, d)
				}
			}
			debits = kept
		}
	}
	window := int64(opts.WindowDays)
	for _, d := range debits {
		cp := parts[d.Ccy]
		if cp == nil {
			continue // no credit in this currency: nothing this debit can fund
		}
		lo := sort.Search(len(cp.days), func(k int) bool { return cp.days[k] >= d.Day-window })
		best, bestGap, bestDist, bestSame := -1, 0.0, int64(0), false
		for k := lo; k < len(cp.idx) && cp.days[k] <= d.Day+window; k++ {
			i := cp.idx[k]
			c := credits[i]
			if used[i] || !pairableLegs(d, c, opts) {
				continue
			}
			same := c.Group == d.Group && c.Owner == d.Owner
			dist := c.Day - d.Day
			if dist < 0 {
				dist = -dist
			}
			eps := transferMatchMinEps
			if r := opts.TolerancePct / 100 * math.Max(math.Abs(d.Amt), c.Amt); r > eps {
				eps = r
			}
			if opts.ToleranceMaxAbs > 0 && eps > opts.ToleranceMaxAbs {
				eps = opts.ToleranceMaxAbs
			}
			gap := math.Abs(d.Amt + c.Amt)
			if gap > eps {
				continue
			}
			if best < 0 || gap < bestGap || (gap == bestGap && dist < bestDist) ||
				(gap == bestGap && dist == bestDist && bestSame && !same) {
				best, bestGap, bestDist, bestSame = i, gap, dist, same
			}
		}
		if best < 0 {
			continue
		}
		used[best] = true
		out = append(out, TransferMatchPair{Debit: d, Credit: credits[best]})
	}
	return out
}

// pairableLegs reports whether two legs may pair at all, before amount and day
// are considered: the rail each leg demands of its partner, then under
// CrossGroupOnly never two legs of the same group, and two legs of the same
// account only under AllowSameOwner.
func pairableLegs(d, c TransferLeg, opts TransferMatchOpts) bool {
	if opts.Overrides.blocks(d, c) {
		return false
	}
	if !railsCompatible(d, c) {
		return false
	}
	if c.Group != d.Group {
		return true
	}
	if opts.CrossGroupOnly {
		return false
	}
	return opts.AllowSameOwner || c.Owner != d.Owner
}

// railsCompatible enforces the partner rail a leg demands, in both
// directions. A leg that demands nothing (RailPartner empty) constrains
// nothing, so a caller that classifies no leg — the returns engine — pairs
// exactly as it did before this existed.
//
// The asymmetry is the point. A rail is often written on ONE side of a
// movement only: a card's record of being paid says "payment thank you", while
// the bank's record of paying it may say the issuer, or the cardholder's own
// name, or nothing at all. So the side that names the rail declares what its
// partner must be, and the silent side stays free to pair. Requiring BOTH
// sides to name the rail would refuse the true pair whose other half is a bare
// name — the common shape, not the exception.
func railsCompatible(d, c TransferLeg) bool {
	if d.RailPartner != "" && d.RailPartner != c.Rail {
		return false
	}
	if c.RailPartner != "" && c.RailPartner != d.Rail {
		return false
	}
	return true
}
