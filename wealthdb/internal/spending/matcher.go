package spending

import (
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The internal-transfer matcher: the spending caller over gold's shared
// transfer-matching core (gold.MatchTransferLegs). One own-account move is
// booked twice — a withdrawal on the funding account, a card payment or
// deposit on the receiving one — and left unpaired, the outgoing leg reads
// as spending.
//
// What this caller decides differently from the returns engine, each
// argued in docs/SPENDING.md §3 ("The matcher"):
//
//   - same-source pairing is allowed (CrossGroupOnly=false);
//   - a leg no cross-account phase paired pairs with its reversal on its
//     own account (SameAccountReversals, with TransferLeg.Signature and
//     Reversal from reversalRe);
//   - the pool spans every account in gold, and its kind filter is the
//     spend_matcher_pool macro's own;
//   - amounts and currencies are native, converted only in the converted
//     phase, for a pair a narrative names (gold.MatchedByConversion);
//   - legs carry the reference a source stamped on both halves
//     (`payload.$.bank_ref`, loadMovementReferences), the other leg a
//     source describes (`payload.$.counter_currency` / `$.counter_amount`),
//     the card rail (legRail) and the accounts a narrative names
//     (legNames, from spending.internal_transfer_matching.names).
//
// Every pair records the phase that asserted it (gold.TransferMatchPhase):
// a reference or described-counter pair legitimately disagrees in amount
// and currency, and an audit of the pairs cannot be read without knowing
// which road made them.

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

// convertedTolerancePct is the matcher's converted phase's amount gap, as
// a percent of the credit (gold.TransferMatchOpts.ConvertedTolerancePct):
// what a bank's conversion spread and a reference rate's distance from the
// bank's own fixing cost a conversion between two own accounts.
const convertedTolerancePct = 1.0

// matchTransferLegs is the shared core, behind a variable so a test can
// count its runs: the pass runs it once, and nothing else in this
// package runs it.
var matchTransferLegs = gold.MatchTransferLegs

// matchTransferPairs is the pass's call into the shared core, with the
// options this caller decides.
func matchTransferPairs(legs []gold.TransferLeg, windowDays int, tolerancePct float64, overrides gold.TransferOverrides, rates *gold.DailyRates) []gold.TransferMatchPair {
	return matchTransferLegs(legs, gold.TransferMatchOpts{
		WindowDays:            windowDays,
		TolerancePct:          tolerancePct,
		ToleranceMaxAbs:       gold.DefaultTransferFeeCap,
		CrossGroupOnly:        false,
		Overrides:             overrides,
		SameAccountReversals:  true,
		Rate:                  rates.Direct,
		ConvertedTolerancePct: convertedTolerancePct,
	})
}

// The evidence a transfer_links row carries (gold migration 0182). A
// pair carries its phase's name, but the override phase's, which is the
// ledger's word. A one-legged row carries the road that named its far
// side: the source's stated counter account, or a rule.
const (
	evidenceStatedAccount = "stated-account"
	evidenceRule          = "rule"
	// measureCash is a link's measure for a movement of money.
	measureCash = "cash"
)

// phaseEvidence is the evidence each matcher phase writes, and
// evidencePhase the reverse, for the audit surface's listing.
var (
	phaseEvidence = map[gold.TransferMatchPhase]string{
		gold.MatchedByOverride:      "ledger",
		gold.MatchedByReference:     string(gold.MatchedByReference),
		gold.MatchedByStatedCounter: string(gold.MatchedByStatedCounter),
		gold.MatchedByName:          string(gold.MatchedByName),
		gold.MatchedByAmount:        string(gold.MatchedByAmount),
		gold.MatchedByReversal:      string(gold.MatchedByReversal),
		gold.MatchedByConversion:    string(gold.MatchedByConversion),
	}
	evidencePhase = func() map[string]gold.TransferMatchPhase {
		out := make(map[string]gold.TransferMatchPhase, len(phaseEvidence))
		for phase, evidence := range phaseEvidence {
			out[evidence] = phase
		}
		return out
	}()
)

// matchedPartners flattens pairs to a map from each matched leg to the
// leg it was paired with. Membership answers "was this an own-account
// move?"; the value answers "to where?".
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

// The card-bill rail: a card's own receipt demands a card payment opposite
// it; the paying side demands nothing (gold.TransferLeg.Rail;
// docs/SPENDING.md §3).
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
// simply unclassified, and an unclassified leg constrains nothing and pairs on
// amount and day alone. Only the receipt side must be right.
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
// of legs, which then pair on amount and day alone.
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
