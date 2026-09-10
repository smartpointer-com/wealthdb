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
//   - same-ACCOUNT pairing is ALLOWED too (AllowSameOwner=true). A
//     withdrawal and a deposit of the same amount on the same account
//     within the window is a round trip — a transfer bounced back, a
//     reversal booked as its own line — and it nets to zero. Left
//     unpaired, the withdrawal half counts as spending. The returns
//     engine leaves this off because for it an in-and-out on one
//     account is two boundary flows, not one movement. The kind set
//     below is what keeps this safe: `purchase` and `refund` are not
//     in it, so a card purchase and its refund never reach the matcher
//     and cannot pair through this knob — a refund is the merchant's
//     money coming back, not the holder's money going round.
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
//
// KNOWN LIMITATION: a cross-currency own-transfer cannot match.
// MatchTransferLegs partitions candidates by native currency, because
// matching converted amounts would make the same movement pair
// differently per output currency — a report's display currency must
// not change what counts as spending. So a withdrawal in one currency
// funding a card in another stays one-legged and is left to the rule
// tier. This is a property of the shared core, not something a caller
// can configure away.

// ProvenanceMatcher tags an enrichment row placed by this tier.
const ProvenanceMatcher = "matcher"

// txKey identifies one transaction across the pass: the enrichment
// table's primary key.
type txKey struct {
	source string
	txID   string
}

// matchInternalTransfers pairs the legs of the matcher pool and
// returns the set of transactions that are therefore own-account
// moves. BOTH legs of a pair are marked: the outgoing leg because it
// is not spending, the incoming leg because a later report that widens
// the population must not suddenly start counting it as income.
//
// The result is deterministic — MatchTransferLegs sorts its input and
// resolves ties by amount gap then day distance — so two runs over the
// same gold produce the same set.
func matchInternalTransfers(legs []gold.TransferLeg, windowDays int, tolerancePct float64, overrides gold.TransferOverrides) map[txKey]bool {
	return matchedLegSet(matchTransferPairs(legs, windowDays, tolerancePct, overrides))
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

// matchedLegSet flattens pairs to the set of transactions they cover.
func matchedLegSet(pairs []gold.TransferMatchPair) map[txKey]bool {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[txKey]bool, len(pairs)*2)
	for _, p := range pairs {
		out[txKey{p.Debit.Group, p.Debit.ID}] = true
		out[txKey{p.Credit.Group, p.Credit.ID}] = true
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
