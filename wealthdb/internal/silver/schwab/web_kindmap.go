package schwab

import (
	"encoding/json"
	"regexp"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// webKind maps the web silver's `kind` discriminator (the
// space-separated text Schwab uses on statements and in
// transaction-history exports) to canonical TxKind. Conservative
// — unmapped strings route to TxKindOther so no semantics are
// invented. Schwab uses different vocab on statements vs the
// Trader API; the api-side mapping is in kindmap.go.
//
// Reinvested dividends land as two separate transactions on
// Schwab statements: the cash-income side ("Reinvest Dividend")
// and the share-purchase side ("Reinvest" / "Reinvest Shares").
// They map to dividend and buy respectively so the canonical
// single-entry sums stay correct (the two cancel out, net zero
// cash impact). See canonical/sign.go for the per-kind sign rules.
//
// Options trades use Schwab's open/close legs: Buy to Open and
// Buy to Close both leave cash (acquiring a long position or
// closing a short); Sell to Open and Sell to Close both add
// cash (receiving premium or closing a long). All four collapse
// to plain buy/sell at the canonical layer.
//
// Corporate actions (Spin-off, Split, Reverse Split, Exchange,
// Reorganized Issue, Return Of Capital, Cash In Lieu, etc.)
// collapse to TxKindCorporateAction. The canonical sign helper
// passes source-supplied signs through for that kind because
// the cash impact varies: cash-in-lieu yields cash, a plain
// split is zero, a cash merger pays out.
//
// "Transfer Out" is directional, and what it gets wrong is not the
// direction but the KIND. One word covers two movements: a securities
// delivery, which is an in-kind leg, and a CASH distribution, which is
// money leaving the household. Mapped alike they are read alike, and
// TxKindTransferOut is the wrong half of that pair to be read as — the
// cash flow statement excludes an unmatched one as "an in-kind ledger leg
// or a source's own tagging", and neither family's population admits the
// kind, so a cash row lands beyond every tier, the matcher and the pins
// ledger at once. It leaves the pool and nothing draws it. The payload
// already separates the two for the dedupe below (distributionIsCash), so
// the same fact decides the kind here, and a cash row books as the
// withdrawal the API era books the same movement as. The SIGN is not read:
// unlike the undirected words below this one states its direction, and
// ApplyCanonicalSign enforces it.
//
// THREE OF THE TRANSFER WORDS TAKE THEIR DIRECTION FROM THE AMOUNT rather
// than from the word, which states none. A journal is invisible to the
// internal-transfer matcher — which admits no such kind — and excluded from
// the cash flow statement outright, so an undirected word leaves the leg
// waiting at the other end unpairable and drawn as money arriving from, or
// vanishing into, nowhere.
//
// Two of them name an OUTSIDE bank: "MoneyLink Transfer" and the undirected
// "Transfer". The API era books those as withdrawals and deposits, so reading
// the sign is continuity across the feed cutover rather than invented
// semantics. The third is a funds journal inside the statement parser's
// "Unknown" catch-all, matched by narrative rather than by kind because that
// bucket is heterogeneous on purpose (see the case below).
//
// What stays a journal is narrower than "the holder's own accounts", and the
// distinction is the point: "Journal" and the two SHARE journals move
// securities, or move cash the boundary has no interest in, while a funds
// journal can reach an account outside the household. Whether a move
// between two accounts one person can log into crosses the household boundary
// is gold's question, not this adapter's — which is why the answer here is a
// DIRECTION and never a verdict.
//
// A row with no amount, or a zero one, stays a journal either way: the
// sign is the whole of the evidence, and a directionless row has no right
// answer.
func webKind(s string, netAmount *canonical.Decimal, description *string, payload string) canonical.TxKind {
	switch s {
	case "Buy", "Buy to Open", "Buy to Close", "Purchase",
		"Reinvest", "Reinvest Shares":
		return canonical.TxKindBuy
	case "Sell", "Sell to Open", "Sell to Close", "Sale", "Redemption":
		return canonical.TxKindSell
	case "Cash Dividend", "Dividend", "Non-Qualified Div",
		"Qualified Dividend", "Reinvest Dividend",
		"Pr Yr Cash Div", "Special Dividend", "Special Qual Div":
		return canonical.TxKindDividend
	case "Bond Interest", "Credit Interest", "Interest",
		"Bank Interest", "Bank Interest Adj", "Margin Interest":
		return canonical.TxKindInterest
	case "Fee", "Service Fee", "ADR Mgmt Fee":
		return canonical.TxKindFee
	case "Foreign Tax Paid", "NRA Tax Adj", "Pr Yr NRA Tax", "Tax":
		return canonical.TxKindTax
	case "Deposit", "MoneyLink Deposit", "Funds Received", "Wire Received":
		return canonical.TxKindDeposit
	case "Withdrawal", "Wire Sent":
		return canonical.TxKindWithdrawal
	case "Transfer Out":
		// Directional third_party_distribution rows (INTEROP §8.2);
		// distinct from the undirected "Transfer" below, whose
		// direction is read off the amount rather than the word.
		// A cash distribution under this word is money out of the
		// household rather than an in-kind leg, and books as a
		// withdrawal so that it reaches the spending population at
		// all (see above).
		if distributionIsCash(payload) {
			return canonical.TxKindWithdrawal
		}
		return canonical.TxKindTransferOut
	case "Transfer In":
		return canonical.TxKindTransferIn
	case "MoneyLink Transfer", "Transfer":
		return webTransferSide(netAmount)
	case "Security Transfer", "Journal", "Journaled Shares":
		return canonical.TxKindJournal
	case "Exchange", "Reorganized Issue", "Spin-off", "Split",
		"Reverse Split", "Return Of Capital", "Cash In Lieu",
		"Litigation", "Unissued Rights Redemption":
		return canonical.TxKindCorporateAction
	case "Unknown":
		// The statement parser's catch-all, and it is heterogeneous on
		// purpose: option legs, ADR fees, corporate actions, share
		// journals. The shapes in it that are plainly cash are read by
		// the name the statement gives them (catchAllShapes); remapping
		// the catch-all wholesale would re-kind everything else in it.
		//
		// One of them is a funds journal between two of the holder's
		// accounts, which the statement narrates as a direction and a
		// partner account. Such a journal is NOT automatically
		// internal: the partner can be an account outside the
		// household. Making the row directional is what lets the
		// household boundary decide — kept as an unsigned catch-all
		// it could decide nothing, and a move out of the household
		// would draw as no movement at all.
		if description == nil {
			break
		}
		if fundsJournalDescription.MatchString(*description) {
			return webTransferSide(netAmount)
		}
		for _, shape := range catchAllShapes {
			if shape.name.MatchString(*description) {
				return shape.kind
			}
		}
	}
	return canonical.TxKindOther
}

// fundsJournalDescription names the funds journal inside the statement
// parser's "Unknown" bucket. Anchored, because the word has to lead: a
// narrative merely mentioning a journal is not one.
var fundsJournalDescription = regexp.MustCompile(`^Journaled Funds\b`)

// catchAllShapes are the other cash shapes of the statement parser's
// "Unknown" bucket, by the name the statement prints first. A short sale
// is cash raised by selling what the account does not hold (a stock, or
// an option written) and a cover the cash spent closing it; the rest are
// a pass-through or borrow fee, a withholding or its reclaim, and a
// fund's capital-gain payout or a dividend's correction. Anchored like
// the journal above. Whatever else the bucket carries — a share
// journal, an in-kind transfer, a corporate action, an expiry — moves
// no cash of its own and stays TxKindOther.
var catchAllShapes = []struct {
	name *regexp.Regexp
	kind canonical.TxKind
}{
	{regexp.MustCompile(`^Short Sale\b`), canonical.TxKindSell},
	{regexp.MustCompile(`^Cover Short\b`), canonical.TxKindBuy},
	{regexp.MustCompile(`^(?:ADR Pass Thru Fee|Service Fee)\b`), canonical.TxKindFee},
	{regexp.MustCompile(`^(?:Foreign Tax Paid|Frgn Tax Reclaim|NRA Withholding)\b`), canonical.TxKindTax},
	{regexp.MustCompile(`^(?:Adjust Dividend|LT Cap Gain|Short Term Cap Gn)\b`), canonical.TxKindDividend},
}

// webTransferSide reads an external transfer's direction off its amount, and
// refuses to guess one for a row that carries none.
func webTransferSide(netAmount *canonical.Decimal) canonical.TxKind {
	switch {
	case netAmount == nil || netAmount.IsZero():
		return canonical.TxKindJournal
	case netAmount.IsNegative():
		return canonical.TxKindWithdrawal
	default:
		return canonical.TxKindDeposit
	}
}

// webSigned orients a web row's figure by its kind. The web feeds print
// most figures as magnitudes and leave the direction to the section or
// the word — a reinvestment, a distribution's market value — so the
// kind's canonical sign is applied. A minus printed on an inflow is the
// exception and is kept: the statements print one only on a correction
// (a dividend clawed back), which the canonical sign would book as a
// second dividend.
//
// The statement parser's "Unknown" bucket keeps its sign whole. Its rows
// carry the figure as printed, a debit in parentheses, and the kind a
// shape in it is read as does not state a direction the row lacks: a
// withholding reclaimed is a tax row with a credit.
func webSigned(silverKind string, kind canonical.TxKind, amount *canonical.Decimal) *canonical.Decimal {
	if silverKind == "Unknown" {
		return amount
	}
	signed := canonical.ApplyCanonicalSign(kind, amount)
	if amount != nil && amount.IsNegative() && signed != nil && signed.IsPositive() {
		return amount
	}
	return signed
}

// distributionIsCash reports whether a third_party_distribution row is a cash
// transfer (payload.transfer_kind == "cash"). Cash transfers may overlap a
// statement cash debit and are deduped against it; securities transfers
// (transfer_kind == "securities") are new data and are left untouched.
func distributionIsCash(payload string) bool {
	var p struct {
		TransferKind string `json:"transfer_kind"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return p.TransferKind == "cash"
}
