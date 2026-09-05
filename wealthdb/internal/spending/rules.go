package spending

import (
	"regexp"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The built-in rule tier.
//
// These are ENGINE CONSTANTS, not user configuration. Each one exists
// because a whole class of rows is structurally mis-read without it,
// and the right answer follows from what the product already knows
// about its own accounts rather than from anyone's preference. A
// per-deployment override list would let a wrong local rule quietly
// break the arithmetic these three protect; a merchant that needs
// special handling belongs in the merchant store, where a verdict is
// per-merchant and visible.
//
// Rules run against three texts, each on its own: the merchant
// SIGNATURE — the same Normalize output the merchant store is keyed
// by — and the two raw narrative fields it was built from, the
// counterparty and the description. All three go through the same
// fold, so a rule sees upper-case, punctuation-free tokens and does
// not have to restate the narrative's spelling variants. The raw
// fields are read because the signature is not always where the
// creditor is: an adapter's counterparty can be a promotion that holds
// only the bank's notice or the bank's own name while the creditor
// sits in the description, and a description can run past the
// signature's length cap. Each field is tested whole and separately,
// so a phrase never straddles the join between two of them.
//
// The description is read up to its memo separator only
// (canonical.SplitDescriptionMemo). What follows it is the payer's
// own words, not the bank's narrative, and a built-in that matched on
// them would place a delta from the payer's vocabulary — "mortgage"
// typed on a wire to a lender the product does not track, "bancomat"
// on a payment to a shop — with this tier's provenance and, for
// internal_transfer, by deleting the row from the base, which is the
// failure that is invisible in a report. A config rule reads the memo
// (ConfigRuleCategory); the built-ins never do.

// ProvenanceRule tags an enrichment row placed by this tier.
const ProvenanceRule = "rule"

// spendRule is one built-in rule: a category, plus the token, phrase
// and shape patterns that trigger it. Tokens match a whole word,
// phrases match a contiguous run of whole words, shapes match one
// whole word by pattern; a rule fires on ANY of its patterns.
type spendRule struct {
	detailed string
	tokens   []string
	phrases  []string
	shapes   []*regexp.Regexp
	// refusedBy vetoes the rule for a row any of its fields match. It
	// is how one rule declines a shape a later rule owns, where the
	// two patterns genuinely overlap on the same row and rule order
	// alone would give it to the wrong one.
	refusedBy *spendRule
}

// cardIssuerDescriptors are the phrases a deposit-account export puts
// on a credit-card bill, one entry per known statement format. The
// digits are masked in the comments and never reach a signature
// anyway — a trailing card number is a reference number to Normalize —
// and an issuer's name is not personal data, so the names stay.
//
// Each is matched as a whole-word phrase ANYWHERE in the signature or
// in either raw narrative field, which is what makes the rule
// indifferent to what precedes the issuer. A Swiss direct debit leads with the mandate notice —
// `DIRECT DEBIT; <CODE> OBJECTION TO <BANK>; WITHIN 30 DAYS;` — which
// Normalize strips (SignatureVersion 2); the rule matches with or
// without that strip, because it never anchors to the head.
//
// ISSUERS ONLY. A store card that names its merchant — a fuel card, a
// retailer's own card — is deliberately NOT here and must not be
// added: its bill is that merchant's spend and belongs in that
// merchant's category, which the model or a config rule places. A
// retailer in this table would re-file its purchases as generic card
// spend, which is precisely the information the placeholder exists to
// stop losing.
var cardIssuerDescriptors = []string{
	// PAYMENT TO CHASE CARD ENDING IN ####
	// CHASE CREDIT CRD AUTOPAY
	"PAYMENT TO CHASE CARD", "CHASE CREDIT CRD",
	// AMERICAN EXPRESS ACH PMT    M### / A###
	// AMERICAN EXPRESS CREDIT CARD
	"AMERICAN EXPRESS ACH PMT", "AMERICAN EXPRESS CREDIT CARD",
	// CITI CARD ONLINE PAYMENT    ####
	// CITI AUTOPAY     PAYMENT    ####
	// CITI CREDIT CARD PAYMENT
	"CITI CARD ONLINE PAYMENT", "CITI AUTOPAY", "CITI CREDIT CARD PAYMENT",
	// UBS SWITZERLAND AG;C/O UBS CARD CENTER
	// UBS AG;C/O UBS CARD CENTER AG …
	// …; UBS CARD CENTER; CREDIT CARD STATEMENT …   (direct debit)
	// …; UBS CARD CENTER; CARD PAYMENT …            (direct debit)
	"UBS CARD CENTER",
	// XXXX XXXX XXXX ####: a masked card number as the whole
	// counterparty is a card top-up. The masking that keeps some of
	// the digits is a shape, not a phrase — see cardIssuerShapes.
	"XXXX XXXX XXXX",
}

// cardIssuerShapes are the masked card numbers a phrase cannot spell:
// where the export keeps some of the digits, every card is a different
// string and only the SHAPE is shared. Each is matched against a
// single whole token, anywhere in the signature, and places the card
// rule's verdict exactly as a descriptor phrase does. The same
// issuers-only boundary applies.
var cardIssuerShapes = []*regexp.Regexp{
	// ####XXXXXXXX####, often followed by a date fragment: the first
	// four and last four digits kept, the middle eight masked, as one
	// token. The mixed token survives Normalize's reference-number
	// strip, which is what lets the rule see it at all. Exactly 4-8-4,
	// so a bare 16-digit number — a reference number, dropped by
	// Normalize besides — is not this shape. tokenize upper-cases
	// before a rule sees a token, so the X's match in either case.
	regexp.MustCompile(`^[0-9]{4}X{8}[0-9]{4}$`),
}

// cashWithdrawalRule places cash taken from a machine.
//
// Cash leaving an ATM is spend — the money is gone from the tracked
// accounts — but WHAT it was spent on is unobservable: no narrative,
// no merchant, no later record. `cash_withdrawal` is its own primary
// rather than a guess, so a report can show how much of a period's
// spending is simply unattributable instead of hiding it inside a
// plausible-looking category.
//
// It is declared apart from the table because the card-payment rule
// refuses every row it matches (refusedBy), so the two share one set
// of patterns rather than restating them.
var cashWithdrawalRule = spendRule{
	detailed: canonical.SpendDetailedCashWithdrawal,
	tokens:   []string{"ATM", "BANCOMAT", "GELDAUTOMAT", "BARGELDBEZUG", "CASHPOINT"},
	phrases: []string{
		"CASH WITHDRAWAL", "CASH ADVANCE", "WITHDRAWAL AT",
		"CASH DISBURSEMENT",
	},
}

// builtinRules are evaluated in order, first match wins, so the order
// is part of the contract. Card payments lead because their narratives
// are the most specific; a row matching two rules is a row whose
// narrative already named the more specific thing.
//
// Each rule must keep a DISTINCT detailed value. RuleCategory returns
// the category alone, so the tests identify which rule fired by the
// value it assigns; two rules sharing one would quietly weaken those
// tests to a category assertion. If a second rule ever has to assign
// an existing value, give the tests an unexported matchRule over the
// same inputs, returning (spendRule, bool), that RuleCategory wraps —
// rather than putting the rule's name back on the exported signature:
// provenance in gold is the tier, not the rule.
var builtinRules = []spendRule{
	{
		// A card payment leaving a cash account is one of two things,
		// and which one is decided ABOVE this rule, by the matcher.
		//
		// When the card is collected, the bill pairs with the card's
		// own `card_payment` leg and the matcher marks both legs
		// `internal_transfer`: the purchases are itemised on the card,
		// so the bill is an own-account move. The matcher outranks this
		// rule, so that verdict stands whatever the narrative says.
		//
		// When it is NOT — a card no collector exists for, or the deep
		// era, where a card payment is dated before the card's own
		// ledger begins — the bill has no counter-leg and is the only
		// trace of that spending. This rule places `card_spend`: a
		// placeholder primary, kept IN the spending base and shown as
		// its own line, so the money is counted as what it is — generic
		// spend on a card wealthdb does not itemise — rather than
		// deleted as an own-account move. Collecting the card flips the
		// verdict on the next pass, since the leg then pairs.
		//
		// Three pattern sets: the generic phrases that name the ACT of
		// paying a card, cardIssuerDescriptors, the issuer formats seen
		// on real exports, and cardIssuerShapes, the masked card numbers
		// a phrase cannot spell. None names a retailer — see the table's
		// comment for why a store card is left alone.
		//
		// A row the cash-withdrawal rule matches is not one of these,
		// whatever else it carries. Cash taken at a machine is booked
		// against the card that opened the drawer, so the counterparty
		// is the masked card number this rule reads as a card bill —
		// and the money is cash out, not spend on a card. The refusal
		// is what stops rule order from deciding it: the later rule
		// places the row from its own narrative, or, where only the
		// bank's booking type names the machine, the provider tier
		// does (§3). Cash is never card spend, and it will not become
		// so when the card is collected either — a withdrawal is not
		// on the card statement.
		refusedBy: &cashWithdrawalRule,
		detailed:  canonical.SpendDetailedCardSpend,
		tokens:    []string{"AUTOPAY", "AUTOPMT", "EPAY", "CARDMEMBER"},
		phrases: append([]string{
			"AUTO PAY", "AUTOMATIC PAYMENT", "PAYMENT THANK YOU",
			"ONLINE PAYMENT", "ELECTRONIC PAYMENT", "CARD PAYMENT",
			"CREDIT CARD PAYMENT", "CREDIT CRD", "PAYMENT TO CARD",
			"CARD PMT", "CC PAYMENT",
		}, cardIssuerDescriptors...),
		shapes: cardIssuerShapes,
	},
	cashWithdrawalRule,
	{
		// A mortgage payment is an own-account move by the product's own
		// definition: the mortgage is a tracked account
		// (AccountKindMortgage), so the payment moves value between two
		// accounts already on the balance sheet rather than out of them.
		// Counting it as spend would double-count against the liability
		// the payment reduces.
		//
		// TODO(cashflow): part of that payment — the interest — really
		// IS consumed, and only the principal share is the own-account
		// move. The transaction does not carry the split, and deriving
		// it needs an amortisation view the product has no place for
		// yet. When the cashflow feature lands, revisit whether the
		// interest leg should surface as spending after all.
		detailed: canonical.SpendDetailedInternalTransfer,
		tokens:   []string{"MORTGAGE", "HYPOTHEK", "HYPOTHEKARZINS"},
		phrases:  []string{"HOME LOAN", "MORTGAGE PAYMENT"},
	},
}

// RuleCategory applies the built-in rule tier to a row: its merchant
// signature and the two raw narrative fields it was built from, each
// tested on its own — the description up to its memo separator, never
// the memo behind it. It returns the detailed category and whether any
// rule fired; which rule fired is not carried, because provenance in
// gold is the tier rather than the individual rule. Rule order
// outranks field order — a row whose signature fits a later rule and
// whose description fits an earlier one gets the earlier verdict — and
// a row with no text anywhere never fires.
//
// The provider's own filing of the row is read for REFUSALS only
// (spendRule.refusedBy), never to place a verdict. A booking type is
// the bank's structured classification of the entry rather than a
// narrative, and a rule that placed a category from it would be the
// provider tier wearing the rule tier's provenance and outranking it.
// Reading it to decline a row is the opposite move: it lets the tier
// below, which owns that filing, have the row.
func RuleCategory(signature, counterparty, description, providerCategory string) (detailed string, ok bool) {
	description, _ = canonical.SplitDescriptionMemo(description)
	fields := make([]narrativeField, 0, 3)
	for _, s := range []string{signature, counterparty, description} {
		if f, ok := newNarrativeField(s); ok {
			fields = append(fields, f)
		}
	}
	refusalFields := fields
	if f, ok := newNarrativeField(providerCategory); ok {
		refusalFields = append(append([]narrativeField{}, fields...), f)
	}
	for _, r := range builtinRules {
		if r.refusedBy != nil && r.refusedBy.matchesAny(refusalFields) {
			continue
		}
		if r.matchesAny(fields) {
			return r.detailed, true
		}
	}
	return "", false
}

// narrativeField is one text the rules are tested against, folded
// once: its tokens as a set, for the token and shape patterns, and
// joined by single spaces, for the phrases.
type narrativeField struct {
	tokens map[string]bool
	joined string
}

// newNarrativeField folds one text; the second result is false when
// the text holds no token at all.
func newNarrativeField(s string) (narrativeField, bool) {
	tokens := tokenize(s)
	if len(tokens) == 0 {
		return narrativeField{}, false
	}
	set := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		set[tok] = true
	}
	return narrativeField{tokens: set, joined: strings.Join(tokens, " ")}, true
}

// Rule is one entry of `spending.rules`, compiled: a case-insensitive
// pattern over a row's raw narrative and the category a match places.
// Rules are the one deployment-specific input to the rule tier. The
// category may be any valid spend_detailed value, vendored or delta:
// a rule is the holder's own local input, applied by this pass and
// never shown to the model, so the vendored-only restriction the
// model tier lives under has no reason to reach it.
//
// It exists for three populations neither the matcher nor the model
// can ever reach. One is own-money movement whose receiving side is
// booked nowhere in gold — a wire to the holder's account at a bank
// the product does not track, a transfer to an exchange it does — so
// the outgoing leg is one-legged forever. Another is capital deployed
// to a destination the product does not track — a subscription the
// bank books as a plain withdrawal. The third is consumption paid by
// wire — a lawyer, a contractor, a tax office — whose narrative is
// person- or IBAN-shaped and therefore fenced from the model
// (TransferShaped): nothing but a rule or a pin can categorise it,
// and a rule is the right instrument for a counterparty that recurs.
// In all three the only thing that identifies the row is text in the
// narrative: a name, an account number, a legal entity. That text is
// personal, so the rules live in the user's config, never in the
// repository.
type Rule struct {
	Match    *regexp.Regexp
	Category string
}

// ConfigRuleCategory applies the config-supplied rules to a row's
// narrative — first match wins, in the order written — and returns the
// category the match places. It is consulted AFTER the three built-in
// rules, so a built-in verdict is never overridden by a pattern that
// happens to fire on the same row.
//
// Like the built-ins it reads the raw narrative on each field
// separately — an IBAN or a legal entity may fall past the signature's
// 64-character cap, and a pattern anchored to one field must not
// straddle the join — but it never sees the signature: a regex is the
// holder's own spelling, written against what the bank printed. Unlike
// the built-ins it reads the description whole, memo included: a
// deployment rule may key on what the payer wrote a payment was for,
// which is the holder's own local input about the holder's own rows.
// Patterns arrive compiled case-insensitively by the config loader; a
// nil list never fires.
func ConfigRuleCategory(rules []Rule, counterparty, description string) (string, bool) {
	for _, r := range rules {
		if (counterparty != "" && r.Match.MatchString(counterparty)) ||
			(description != "" && r.Match.MatchString(description)) {
			return r.Category, true
		}
	}
	return "", false
}

// matchesAny reports whether the rule fires on any one of the fields,
// each tested whole so a phrase never straddles the join between two
// of them.
func (r spendRule) matchesAny(fields []narrativeField) bool {
	for _, f := range fields {
		if r.matches(f.tokens, f.joined) {
			return true
		}
	}
	return false
}

func (r spendRule) matches(tokens map[string]bool, joined string) bool {
	for _, tok := range r.tokens {
		if tokens[tok] {
			return true
		}
	}
	// A phrase is a run of WHOLE tokens: the joined form is padded so
	// a phrase can neither start nor end inside a token — SCORECARD
	// PAYMENTS does not carry the phrase CARD PAYMENT.
	padded := " " + joined + " "
	for _, phrase := range r.phrases {
		if strings.Contains(padded, " "+phrase+" ") {
			return true
		}
	}
	// A shape is one whole token by pattern: the set already holds
	// the tokens split, so no boundary padding is needed.
	for _, shape := range r.shapes {
		for tok := range tokens {
			if shape.MatchString(tok) {
				return true
			}
		}
	}
	return false
}
