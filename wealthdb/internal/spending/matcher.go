package spending

import (
	"regexp"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// The internal-transfer matcher: the spending caller over gold's
// shared transfer-matching core.
//
// One own-account move is booked twice — a withdrawal on the funding
// account, a card payment or deposit on the receiving one — and
// nothing in the data links the halves. Left unlinked, the outgoing
// leg is indistinguishable from real spending, and a month whose card
// was paid off reads as if the money had been spent twice: once at the
// merchants, once again paying the card.
//
// The pairing itself lives in gold.MatchTransferLegs. What is decided
// HERE is what the returns engine decides differently:
//
//   - same-source pairing is ALLOWED (CrossGroupOnly=false). The
//     returns matcher forbids it because a same-source pair is the
//     silver classifier's business; for spending the commonest
//     own-account move of all — cash account to card, inside one bank
//     — is exactly a same-source pair.
//   - same-ACCOUNT pairing is ALLOWED too (AllowSameOwner=true), for
//     a round trip only: a withdrawal and a deposit of the same amount
//     on the same account within the window whose narratives say they
//     are one movement out and back — the same merchant signature on
//     both, or a reversal's wording on either (reversalRe). It nets to
//     zero; left unpaired, the withdrawal half counts as spending. Two
//     same-size movements that merely share an account — a payment out
//     and a transfer in on the same day — are NOT a round trip, and
//     paired they would delete each other. The returns engine leaves
//     same-account pairing off because for it an in-and-out on one
//     account is two boundary flows, not one movement. The kind set
//     below keeps this safe too: `purchase` and `refund` are not in it,
//     so a card purchase and its refund never reach the matcher — a
//     refund is the merchant's money coming back, not the holder's
//     money going round.
//   - the pool spans EVERY account in gold — every kind, in or out of
//     the spending scope (migration 0044). The spending BASE stays
//     the scoped ones; the matcher must not, because a leg it cannot see
//     is a pair it cannot form, and the receiving half of a funding
//     movement usually lands on an account no spending report charts.
//     An outgoing leg left one-legged is indistinguishable from
//     spending, so a narrow pool does not merely miss matches — it
//     counts own-account moves as money spent.
//   - the kind filter is the pool's own. The spend_matcher_pool macro's
//     WHERE clause is the only place the transfer-eligible kinds are
//     named, and migration 0044 carries the argument for the set: the
//     kinds that mean "cash moved into or out of an account" AND carry
//     a pinned canonical sign, so every leg can be oriented. Widening
//     it buys false pairs, and a false pair does not surface as a wrong
//     category — it deletes a real spending line.
//   - amounts and currencies are NATIVE, never converted.
//   - the REFERENCE a source stamps on both legs of one movement is
//     offered (TransferLeg.Ref), which the returns engine fills for
//     nothing. It comes from `payload.$.bank_ref` by way of
//     loadMovementReferences, and only where the source minted that
//     reference on exactly two rows.
//   - so is the OTHER LEG the source describes — its currency and its
//     figure (TransferLeg.CounterCcy / CounterAmt, from
//     `payload.$.counter_currency` and `$.counter_amount`). Weaker than
//     a reference, because a description has to be matched rather than
//     read, and guarded to match; it reaches the conversion whose two
//     legs share no reference at all.
//   - so are the accounts a leg's narrative NAMES as its other side
//     (TransferLeg.Names, from spending.internal_transfer_matching.names
//     by way of legNames): a leg that names one pairs with nothing else,
//     and named pairs are claimed before the plain amount pass.
//
// That last one is what reaches a cross-currency own-transfer, and it
// is worth stating why it can. MatchTransferLegs partitions candidates
// by native currency, because matching converted amounts would make
// the same movement pair differently per output currency — a report's
// display currency must not change what counts as spending. So no
// amount test can ever join a conversion's two legs: they carry
// different figures by definition. A shared reference is not an amount
// test. It is the source asserting that two rows are one movement, and
// an identity is currency-blind, so the pairing holds without
// converting anything.
//
// The bill for it is that such a pair's two legs DISAGREE — different
// currencies, different amounts, and the difference is the rate.
// Nothing downstream nets a pair (each leg carries the other's account
// and is drawn, or not drawn, on its own figure), so the disagreement
// costs nothing; but a reader auditing the matcher has always checked a
// pair by comparing its two lines, and for this road that check inverts.
// The pairs therefore say which phase asserted them
// (gold.TransferMatchPhase).
//
// WHAT IS STILL OUT OF REACH: a movement whose source neither stamps a
// per-movement reference on it — stamping none, or one the pool's other
// rows also carry — nor describes by counter currency and figure. Those
// stay one-legged and are left to the rule tier, exactly as every
// cross-currency movement was before.

// ProvenanceMatcher tags an enrichment row placed by this tier.
const ProvenanceMatcher = "matcher"

// txKey identifies one transaction across the pass: the enrichment
// table's primary key.
type txKey struct {
	source string
	txID   string
}

// CounterpartyName says which narratives name an account as the other side of
// a movement: a leg whose narrative matches Pattern names Source — or, with
// Account set, that one account of it (gold.TransferLeg.Names). The entries
// come from the spending.internal_transfer_matching.names block of
// wealthdb.cfg.
type CounterpartyName struct {
	Source  string
	Account string
	Pattern *regexp.Regexp
}

// legNames lists the accounts a leg's narrative names as its other side. A
// name for the leg's own source, or its own account, is its own institution
// speaking — "the capital call Carta recorded" on a Carta row — and names no
// other side, so it is left out.
func legNames(names []CounterpartyName, group, owner, counterparty, description string) []string {
	var out []string
	text := counterparty + " " + description
	for _, n := range names {
		if !n.Pattern.MatchString(text) {
			continue
		}
		switch {
		case n.Account == "" && n.Source != group:
			out = append(out, n.Source)
		case n.Account != "" && (n.Source != group || n.Account != owner):
			out = append(out, gold.NamedAccount(n.Source, n.Account))
		}
	}
	return out
}

// matchTransferPairs is the call into the shared core, in one place so
// the pass (which wants the flattened leg set) and the audit surface
// (which wants the pairs themselves) cannot drift on the options.
func matchTransferPairs(legs []gold.TransferLeg, windowDays int, tolerancePct float64, overrides gold.TransferOverrides) []gold.TransferMatchPair {
	return gold.MatchTransferLegs(legs, gold.TransferMatchOpts{
		WindowDays:      windowDays,
		TolerancePct:    tolerancePct,
		ToleranceMaxAbs: gold.DefaultTransferFeeCap,
		CrossGroupOnly:  false,
		AllowSameOwner:  true,
		Overrides:       overrides,
	})
}

// countPairsBy counts the pairs one phase asserted. What it is for is the
// load summary: a road whose traffic nobody can see is a road nobody can tell
// has stopped carrying any.
func countPairsBy(pairs []gold.TransferMatchPair, by gold.TransferMatchPhase) int {
	n := 0
	for _, p := range pairs {
		if p.By == by {
			n++
		}
	}
	return n
}

// matchedPartners flattens pairs to a map from each matched leg to the
// leg it was paired with. Membership answers "was this an own-account
// move?" exactly as the flattened set did; the value answers "to
// where?".
//
// A leg reaches this map once. MatchTransferLegs pairs each leg with
// at most one partner, so there is no case where a second pair would
// overwrite a first and make the answer depend on iteration order.
func matchedPartners(pairs []gold.TransferMatchPair) map[txKey]gold.TransferLeg {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[txKey]gold.TransferLeg, len(pairs)*2)
	for _, p := range pairs {
		out[txKey{p.Debit.Group, p.Debit.ID}] = p.Credit
		out[txKey{p.Credit.Group, p.Credit.ID}] = p.Debit
	}
	return out
}

// The card-bill rail.
//
// A card statement records being paid as a receipt — "payment thank you",
// "payment received thank you" — and that line is unmistakable: nothing but a
// payment TO that card produces it. The bank-side half of the same movement is
// not unmistakable at all. It may name the issuer ("american express ach pmt",
// "chase credit crd epay"), but it may equally carry only the cardholder's own
// name, or nothing.
//
// So the receipt is the side that constrains: it demands a card payment
// opposite it, and the paying side demands nothing. Without that demand the
// receipt pairs on amount and date alone, and any debit of about the right
// size within the window will do — a utility bill, a cheque, a payment to a
// person. The pair then removes BOTH legs from spending, so the false half is
// real spending that silently disappears.
const (
	railCardPayment = "card_payment"
	railCardReceipt = "card_receipt"
)

// cardReceiptRe matches a card's own record of being paid. Anchored on "thank
// you", which the issuers' receipt lines share and which no bank-side debit
// carries.
var cardReceiptRe = regexp.MustCompile(`(?i)payment\s+(received\s+)?thank\s+you`)

// cardPaymentRe matches a bank-side payment to a card, by the issuer wording
// that names one. It is deliberately not exhaustive: a payment this misses is
// simply unclassified, and an unclassified leg constrains nothing and pairs as
// it always did. Only the receipt side must be right.
var cardPaymentRe = regexp.MustCompile(`(?i)\b(` +
	`payment\s+to\s+\w+\s+card\b` +
	`|credit\s+crd\s+(autopay|epay)` +
	`|card\s+online\s+payment` +
	`|american\s+express\s+ach\s+pmt` +
	`)`)

// reversalRe matches the wording a bank uses for undoing an entry of its own
// — a refunded fee, a cancelled wire or booking, a reversal, a returned item —
// in the languages the collected banks write in. It lets a leg pair with an
// entry of its own account whose narrative is unlike its own (see
// gold.TransferLeg.Reversal).
var reversalRe = regexp.MustCompile(`(?i)\b(refund|reversal|reversed|cancel+ed|canc\b|storno|returned|r(ü|ue|u)ckbuchung)`)

// legRail classifies a matcher leg's narrative as a payment rail and says what
// rail, if any, its partner must carry. Both are empty for the vast majority
// of legs, which therefore pair exactly as they did before.
func legRail(counterparty, description string) (rail, partner string) {
	both := counterparty + " " + description
	if cardReceiptRe.MatchString(both) {
		return railCardReceipt, railCardPayment
	}
	if cardPaymentRe.MatchString(both) {
		return railCardPayment, ""
	}
	return "", ""
}
