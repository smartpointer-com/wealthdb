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
	// issuers is the descriptor table a match is LABELLED from, and
	// only the card-payment rule has one. A rule without it never
	// labels; see spendRule.label and cardIssuers.
	issuers []cardIssuer
	// refusedBy vetoes the rule for a row any of its fields match. It
	// is how one rule declines a shape a later rule owns, where the
	// two patterns genuinely overlap on the same row and rule order
	// alone would give it to the wrong one.
	refusedBy *spendRule
}

// cardIssuer is one row of the card-payment rule's issuer table
// (cardIssuers): an issuer's display name, and the descriptor phrases
// that identify a bill paid to it.
type cardIssuer struct {
	name    string
	phrases []string
}

// cardIssuers is the card-payment rule's descriptor table. Each entry is
// an issuer, and the phrases a deposit-account export prints on a bill
// paid to it — one phrase per known statement format.
//
// The name is what the bill is LABELLED with. A `card_spend` line
// carries it as its merchant (migration 0052), because the issuer is
// the only handle on which card the money went to; the bill's
// signature names the payer's own bank or the holder, and is not a
// merchant. An entry with an EMPTY name is a descriptor that says a
// row is a card bill without saying whose card it is: it places the
// verdict exactly as a named one does and labels nothing.
//
// The digits are masked in the comments and never reach a signature
// anyway — a trailing card number is a reference number to Normalize —
// and an issuer's name is not personal data, so the names stay.
//
// Each phrase is matched as a whole-word phrase ANYWHERE in the
// signature or in either raw narrative field, which is what makes the
// rule indifferent to what precedes the issuer. A Swiss direct debit
// leads with the mandate notice —
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
// stop losing — and would put a retailer's name in the merchant column
// of a line that is not its purchases.
var cardIssuers = []cardIssuer{
	{"Chase", []string{
		// PAYMENT TO CHASE CARD ENDING IN ####
		// CHASE CREDIT CRD AUTOPAY
		"PAYMENT TO CHASE CARD", "CHASE CREDIT CRD",
	}},
	{"American Express", []string{
		// AMERICAN EXPRESS ACH PMT    M### / A###
		// AMERICAN EXPRESS CREDIT CARD
		"AMERICAN EXPRESS ACH PMT", "AMERICAN EXPRESS CREDIT CARD",
	}},
	{"Citi", []string{
		// CITI CARD ONLINE PAYMENT    ####
		// CITI AUTOPAY     PAYMENT    ####
		// CITI CREDIT CARD PAYMENT
		"CITI CARD ONLINE PAYMENT", "CITI AUTOPAY", "CITI CREDIT CARD PAYMENT",
	}},
	{"UBS Card Center", []string{
		// UBS SWITZERLAND AG;C/O UBS CARD CENTER
		// UBS AG;C/O UBS CARD CENTER AG …
		// …; UBS CARD CENTER; CREDIT CARD STATEMENT …   (direct debit)
		// …; UBS CARD CENTER; CARD PAYMENT …            (direct debit)
		//
		// VIS1W is the objection notice the card centre prints on its
		// own LSV collection, and it is the descriptor that does not
		// depend on the creditor line: a statement that drops
		// `c/o UBS Card Center` and prints a plain company address
		// instead (`UBS SWITZERLAND AG; 9999 EXAMPLETOWN`, synthetic)
		// would otherwise stop being read as a card bill at all. The
		// code names the scheme, so it survives a change of address.
		"UBS CARD CENTER", "VIS1W",
	}},
	{"", []string{
		// XXXX XXXX XXXX ####: a masked card number as the whole
		// counterparty is a card top-up. It says the row is a card
		// bill and nothing about who issued the card, so it names no
		// issuer and the line it places carries no label. The masking
		// that keeps some of the digits is a shape, not a phrase —
		// see cardIssuerShapes.
		"XXXX XXXX XXXX",
	}},
}

// cardIssuerPhrases flattens the table into the card rule's phrase
// list. What a bill LOOKS like is spelled once, in the table, which
// stays the only place an issuer is named.
func cardIssuerPhrases() []string {
	out := make([]string, 0, len(cardIssuers))
	for _, iss := range cardIssuers {
		out = append(out, iss.phrases...)
	}
	return out
}

// cardIssuerShapes are the masked card numbers a phrase cannot spell:
// where the export keeps some of the digits, every card is a different
// string and only the SHAPE is shared. Each is matched against a
// single whole token, anywhere in the signature, and places the card
// rule's verdict exactly as a descriptor phrase does. The same
// issuers-only boundary applies. A shape names no issuer — a masked
// number says which card, never whose — so a bill it places carries
// no label, exactly as the bare masked-card descriptor does.
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
// Each rule must keep a DISTINCT detailed value. RuleCategory names no
// rule — it returns the category and the issuer label a card bill
// carries — so the tests identify which rule fired by the value it
// assigns; two rules sharing one would quietly weaken those tests to a
// category assertion. If a second rule ever has to assign an existing
// value, give the tests an unexported matchRule over the same inputs,
// returning (spendRule, bool), that RuleCategory wraps — rather than
// putting the rule's name back on the exported signature: provenance
// in gold is the tier, not the rule.
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
		// paying a card, the descriptor phrases of cardIssuers, the issuer
		// formats a deposit-account export prints, and cardIssuerShapes,
		// the masked
		// card numbers a phrase cannot spell. None names a retailer — see
		// the table's comment for why a store card is left alone.
		//
		// The issuer table also LABELS the bill: a match on a named
		// issuer's descriptor carries that issuer's name out of
		// RuleCategory, and the enrichment pass stores it as the line's
		// merchant label, which is what lets a report group card spend by
		// the card it went to. A match on a generic phrase, on the bare
		// masked-card descriptor or on a shape names no issuer and labels
		// nothing.
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
		// "ONLINE PAYMENT" is deliberately NOT here. A bank's bill-pay
		// descriptor is "Online Payment <ref> To <payee>" — a payment to
		// whoever the holder addressed it to, a landlord as readily as a
		// card — so the bare phrase says the payment was made online and
		// nothing about what it paid. Read as a card bill it files real
		// spending under a card the holder may not even have, and the
		// payee sitting right there in the narrative is ignored. The
		// issuers that DO announce themselves this way keep their own
		// phrase ("CITI CARD ONLINE PAYMENT"), which still matches.
		phrases: append([]string{
			"AUTO PAY", "AUTOMATIC PAYMENT", "PAYMENT THANK YOU",
			"ELECTRONIC PAYMENT", "CARD PAYMENT",
			"CREDIT CARD PAYMENT", "CREDIT CRD", "PAYMENT TO CARD",
			"CARD PMT", "CC PAYMENT",
		}, cardIssuerPhrases()...),
		shapes:  cardIssuerShapes,
		issuers: cardIssuers,
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
// the memo behind it. It returns the detailed category, the label the
// match carries, and whether any rule fired; which rule fired is not
// carried, because provenance in gold is the tier rather than the
// individual rule. Rule order outranks field order — a row whose
// signature fits a later rule and whose description fits an earlier
// one gets the earlier verdict — and a row with no text anywhere never
// fires.
//
// The label is the ISSUER a card bill was paid to, and it is empty for
// everything else: every other built-in has no issuer table, and
// inside the card rule a generic card-payment phrase, the bare
// masked-card descriptor and the masked-card shapes name no issuer.
// The pass stores it as the line's merchant label, which is the whole
// of the exception to a delta line carrying no merchant.
//
// The provider's own filing of the row is read for REFUSALS only
// (spendRule.refusedBy), never to place a verdict. A booking type is
// the bank's structured classification of the entry rather than a
// narrative, and a rule that placed a category from it would be the
// provider tier wearing the rule tier's provenance and outranking it.
// Reading it to decline a row is the opposite move: it lets the tier
// below, which owns that filing, have the row.
func RuleCategory(signature, counterparty, description, providerCategory string) (detailed, label string, ok bool) {
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
			return r.detailed, r.label(fields), true
		}
	}
	return "", "", false
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
	// Scope optionally narrows where and when the rule may fire. The
	// zero value constrains nothing, which is what a rule written
	// without a scope means.
	Scope RuleScope
}

// RuleScope narrows a rule to part of the ledger: a source, a
// portfolio, an account, a date range, or any combination. An empty
// field does not constrain. `From`/`To` are unix seconds and inclusive
// — the config layer resolves the named days to their first and last
// second, so a one-day scope is written with both dates the same.
//
// The point is that a pattern specific enough for one booking is
// rarely specific enough for the whole future: a rule that reads a
// mortgage settlement correctly today is a liability the day another
// bank writes the same word on something else. A scope lets such a
// rule stay surgical instead of permanent.
type RuleScope struct {
	Source    string
	Portfolio string
	Account   string
	From      int64
	To        int64
}

// Any reports whether the scope constrains nothing — the hot path
// skips the check entirely for the rules that carry no scope.
func (s RuleScope) Any() bool {
	return s.Source == "" && s.Portfolio == "" && s.Account == "" &&
		s.From == 0 && s.To == 0
}

// Admits reports whether a row is inside the scope. An empty portfolio
// or account on the row can never satisfy a scope that names one.
func (s RuleScope) Admits(source, portfolio, account string, occurredAt int64) bool {
	switch {
	case s.Source != "" && s.Source != source:
		return false
	case s.Portfolio != "" && s.Portfolio != portfolio:
		return false
	case s.Account != "" && s.Account != account:
		return false
	case s.From != 0 && occurredAt < s.From:
		return false
	case s.To != 0 && occurredAt > s.To:
		return false
	}
	return true
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
func ConfigRuleCategory(rules []Rule, row RuleRow) (string, bool) {
	for _, r := range rules {
		if !r.Scope.Any() && !r.Scope.Admits(row.Source, row.Portfolio, row.Account, row.OccurredAt) {
			continue
		}
		// The issuer's own filing is a field a rule may match on, like
		// the counterparty and the narrative. It is the only way to
		// write a rule about a class of merchant the descriptor does
		// not name — a membership, a trade — and it is what makes the
		// issuer an INPUT to our classification rather than a tier
		// that outranks it. Tested whole, on the raw value, so a rule
		// says what the issuer said.
		if (row.Counterparty != "" && r.Match.MatchString(row.Counterparty)) ||
			(row.Description != "" && r.Match.MatchString(row.Description)) ||
			(row.ProviderCategory != "" && r.Match.MatchString(row.ProviderCategory)) {
			return r.Category, true
		}
	}
	return "", false
}

// RuleRow is what a config rule is tested against: the narrative it
// matches on, plus the facts an optional scope narrows by. Passing a
// struct rather than a widening argument list is what keeps a new
// scope dimension from touching every caller.
type RuleRow struct {
	Counterparty string
	Description  string
	// ProviderCategory is the issuer's own filing of the row, verbatim.
	// A rule may match it like any other field; it never decides on its
	// own (see the provider tier in enrich.go, which records it and
	// declines a catch-all).
	ProviderCategory string
	Source           string
	Portfolio        string
	Account          string
	OccurredAt       int64
}

// matchesAny reports whether the rule fires on any one of the fields,
// each tested whole so a phrase never straddles the join between two
// of them.
func (r spendRule) matchesAny(fields []narrativeField) bool {
	for _, f := range fields {
		if r.matches(f) {
			return true
		}
	}
	return false
}

// label is the display name of the first NAMED issuer in the rule's
// table whose descriptor matches one of the fields, and "" when none
// does. A rule with no table — every built-in but the card rule —
// never labels, and neither do the card rule's own unnamed
// descriptors: what they identify is a card bill, not whose card.
//
// The table's order decides a row naming two issuers, the way rule
// order decides a row matching two rules; the fields are equal, since
// a creditor named in the description alone is as much the creditor as
// one named in the key.
func (r spendRule) label(fields []narrativeField) string {
	for _, iss := range r.issuers {
		if iss.name == "" {
			continue
		}
		for _, f := range fields {
			for _, phrase := range iss.phrases {
				if f.hasPhrase(phrase) {
					return iss.name
				}
			}
		}
	}
	return ""
}

// hasPhrase reports whether the field carries the phrase as a run of
// WHOLE tokens: the joined form is padded so a phrase can neither
// start nor end inside a token — SCORECARD PAYMENTS does not carry the
// phrase CARD PAYMENT.
func (f narrativeField) hasPhrase(phrase string) bool {
	return strings.Contains(" "+f.joined+" ", " "+phrase+" ")
}

func (r spendRule) matches(f narrativeField) bool {
	for _, tok := range r.tokens {
		if f.tokens[tok] {
			return true
		}
	}
	for _, phrase := range r.phrases {
		if f.hasPhrase(phrase) {
			return true
		}
	}
	// A shape is one whole token by pattern: the set already holds
	// the tokens split, so no boundary padding is needed.
	for _, shape := range r.shapes {
		for tok := range f.tokens {
			if shape.MatchString(tok) {
				return true
			}
		}
	}
	return false
}
