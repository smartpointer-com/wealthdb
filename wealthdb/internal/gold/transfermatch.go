package gold

import (
	"math"
	"sort"
	"strconv"
)

// Transfer matching, the shared core.
//
// One money movement between two accounts is booked twice — a debit on the
// sending account, a credit on the receiving one — and nothing in the data
// links the halves — except where the source itself does, by stamping one
// reference on both. This file holds the algorithm that re-pairs them: a
// deterministic 1:1 matcher in five phases, asserting first what the holder
// stated, then what the source stamped and what it described, and only then
// guessing from same-currency amounts inside a day window and a tolerance —
// first among the pairs a narrative names, then among the rest. It
// knows nothing about returns, spending, or accounts; it takes legs and hands
// back pairs. Callers own what a leg IS, which legs are offered, and what a
// pair MEANS:
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

	// Ref is the reference the SOURCE stamped on BOTH halves of one movement
	// — a bank's own transaction number, written once and printed on the
	// debit and on the credit alike. Empty when the source stamped none,
	// which is the common case and the only case the returns engine has.
	//
	// It is a different KIND of evidence from everything else on this
	// struct. Amount, day and rail are a guess that two rows describe one
	// movement; a shared reference is the source SAYING so. That is why the
	// reference phase spends neither the tolerance nor the window (see
	// matchSharedReferences), and why it is the only road on which a
	// cross-currency movement can pair at all: an identity does not care what
	// the two legs were denominated in, and a conversion's two legs never
	// agree on an amount.
	//
	// The strength of the claim is exactly the strength of the reference's
	// uniqueness, so a caller must offer a value the source mints per
	// movement rather than per booking, per day or per batch. The phase
	// checks what it can — a reference carried by anything other than one
	// debit and one credit pairs nothing — but it cannot check a reference
	// space it is only shown two rows of, and the cost of a false identity
	// here is the cost of every false pair: a real spending line deleted,
	// not merely mislabelled.
	Ref string

	// CounterCcy and CounterAmt are the OTHER leg as the source DESCRIBES
	// it: the currency and the figure the movement became, or came from.
	// Both empty when the source describes none, which is the common case
	// and the only case the returns engine has.
	//
	// It is a weaker claim than Ref and a stronger one than an amount. A
	// reference NAMES the movement, so it needs no corroboration; a
	// description has to be matched against a leg that answers to it, and
	// two unrelated conversions of the same size on the same day would
	// answer equally well. So the phase that reads this
	// (matchStatedCounters) keeps the day constraint an identity does not
	// need, and refuses any description more than one leg answers to.
	//
	// What it buys is the movement whose two legs share nothing else. A
	// bank converting between two of one holder's accounts may stamp no
	// common reference on the pair — a statement reconstruction carries
	// none at all — and the two figures differ by the rate, so no tolerance
	// can bring them together. A narrative saying "this became CCY 1234.56"
	// is then the only link there is.
	CounterCcy string
	CounterAmt float64

	// Signature and Reversal decide whether two legs of the SAME account
	// may pair, under AllowSameOwner: only when their signatures agree — the
	// same movement booked out and back, like a transfer that bounced — or
	// when either leg is a Reversal, marked so by the words a bank uses for
	// undoing an entry. Signature is the caller's normalised narrative, and
	// the matcher only ever compares it for equality.
	//
	// Without this, one account's debit and credit of equal size inside the
	// window pair on amount alone, and on a busy account that is usually two
	// unrelated movements — a payment out and a transfer in — which the pair
	// then deletes together. A caller that fills neither leaves every
	// same-account pair open, which is where AllowSameOwner stood before.
	Signature string
	Reversal  bool

	// Names are the accounts this leg's own narrative names as the other
	// side of its movement — "EXAMPLE BANK", "EXAMPLE BROKERAGE", an exchange's
	// name — each a whole group (any of its accounts) or one account
	// (NamedAccount). Empty for most legs. A pair whose either leg names
	// the other's account is claimed ahead of the plain amount pass (see
	// MatchTransferLegs), so a debit whose narrative names where the money
	// went is not left stranded because an earlier, silent debit of the same
	// size took that credit for being a day nearer.
	Names []string
}

// NamedAccount is the TransferLeg.Names entry for one account of a group.
// A bare group name stands for every account in it.
func NamedAccount(group, owner string) string { return group + "\x00" + owner }

// names reports whether l's narrative names the account other sits on.
func (l TransferLeg) names(other TransferLeg) bool {
	for _, n := range l.Names {
		if n == other.Group || n == NamedAccount(other.Group, other.Owner) {
			return true
		}
	}
	return false
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
	// reading a pair as "not spending" wants that round trip out. The legs'
	// Signature and Reversal say which same-account pairs are such a round
	// trip. It is off by default because for the returns engine an
	// in-and-out on one account is two boundary flows, not one transfer, and
	// a same-account pair would net capital that really did leave and
	// return. Meaningless under CrossGroupOnly, which forbids the whole
	// group first. When it is on and a debit finds equally good partners on
	// its own account and on another, the other account's leg wins the tie
	// (see MatchTransferLegs).
	AllowSameOwner bool
}

// TransferMatchPair is one matched movement: the debit leg and the credit leg
// it funded, returned verbatim as the caller supplied them, and the phase that
// asserted them.
type TransferMatchPair struct {
	Debit, Credit TransferLeg
	By            TransferMatchPhase
}

// TransferMatchPhase names which of MatchTransferLegs' phases asserted a pair,
// and therefore how strong the claim behind it is.
//
// It is carried because an audit of the matcher cannot be read without it. A
// reader checking a pair has always asked one question — do these two legs
// agree in amount and currency? — and for a reference pair the answer is
// supposed to be NO. Unlabelled, a correct conversion is indistinguishable on
// the page from the matcher bug such a listing exists to catch.
type TransferMatchPhase string

const (
	// MatchedByOverride is the holder naming two rows as one movement.
	MatchedByOverride TransferMatchPhase = "override"
	// MatchedByReference is the source stamping one reference on both.
	MatchedByReference TransferMatchPhase = "reference"
	// MatchedByStatedCounter is the source describing one leg on the other.
	MatchedByStatedCounter TransferMatchPhase = "stated-counter"
	// MatchedByName is the banded pass over the pairs one leg's narrative
	// names the other's account for (TransferLeg.Names).
	MatchedByName TransferMatchPhase = "named"
	// MatchedByAmount is the greedy banded pass: everything the data says
	// when nothing has said it outright.
	MatchedByAmount TransferMatchPhase = "amount"
)

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

// referenceMatchMaxDays bounds how far apart the two legs of a REFERENCE pair
// may sit, in days. It is not a tolerance and not the amount pass's window: a
// reference is an identity, and an identity has no credibility to bound.
//
// What it bounds is the reference SPACE. A source that mints a reference per
// movement resolves its two legs within days, because that is how long a
// booking takes to settle; a reference that resolves a "pair" across a span
// no settlement takes has wrapped and been issued again, and the two rows
// under it are two movements that happen to share a string. The uniqueness
// test upstream catches a space that repeats often — a reference on three
// legs pairs nothing — and this catches the residue it cannot: a space that
// repeats RARELY, where exactly two rows collide, years apart.
//
// A quarter is far past any settlement lag a bank has, so the bound refuses
// nothing a bank books as one movement; it exists only to keep a collision's
// blast radius from spanning an archive.
const referenceMatchMaxDays = 90

// MatchTransferLegs pairs debit legs with the credit legs they funded.
//
// Pairing runs in five phases over one pool, each withdrawing the legs it
// claims before the next one looks. The order is the order of how much the
// evidence is worth:
//
//  1. FORCED pairs — the holder naming two rows as one movement
//     (transferoverride.go). Nothing outranks a person who was there.
//  2. REFERENCE pairs — two legs carrying the reference their source stamped
//     on both halves of one movement (matchSharedReferences). An identity
//     the source asserts, not an inference drawn from it.
//  3. STATED-COUNTER pairs — a leg whose narrative describes the other leg's
//     currency and figure, matched to the leg that answers to the
//     description (matchStatedCounters). Weaker than a reference, because a
//     description has to be matched rather than merely read, and guarded
//     accordingly.
//  4. NAMED pairs — the banded pass below, over only the pairs one leg's
//     narrative names the other's account for (TransferLeg.Names): those
//     both legs name first, then those one leg names. Amount and window
//     still have to agree; the name decides between candidates they cannot
//     tell apart, which is the busy week when several transfers of one round
//     size cross the same accounts.
//  5. AMOUNT pairs — the same banded pass over everything left, which is
//     everything the data says when nothing has said it outright.
//
// A banded pair must agree on native currency, sit within opts.WindowDays of
// each other, and differ in amount by no more than the tolerance. Matching on
// NATIVE amounts is deliberate: converted amounts drift with the FX of each
// leg's day, so the same movement would pair differently per output currency.
// A debit whose currency has no credits at all is skipped, which is why a
// cross-currency movement cannot pair ON AMOUNT — a property of this phase
// rather than of the matcher, and the reason the reference phase exists: an
// FX conversion between two of one holder's own accounts is a real movement
// whose two legs no tolerance can ever bring together.
//
// The result is deterministic. Legs are sorted by (day, group, owner, id) —
// the slice is sorted IN PLACE — and each debit in that order ranks the
// eligible credits by the smallest amount gap, then the nearest day, then a
// leg of its own source, then another source's, and — only reachable under
// AllowSameOwner — one on its own account last, earliest on ties. Ranking
// amount before day is what keeps an exact-amount partner from losing to a
// nearer-day coincidence, the main false-pair pressure at loose tolerances;
// ranking the own account last keeps a same-account coincidence (a payroll
// credit landing the day a transfer of the same size leaves) from stealing
// the partner that is really on the far side. The amount phase is greedy and
// one-to-one: a credit claimed by an earlier debit is out of the pool for
// every later one, and it never backtracks. The named phase does, but only
// for a debit left with no free candidate (banded.maximal).
//
// Returned pairs follow the phases, and within each banded phase the debit
// order. Legs that found no partner are simply absent — the caller keeps
// whatever meaning an unmatched leg has for it.
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
	// claimed marks a debit taken by one of the three phases that assert a
	// pair outright. All three share it, and the pool is compacted ONCE
	// afterwards, so the banded passes below see a debit slice holding only
	// what none of them spoke for.
	claimed := make([]bool, len(debits))
	var out []TransferMatchPair
	claim := func(di, ci int, by TransferMatchPhase) {
		used[ci], claimed[di] = true, true
		out = append(out, TransferMatchPair{Debit: debits[di], Credit: credits[ci], By: by})
	}
	matchForcedPairs(debits, credits, used, claimed, opts, claim)
	matchSharedReferences(debits, credits, used, claimed, opts, claim)
	namedByDescription := matchStatedCounters(debits, credits, used, claimed, opts, claim)
	if len(out) > 0 {
		kept := debits[:0]
		for i, d := range debits {
			if !claimed[i] {
				kept = append(kept, d)
			}
		}
		debits = kept
	}
	band := banded{credits: credits, used: used, parts: parts, opts: opts, namedByDescription: namedByDescription}
	mutual, debits := band.maximal(debits, namedByBoth)
	oneSided, debits := band.maximal(debits, namedByEither)
	amount := band.greedy(debits)
	out = append(out, mutual...)
	out = append(out, oneSided...)
	return append(out, amount...)
}

// namedByBoth and namedByEither select the pairs the two named sub-phases
// admit: each leg naming the other's account, then one leg naming the
// other's. The pair two narratives agree on is the stronger claim, and is
// claimed before a one-sided name can hand its credit to a silent debit.
func namedByBoth(d, c TransferLeg) bool   { return d.names(c) && c.names(d) }
func namedByEither(d, c TransferLeg) bool { return d.names(c) || c.names(d) }

// banded is the pool the banded phases share: the credits with their
// per-currency day index, the claims made so far, and the legs the
// stated-counter phase placed elsewhere.
type banded struct {
	credits []TransferLeg
	used    []bool
	parts   map[string]*ccyPart
	opts    TransferMatchOpts
	// namedByDescription holds the legs whose other half the source places
	// on another account, which therefore never pair on their own.
	namedByDescription map[string]bool
}

// ccyPart indexes one currency's credits in the global sorted order, so each
// debit scans only its currency's day band instead of every credit.
type ccyPart struct {
	idx  []int   // indices into credits, day-ascending
	days []int64 // credits[idx[k]].Day, for the band search
}

// maximal pairs the debits with the credits admit selects, as many as there
// are pairs to make: each debit takes its best free candidate, and only a
// debit left with none frees a credit an earlier debit holds, by moving that
// debit to another of its own (an augmenting path). So one debit's nearest choice
// never strands a later debit that had no other — the shape of a run of
// identical transfers between two accounts, where the nearest day is not the
// right partner as often as not. Returns the pairs in debit order and the
// debits left.
func (b banded) maximal(debits []TransferLeg, admit func(d, c TransferLeg) bool) ([]TransferMatchPair, []TransferLeg) {
	cands := make([][]int, len(debits))
	for di, d := range debits {
		cands[di] = b.candidates(d, admit)
	}
	holder := map[int]int{} // credit index → debit index holding it
	var augment func(di int, seen map[int]bool) bool
	augment = func(di int, seen map[int]bool) bool {
		// A free credit first, so a debit never displaces another it does
		// not have to: the pairs stay what the greedy order would make
		// wherever that strands no one.
		for _, ci := range cands[di] {
			if _, held := holder[ci]; !held && !seen[ci] {
				seen[ci] = true
				holder[ci] = di
				return true
			}
		}
		for _, ci := range cands[di] {
			if seen[ci] {
				continue
			}
			seen[ci] = true
			if augment(holder[ci], seen) {
				holder[ci] = di
				return true
			}
		}
		return false
	}
	for di := range debits {
		if len(cands[di]) > 0 {
			augment(di, map[int]bool{})
		}
	}
	credit := make(map[int]int, len(holder)) // debit index → credit index
	for ci, di := range holder {
		credit[di] = ci
	}
	var out []TransferMatchPair
	var left []TransferLeg
	for di, d := range debits {
		ci, ok := credit[di]
		if !ok {
			left = append(left, d)
			continue
		}
		b.used[ci] = true
		out = append(out, TransferMatchPair{Debit: d, Credit: b.credits[ci], By: MatchedByName})
	}
	return out, left
}

// greedy is the amount phase: each debit in order takes its best free credit
// and keeps it.
func (b banded) greedy(debits []TransferLeg) []TransferMatchPair {
	var out []TransferMatchPair
	for _, d := range debits {
		cands := b.candidates(d, nil)
		if len(cands) == 0 {
			continue
		}
		b.used[cands[0]] = true
		out = append(out, TransferMatchPair{Debit: d, Credit: b.credits[cands[0]], By: MatchedByAmount})
	}
	return out
}

// candidates are the free credits d may pair with in a banded phase —
// same currency, inside the window and the tolerance, pairable, and admitted
// by admit when one is given — best first: the smallest amount gap, then the
// nearest day, then an account of d's own source, then one of another source,
// and d's own account last.
func (b banded) candidates(d TransferLeg, admit func(d, c TransferLeg) bool) []int {
	cp := b.parts[d.Ccy]
	if cp == nil {
		return nil // no credit in this currency: nothing this debit can fund
	}
	type cand struct {
		i        int
		gap      float64
		dist     int64
		affinity int // 0 own source, 1 another source, 2 own account
	}
	var out []cand
	window := int64(b.opts.WindowDays)
	lo := sort.Search(len(cp.days), func(k int) bool { return cp.days[k] >= d.Day-window })
	for k := lo; k < len(cp.idx) && cp.days[k] <= d.Day+window; k++ {
		i := cp.idx[k]
		c := b.credits[i]
		if b.used[i] || !pairableLegs(d, c, b.opts) || (admit != nil && !admit(d, c)) {
			continue
		}
		same := c.Group == d.Group && c.Owner == d.Owner
		if same && (b.namedByDescription[LegRef{d.Group, d.Owner, d.ID}.key()] ||
			b.namedByDescription[LegRef{c.Group, c.Owner, c.ID}.key()]) {
			continue // the source places this leg's other half elsewhere
		}
		dist := c.Day - d.Day
		if dist < 0 {
			dist = -dist
		}
		eps := transferMatchMinEps
		if r := b.opts.TolerancePct / 100 * math.Max(math.Abs(d.Amt), c.Amt); r > eps {
			eps = r
		}
		if b.opts.ToleranceMaxAbs > 0 && eps > b.opts.ToleranceMaxAbs {
			eps = b.opts.ToleranceMaxAbs
		}
		gap := math.Abs(d.Amt + c.Amt)
		if gap > eps {
			continue
		}
		affinity := 1
		switch {
		case same:
			affinity = 2
		case c.Group == d.Group:
			affinity = 0
		}
		out = append(out, cand{i, gap, dist, affinity})
	}
	sort.SliceStable(out, func(x, y int) bool {
		a, b := out[x], out[y]
		if a.gap != b.gap {
			return a.gap < b.gap
		}
		if a.dist != b.dist {
			return a.dist < b.dist
		}
		return a.affinity < b.affinity
	})
	idx := make([]int, len(out))
	for k, c := range out {
		idx[k] = c.i
	}
	return idx
}

// matchForcedPairs asserts the holder's manual pairs and withdraws their legs
// from the pool, so a manually stated movement cannot lose either half to a
// nearer coincidence — which is the whole reason to state it. A forced pair
// whose legs are not both in the pool is silently absent: the pool is the
// caller's business, and a leg it never offered is not this file's to
// complain about.
func matchForcedPairs(debits, credits []TransferLeg, used, claimed []bool, opts TransferMatchOpts, claim func(di, ci int, by TransferMatchPhase)) {
	if len(opts.Overrides.forced) == 0 {
		return
	}
	debitAt := make(map[string]int, len(debits))
	for i, d := range debits {
		debitAt[LegRef{d.Group, d.Owner, d.ID}.key()] = i
	}
	creditAt := make(map[string]int, len(credits))
	for i, c := range credits {
		creditAt[LegRef{c.Group, c.Owner, c.ID}.key()] = i
	}
	for _, p := range opts.Overrides.forced {
		di, dok := debitAt[p.Debit.key()]
		ci, cok := creditAt[p.Credit.key()]
		if !dok || !cok || used[ci] || claimed[di] {
			continue
		}
		claim(di, ci, MatchedByOverride)
	}
}

// assertedPairHolds reports whether a pair one of the asserting phases has
// resolved may actually be claimed.
//
// The tests are the same for every phase above the amount pass, and they are
// why an assertion is not simply obeyed: a source can name a movement it did
// not make. The credit must still be free, the two legs must sit on different
// accounts — a leg is not its own counterparty — a zero credit funds nothing,
// and the holder's own ledger still outranks the source, so a blocked pair
// stays blocked however plainly the source asserts it.
//
// maxDays is the one test that varies, and it varies with the strength of the
// claim: a reference is an identity and survives a settlement lag, while a
// description is matched rather than read and is only ever offered against
// the same day's legs (maxDays 0).
func assertedPairHolds(d, c TransferLeg, creditUsed bool, maxDays int64, opts TransferMatchOpts) bool {
	if creditUsed || c.Owner == d.Owner || c.Amt == 0 {
		return false
	}
	if dist := c.Day - d.Day; dist > maxDays || dist < -maxDays {
		return false
	}
	return !opts.Overrides.blocks(d, c)
}

// matchSharedReferences pairs the legs a source stamped with one reference:
// two rows the bank itself says are the two halves of one movement
// (TransferLeg.Ref).
//
// It spends neither the amount tolerance nor the day window, and that is the
// point rather than a loosening. A window and a tolerance are how a GUESS is
// bounded — they decide how far apart two rows may sit before calling them
// one movement stops being credible. A reference is not a guess, so there is
// no credibility to bound: the two legs are one movement or the reference is
// wrong, and no distance between them changes which. What replaces the band
// is a uniqueness test, below, and the caller's obligation to offer a
// reference its source mints per movement.
//
// Spending no tolerance is also the only way the phase reaches the movements
// it exists for. An FX conversion between two of one holder's own accounts is
// booked as a debit in one currency and a credit in another, and the two
// figures differ by the rate; the amount pass partitions by native currency
// precisely so that a report's display currency cannot change what counts as
// spending, and therefore cannot see such a pair at all. Identity is
// currency-blind, so this phase can.
//
// Four conditions, and a reference that fails any of them pairs NOTHING
// rather than pairing its best guess:
//
//   - The two legs are in the SAME group. A reference is an identity only
//     within one source's id space; two banks can mint the same string, and a
//     cross-source pair on a bare reference would be a coincidence dressed as
//     a fact. Under CrossGroupOnly — which forbids same-group pairing
//     outright — this phase therefore does nothing at all, which is why the
//     returns engine is unaffected whether or not it ever fills Ref.
//   - The legs are on DIFFERENT accounts. One account's two rows under one
//     reference are a bank's bookkeeping — a charge booked beside the payment
//     it belongs to, a correction beside the entry it corrects — not money
//     crossing between accounts. A genuine round trip on one account is still
//     the amount pass's to find, under AllowSameOwner.
//   - The reference is carried by EXACTLY ONE DEBIT AND ONE CREDIT among the
//     legs offered. Anything else means the reference does not name one
//     movement in this pool: three legs under one reference cannot say which
//     two are the pair, and two legs in the same direction are not a movement
//     at all. Both refuse rather than guess, because the cost of guessing is
//     not a wrong label — a false pair withdraws both legs, and the spending
//     line the debit represented is simply gone.
//   - The credit's amount is not zero. A zero is not half of a movement, and
//     with no amount test at all this phase is the only one that has to say
//     so: the amount pass refuses it by construction, since a zero can close
//     no gap. Only the credit is tested because only a credit can be zero —
//     the sign split puts every non-negative leg on that side.
//   - The two legs sit within referenceMatchMaxDays of each other. This is not
//     the amount pass's window in a longer coat; it is a staleness test on the
//     reference SPACE. A source that mints references per movement resolves a
//     pair within days, because that is how long a booking takes to settle; a
//     "pair" resolving across a span no settlement takes is a source that has
//     run out of reference and started again, and the two rows under it are
//     two movements. The bound is deliberately far past any real lag, so it
//     refuses nothing a bank books as one movement and costs a decade-wide
//     collision its whole blast radius.
//
// Two things it deliberately does NOT ask. A pair the holder has unmatched is
// still refused (Overrides.blocks): a person saying these two rows are not
// one movement outranks the clerk's reference, which is the one claim about a
// pair that beats an identity. But the partner RAIL a leg demands is not
// consulted, unlike the amount pass. That demand exists because amount and
// date alone let a card's receipt pair with any debit of the right size, and
// the narrative is the only thing that refuses it; against a reference the
// source stamped on both rows, a narrative regex is the weaker witness, and
// enforcing it would refuse true pairs whose bank-side half names nothing.
//
// The pass is deterministic and order-independent: the census is taken over
// the whole pool before anything is claimed, and the claiming walks the
// debits in the caller-independent order MatchTransferLegs has already sorted
// them into.
func matchSharedReferences(debits, credits []TransferLeg, used, claimed []bool, opts TransferMatchOpts, claim func(di, ci int, by TransferMatchPhase)) {
	if opts.CrossGroupOnly {
		return
	}
	type refKey struct{ group, ref string }
	// One census entry per reference: how many legs carry it on each side,
	// and the last credit index seen. The counts are what the uniqueness test
	// reads, and the index is meaningful only once `credits` says there was
	// exactly one to record.
	type census struct{ debits, credits, ci int }
	// Taken over EVERY leg offered, including any a forced pair has already
	// claimed. A reference the forced phase took a leg from is one this phase
	// must not read as naming a movement too, and counting the claimed leg is
	// what makes the count say so.
	seen := map[refKey]*census{}
	at := func(l TransferLeg) *census {
		k := refKey{l.Group, l.Ref}
		e := seen[k]
		if e == nil {
			e = &census{}
			seen[k] = e
		}
		return e
	}
	for _, d := range debits {
		if d.Ref != "" {
			at(d).debits++
		}
	}
	for i, c := range credits {
		if c.Ref != "" {
			e := at(c)
			e.credits, e.ci = e.credits+1, i
		}
	}
	// Walking the debits rather than the map: map order is not an order, and
	// a debit reached here whose reference carries exactly one debit IS that
	// debit, so the pair is fully determined by the census.
	for di, d := range debits {
		if d.Ref == "" || claimed[di] {
			continue
		}
		e := seen[refKey{d.Group, d.Ref}]
		if e.debits != 1 || e.credits != 1 {
			continue
		}
		if !assertedPairHolds(d, credits[e.ci], used[e.ci], referenceMatchMaxDays, opts) {
			continue
		}
		claim(di, e.ci, MatchedByReference)
	}
}

// matchStatedCounters pairs a leg whose narrative DESCRIBES the other leg —
// its currency and its figure — with the leg that answers to the description.
//
// It exists for the movement that shares nothing else. A conversion between
// two of one holder's own accounts carries two different figures in two
// different currencies, so no tolerance reaches it; and where the source
// stamps no common reference on the pair — a statement reconstructed from a
// printed page carries none — the reference phase above cannot reach it
// either. What is left is the bank writing, on one of the two rows, what the
// other row holds.
//
// A DESCRIPTION IS NOT AN IDENTITY, and the guards are set to that. A
// reference names one movement and needs no corroboration; "CCY 1234.56" is a
// claim that has to be matched, and two unrelated conversions of that size
// would answer it equally. So this phase keeps constraints the reference
// phase drops:
//
//   - the two legs must fall on the SAME DAY. A conversion settles both
//     halves on one value date, and the day is most of what keeps a
//     description from reaching a coincidence in another month.
//   - the described leg must be the ONLY leg that answers, and the ONLY leg
//     described. Two legs of one currency and figure on one day cannot say
//     which was meant, and two narratives describing one leg cannot say which
//     movement it belongs to. Both refuse.
//   - same group, different accounts, and the holder's unmatch still binds —
//     the reference phase's reasons, unchanged.
//
// Either leg may be the one that describes. A bank writes the conversion on
// whichever side its statement had room for, so the phase reads a debit's
// description first and, failing that, asks whether any credit describes the
// debit. The result is the same pair either way.
func matchStatedCounters(debits, credits []TransferLeg, used, claimed []bool, opts TransferMatchOpts, claim func(di, ci int, by TransferMatchPhase)) map[string]bool {
	if opts.CrossGroupOnly {
		return nil
	}
	// A leg's own identity as a description would state it, and the
	// description it carries. Both are (group, day, currency, figure) — the
	// same shape, so one answers the other by equality.
	type legKey struct {
		group string
		day   int64
		ccy   string
		amt   string
	}
	key := func(group string, day int64, ccy string, amt float64) legKey {
		return legKey{group, day, ccy, strconv.FormatFloat(math.Abs(amt), 'f', 2, 64)}
	}
	ownKey := func(l TransferLeg) legKey { return key(l.Group, l.Day, l.Ccy, l.Amt) }
	statedKey := func(l TransferLeg) (legKey, bool) {
		if l.CounterCcy == "" || l.CounterAmt == 0 {
			return legKey{}, false
		}
		return key(l.Group, l.Day, l.CounterCcy, l.CounterAmt), true
	}

	// Two censuses over every leg offered, claimed ones included, for
	// matchSharedReferences' reason: what a description resolves to must be
	// a property of the data rather than of what an earlier phase removed.
	owners, described := map[legKey]int{}, map[legKey]int{}
	creditOwning, creditDescribing := map[legKey]int{}, map[legKey]int{}
	allLegs := make([]TransferLeg, 0, len(debits)+len(credits))
	allLegs = append(append(allLegs, debits...), credits...)
	for _, l := range allLegs {
		owners[ownKey(l)]++
		if k, ok := statedKey(l); ok {
			described[k]++
		}
	}
	for i, c := range credits {
		creditOwning[ownKey(c)] = i
		if k, ok := statedKey(c); ok {
			creditDescribing[k] = i
		}
	}
	// resolved reports the credit a key names, when exactly one leg answers
	// to the key and exactly one leg describes it.
	resolved := func(k legKey, at map[legKey]int) (int, bool) {
		if owners[k] != 1 || described[k] != 1 {
			return 0, false
		}
		ci, ok := at[k]
		return ci, ok
	}

	for di, d := range debits {
		if claimed[di] {
			continue
		}
		ci, ok := -1, false
		if k, has := statedKey(d); has {
			ci, ok = resolved(k, creditOwning) // the debit describes the credit
		}
		if !ok {
			ci, ok = resolved(ownKey(d), creditDescribing) // a credit describes the debit
		}
		if !ok || !assertedPairHolds(d, credits[ci], used[ci], 0, opts) {
			continue
		}
		claim(di, ci, MatchedByStatedCounter)
	}

	// What the descriptions NAMED, whether or not a pair came of it. A leg
	// some narrative places on another account, in another currency, is not
	// half of a same-account round trip, and the amount pass is told so
	// (pairableLegs). Without that, a leg this phase could not resolve falls
	// through to a pass that may hand it the credit sitting on its own
	// account — a pair the source has already contradicted, and one that
	// takes that credit away from the leg it really belongs to.
	// A leg that states a counter is named by its own description, so no
	// census lookup can add anything: `described` was built by walking
	// these same legs. The census is asked only about the other
	// direction — whether some OTHER leg named this one.
	named := map[string]bool{}
	for _, l := range allLegs {
		if _, stated := statedKey(l); stated || described[ownKey(l)] > 0 {
			named[LegRef{l.Group, l.Owner, l.ID}.key()] = true
		}
	}
	return named
}

// pairableLegs reports whether two legs may pair at all, before amount and day
// are considered: the rail each leg demands of its partner and the account it
// names (namesCompatible), then under
// CrossGroupOnly never two legs of the same group, and two legs of the same
// account only under AllowSameOwner and only as a round trip (sameOwnerRoundTrip).
func pairableLegs(d, c TransferLeg, opts TransferMatchOpts) bool {
	if opts.Overrides.blocks(d, c) {
		return false
	}
	if !railsCompatible(d, c) || !namesCompatible(d, c) {
		return false
	}
	if c.Group != d.Group {
		return true
	}
	if opts.CrossGroupOnly {
		return false
	}
	if c.Owner != d.Owner {
		return true
	}
	return opts.AllowSameOwner && sameOwnerRoundTrip(d, c)
}

// sameOwnerRoundTrip reports whether two legs of one account read as one
// movement out and back: the same signature on both, or a reversal on either.
func sameOwnerRoundTrip(d, c TransferLeg) bool {
	return d.Signature == c.Signature || d.Reversal || c.Reversal
}

// namesCompatible holds a leg that names its other side to that side, in both
// directions: a debit whose narrative says the money went to one account is
// not the funding half of a credit on another, however well amount and day
// agree. A leg that names nothing constrains nothing.
func namesCompatible(d, c TransferLeg) bool {
	return (len(d.Names) == 0 || d.names(c)) && (len(c.Names) == 0 || c.names(d))
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
