package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// UBS books a dividend to the cash account it credits, beside the
// custody account that holds the security. The line names the security
// in the bank's own words, and gold keys the security by its ISIN. So a
// dividend line is linked to its security here, by the first of three
// roads that answers:
//
//  1. The confirmation it pairs with (MT940 lines). The custody
//     account's MT566 states the ISIN and the cash it posted: the cash
//     account, the payment day, the amount and its currency. A credit
//     line equal on all four is that confirmation's cash leg — where
//     one line and one confirmation's statement answer the four facts,
//     and the line states no valor of another security. A confirmation
//     a cancellation or a reversal withdraws names the security and
//     nothing else (pairOf).
//  2. The valor the line states, `VN <valor>`: in the MT940 line's
//     customer reference, or in a statement's narrative. It resolves
//     through the valor index (buildValorIndex) and its collision rule.
//  3. The ticker that closes the line's narrative, `(<TKR>)`, against
//     the ticker that closes the description of each custody position
//     the same portfolio's statements of assets print. Only where
//     exactly one ISIN answers, and never a token that is a currency or
//     shorter than three characters: a share-class or domicile marker
//     looks like a ticker.
//
// Each road reads the line's own silver fields, not the text the row
// attaches, so a row's instrument never depends on its text columns.
//
// A line no road answers keeps no instrument. A valor it stated goes on
// instrument_hint, as a statement-era trade's does, for a config link to
// close.
//
// The bank corrects a dividend by reversing its credit: an MT940 line
// marked `RC`, signed negative whatever its kind (buildTransaction).
// Where road 2 or 3 names its security, and the line or a credit on or
// before its day says the credit was a dividend, it is that security's
// dividend reversal (creditReversal). Each reversal takes back its own
// credit (matchReversals), at what that credit was booked at: the gross
// and its tax, where gold booked a gross (reversedGross). Any other
// reversal keeps the kind map's reading. An export row's reversal states
// its own gross (webStatedGross).
// docs/adapters/ubs.md §7 has the whole rule.
//
// UBS credits a dividend net of the tax withheld at source. The
// confirmation states the gross beside it, by its amounts or its rates
// (grossOf), and so does an export row, in the row's currency or in the
// security's beside the rate it converted at (webStatedGross). Where
// either does, the dividend is booked as a brokerage books one: the
// dividend at the gross, and the withholding as its own `tax` row on the
// same account, day, currency and instrument (bookAtGross). Income then
// reads the gross, its `withheld` memo the tax row, and spending places
// the tax row as withholding tax (docs/INCOME.md §5).

// withholdingLabel opens a booked withholding row's description; the
// security's name follows it.
const withholdingLabel = "Withholding tax"

// withholdingIDSuffix closes a booked withholding row's id, after the id
// of the dividend it was withheld from.
const withholdingIDSuffix = ":tax"

// withholdingPayload is what a booked withholding row's payload states:
// the dividend it was withheld from and the confirmation stating it, if
// one does.
type withholdingPayload struct {
	WithheldFrom    string `json:"withheld_from"`
	CorporateAction string `json:"corporate_action,omitempty"`
}

// corporateActionKey is the payload key naming the confirmation a
// dividend line was paired with.
const corporateActionKey = `"corporate_action":`

// grossOf is the gross the movement states for a dividend that credited
// `credited` (a magnitude) in `currency`. Its amounts state it first: the
// gross entitlement (GRSS) where it is stated in that currency, else the
// credited amount plus the tax (TAXR) where that is. Where neither is, a
// rate may (rateGross). ok is false where nothing states one. It is false
// too where the difference cannot be withholding: a tax stated as zero
// beside a gross that differs, or a gross below the credited amount. The
// line then keeps what it credited.
func (m caCashMove) grossOf(credited canonical.Decimal, currency string) (gross canonical.Decimal, ok bool) {
	currency = bookingCurrency(currency)
	switch {
	case m.grossCcy != "" && bookingCurrency(m.grossCcy) == currency && !m.gross.IsZero():
		gross = m.gross
	case m.taxCcy != "" && bookingCurrency(m.taxCcy) == currency:
		gross = credited.Add(m.tax)
	default:
		if gross, ok = m.rateGross(credited, currency); !ok {
			return canonical.Decimal{}, false
		}
	}
	nothingWithheld := m.taxCcy != "" && m.tax.IsZero()
	if gross.LessThan(credited) || nothingWithheld && !gross.Equal(credited) {
		return canonical.Decimal{}, false
	}
	return gross, true
}

// rateGross is the gross the confirmation states through a rate, where no
// amount states it in the currency credited. In order:
//
//  1. the gross amount at the confirmation's exchange rate, which is the
//     conversion the bank made;
//  2. the gross rate per share times the confirmed balance, in the
//     currency credited or at the exchange rate;
//  3. the credited amount over one less the withholding rate.
//
// Each is rounded to two decimals, and holds only where it fits what moved
// (fitsCredit). false where none does.
func (m caCashMove) rateGross(credited canonical.Decimal, currency string) (canonical.Decimal, bool) {
	var grosses []canonical.Decimal
	if m.grossCcy != "" && !m.gross.IsZero() {
		if gross, ok := m.inCurrency(m.gross, m.grossCcy, currency); ok {
			grosses = append(grosses, gross)
		}
	}
	if m.grossRate != nil && m.quantity != nil {
		if gross, ok := m.inCurrency(m.grossRate.Mul(*m.quantity), m.grossRateCcy, currency); ok {
			grosses = append(grosses, gross)
		}
	}
	hundred := canonical.NewDecimalFromInt(100)
	if m.taxRate != nil && !m.taxRate.IsNegative() && m.taxRate.LessThan(hundred) {
		grosses = append(grosses, credited.Div(hundred.Sub(*m.taxRate).Div(hundred)))
	}
	for _, gross := range grosses {
		if gross = gross.Round(2); fitsCredit(gross, credited) {
			return gross, true
		}
	}
	return canonical.Decimal{}, false
}

// inCurrency is amount, stated in from, in the currency credited: as it
// is where from is that currency, else at the confirmation's exchange
// rate where that rate links the two, either way round. false where
// nothing does.
func (m caCashMove) inCurrency(amount canonical.Decimal, from, currency string) (canonical.Decimal, bool) {
	from = bookingCurrency(from)
	x := m.exchange
	switch {
	case from == currency:
		return amount, true
	case x == nil:
	case bookingCurrency(x.from) == from && bookingCurrency(x.to) == currency:
		return amount.Mul(x.rate), true
	case bookingCurrency(x.from) == currency && bookingCurrency(x.to) == from:
		return amount.Div(x.rate), true
	}
	return canonical.Decimal{}, false
}

// fitsCredit reports whether a gross read through a rate fits the amount
// credited, a magnitude: at least it, and no more than twice it. A rate
// read the wrong way, such as per unit where it is quoted per 100 units,
// lands far outside.
func fitsCredit(gross, credited canonical.Decimal) bool {
	return !gross.LessThan(credited) && !gross.GreaterThan(credited.Add(credited))
}

// bookGross books a dividend at the gross its confirmation states
// (bookAtGross). withheld is false where the confirmation states no
// gross in the row's currency, so the row keeps what it credited, and
// where nothing was withheld.
func bookGross(tx *canonical.TransactionChange, c cashConfirmation) (tax canonical.TransactionChange, withheld bool, err error) {
	if tx.NetAmount == nil {
		return canonical.TransactionChange{}, false, nil
	}
	gross, ok := c.move.grossOf(tx.NetAmount.Abs(), tx.Currency)
	if !ok {
		return canonical.TransactionChange{}, false, nil
	}
	return bookAtGross(tx, gross, c.eventID, c.name)
}

// webGrossRe is the gross an export dividend row states in its
// Description3, before the tax withheld at source: `Number/Amt.
// <gross> <currency>`, in either sign, its thousands grouped by `'` or
// not.
var webGrossRe = regexp.MustCompile(`\bNumber/Amt\.\s+(-?\d[\d']*(?:\.\d+)?)\s+([A-Z]{3})\b`)

// webRateRe is the rate a foreign security's export dividend row states in
// its Description3, beside its gross: `Exchange rate: <rate>`, the rate
// the bank converted the dividend at.
var webRateRe = regexp.MustCompile(`\bExchange rate:?\s*(\d[\d']*(?:\.\d+)?)`)

// rateQuotations are the amounts of a currency a rate can be quoted
// for: one unit, or a hundred.
var rateQuotations = []int64{1, 100}

// halfCent is how far a figure rounded to two decimals can lie from the
// one it stands for.
var halfCent = canonical.NewDecimalFromInt(5).Shift(-3)

// parseGroupedDecimal reads a figure the export prints, its thousands
// grouped by `'` or not.
func parseGroupedDecimal(s string) (canonical.Decimal, error) {
	return canonical.NewDecimalFromString(strings.ReplaceAll(s, "'", ""))
}

// webStatedGross is the gross an export dividend row states, a
// magnitude. In the row's own currency, it is the gross where it is no
// smaller than what moved. A foreign security's row states its gross in
// the security's currency, beside the rate the bank converted at
// (convertedGross).
func webStatedGross(p webTxPayload, currency string, moved canonical.Decimal) (canonical.Decimal, bool) {
	m := webGrossRe.FindStringSubmatch(p.Description3)
	if m == nil || moved.IsZero() {
		return canonical.Decimal{}, false
	}
	gross, err := parseGroupedDecimal(m[1])
	if err != nil {
		return canonical.Decimal{}, false
	}
	gross, moved = gross.Abs(), moved.Abs()
	if bookingCurrency(m[2]) != bookingCurrency(currency) {
		return convertedGross(p.Description3, gross, moved)
	}
	return gross, !gross.LessThan(moved)
}

// exportGross is the gross the export pass books a web row at
// (webTxPass.emit): the gross a dividend credit's row states
// (webStatedGross), unless the row is a PDF backfill. false for any other
// row. The era fold reads it too (buildEraFold), so it withholds a cash
// leg only where the leg's export copy books a gross.
func exportGross(kind canonical.TxKind, p webTxPayload, pdfBackfill bool, currency string,
	net *canonical.Decimal) (canonical.Decimal, bool) {
	if kind != canonical.TxKindDividend || net == nil || pdfBackfill {
		return canonical.Decimal{}, false
	}
	return webStatedGross(p, currency, *net)
}

// convertedGross is a foreign gross, a magnitude, converted at the rate
// the row states. The row does not say whether the rate is quoted per unit
// or per 100 units, so what moved pins it: the first quotation under which
// the conversion answers. At most one can, for the two lie a hundredfold
// apart.
//
//   - Within the rate's rounding of what moved, nothing was withheld, and
//     the gross is what moved. The rate is printed to a few digits, so the
//     conversion can lie the gross times half a unit of its last digit
//     from the bank's, and half a cent more for the rounding.
//   - Else the conversion, rounded to two decimals, is the gross where it
//     fits the amount credited (fitsCredit).
//
// false where the row states no rate or no quotation answers; the row then
// keeps what it credited.
func convertedGross(description3 string, gross, moved canonical.Decimal) (canonical.Decimal, bool) {
	m := webRateRe.FindStringSubmatch(description3)
	if m == nil {
		return canonical.Decimal{}, false
	}
	rate, err := parseGroupedDecimal(m[1])
	if err != nil || !rate.IsPositive() {
		return canonical.Decimal{}, false
	}
	halfDigit := canonical.NewDecimalFromInt(5).Shift(rate.Exponent() - 1)
	for _, per := range rateQuotations {
		units := canonical.NewDecimalFromInt(per)
		converted := gross.Mul(rate).Div(units)
		if band := gross.Mul(halfDigit).Div(units).Add(halfCent); converted.Sub(moved).Abs().LessThanOrEqual(band) {
			return moved, true
		}
		if converted = converted.Round(2); fitsCredit(converted, moved) {
			return converted, true
		}
	}
	return canonical.Decimal{}, false
}

// bookAtGross books a dividend at gross, a magnitude, in the sign of what
// moved: a reversal of a dividend reverses its gross. The row's net and
// gross amounts read it, and the tax withheld is returned as the row to
// book beside it, on the same account, day, currency and instrument. The
// tax row is the amount that moved less the gross, so the two rows sum to
// the cash that moved, exactly; a rounding or fee difference in the
// bank's figures moves no cash. A reversal's tax row is positive: the
// tax taken back with the dividend. withheld is false where nothing was
// withheld: the row reads the gross, which is what moved. confirmation
// names the confirmation stating the gross, "" where none does (an export
// row, a reversal); name is the security's, else the row's description
// serves.
func bookAtGross(tx *canonical.TransactionChange, gross canonical.Decimal, confirmation, name string) (tax canonical.TransactionChange, withheld bool, err error) {
	moved := *tx.NetAmount
	signed := gross
	if moved.IsNegative() {
		signed = gross.Neg()
	}
	net, total := signed, signed
	tx.NetAmount, tx.GrossAmount = &net, &total
	if signed.Equal(moved) {
		return canonical.TransactionChange{}, false, nil
	}
	encoded, err := json.Marshal(withholdingPayload{
		WithheldFrom:    tx.TransactionExternalID,
		CorporateAction: confirmation,
	})
	if err != nil {
		return canonical.TransactionChange{}, false, fmt.Errorf("ubs withholding payload: %w", err)
	}
	if name == "" {
		name = derefText(tx.Description)
	}
	amount := moved.Sub(signed)
	var instrument *string
	if tx.InstrumentExternalID != nil {
		id := *tx.InstrumentExternalID
		instrument = &id
	}
	return canonical.TransactionChange{
		TransactionExternalID: tx.TransactionExternalID + withholdingIDSuffix,
		OccurredAt:            tx.OccurredAt,
		AccountExternalID:     tx.AccountExternalID,
		InstrumentExternalID:  instrument,
		InstrumentHint:        tx.InstrumentHint,
		Kind:                  canonical.TxKindTax,
		Currency:              tx.Currency,
		NetAmount:             &amount,
		Description:           textPtr(silver.JoinText(withholdingLabel, name)),
		Payload:               json.RawMessage(encoded),
	}, true, nil
}

// statedValorRe is the valor a dividend line states, `VN <valor>`; the
// bank spaces it variously.
var statedValorRe = regexp.MustCompile(`\bVN\s*(\d+)\b`)

// statedValor is the valor `VN <valor>` in s names, or "".
func statedValor(s string) string {
	if m := statedValorRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// closingTicker is the ticker in parentheses that closes a narrative
// (tickerFromDescription), or "".
func closingTicker(narrative string) string {
	return derefText(tickerFromDescription(strings.TrimSpace(narrative)))
}

// webDividendNarrative is the narrative a web dividend row names its
// security in, as silver states it: a statement's own continuation
// lines, or the export's Description1.
func webDividendNarrative(p webTxPayload, pdfBackfill bool) string {
	if !pdfBackfill {
		return p.Description1
	}
	own, _ := bookingLines(p.Continuation)
	return strings.Join(own, "\n")
}

// minTickerLen is the shortest token road 3 reads as a ticker. A one- or
// two-letter token in parentheses is as often a share class or a
// domicile, `(A)` or `(IE)`, and nothing tells the two apart.
const minTickerLen = 3

// dividendSecurities is what roads 2 and 3 resolve a dividend line
// against.
type dividendSecurities struct {
	// valors is the valor index (buildValorIndex).
	valors map[string]string
	// tickers maps a portfolio onto the ISIN its custody positions print
	// each ticker for: "" where two ISINs print one ticker.
	tickers map[string]map[string]string
	// currencies are the currencies the statements of assets print. A
	// token that is one names a share class, not a security.
	currencies map[string]bool
	// portfolios maps a cash account onto the portfolios the account
	// master data and the printed statements place it in.
	portfolios map[string][]string
	// credits are the dividend credits any rail states, each day's per
	// rail (addCredit): the evidence a reversal of a credit reverses a
	// dividend, and the credits a reversal takes back (matchReversals).
	credits map[creditKey]map[int64]railCounts
	// reversals maps each MT940 reversal of a dividend credit onto the
	// credit it takes back (matchReversals).
	reversals map[string]takenCredit
	// booked are the dividend credits gold books at a gross, each the
	// gross it reads: the export rows the web pass emits
	// (recordExportGrosses), the MT940 lines that pair with a confirmation
	// stating one, and the cash legs a confirmation books itself
	// (addFeedGrosses). A reversal of one takes back that gross
	// (reversedGross).
	booked map[creditKey][]bookedGross
}

// resolve finds the security a dividend line on account names: by the
// valor it states, else by the ticker closing its narrative. A stated
// valor neither road resolves comes back as the hint.
func (d *dividendSecurities) resolve(account, valor, narrative string) (id *string, hint string) {
	if id, hint = resolveValor(d.valors, valor); id != nil {
		return id, ""
	}
	if isin, ok := d.byTicker(account, closingTicker(narrative)); ok {
		return &isin, ""
	}
	return nil, hint
}

// byTicker is the one ISIN the custody positions of the account's
// portfolios print the ticker for. false for no ticker, a token too short
// or a currency, no portfolio, or two ISINs.
func (d *dividendSecurities) byTicker(account, ticker string) (string, bool) {
	if len(ticker) < minTickerLen || d.currencies[ticker] {
		return "", false
	}
	var isin string
	for _, portfolio := range d.portfolios[account] {
		id, ok := d.tickers[portfolio][ticker]
		if !ok {
			continue
		}
		if id == "" || isin != "" && id != isin {
			return "", false
		}
		isin = id
	}
	return isin, isin != ""
}

// buildDividendSecurities reads what roads 2 and 3 need: the valor index
// the caller holds, the tickers the printed statements' custody positions
// print, and the portfolios each cash account belongs to. Then it reads
// every dividend credit both silvers state, and pairs each reversal of a
// credit with the credit it takes back. A nil or absent side contributes
// nothing.
func buildDividendSecurities(ctx context.Context, psn *psnReader, web *webReader, valors map[string]string) (*dividendSecurities, error) {
	d := &dividendSecurities{
		valors:     valors,
		tickers:    map[string]map[string]string{},
		currencies: map[string]bool{},
		portfolios: map[string][]string{},
		credits:    map[creditKey]map[int64]railCounts{},
		reversals:  map[string]takenCredit{},
		booked:     map[creditKey][]bookedGross{},
	}
	if err := web.addCustodyTickers(ctx, d); err != nil {
		return nil, err
	}
	portfolioOf, err := psn.portfolioOfCashAccount(ctx)
	if err != nil {
		return nil, err
	}
	for account, portfolio := range portfolioOf {
		d.addPortfolio(account, portfolio)
	}
	// The credits resolve their security by the roads above, so they
	// are read last.
	if err := d.addFeedCredits(ctx, psn); err != nil {
		return nil, err
	}
	if err := d.addWebCredits(ctx, web); err != nil {
		return nil, err
	}
	if err := d.matchReversals(ctx, psn); err != nil {
		return nil, err
	}
	return d, nil
}

// addTicker records that a custody position of the portfolio prints the
// ticker for the ISIN. A second ISIN for the ticker leaves it naming
// none.
func (d *dividendSecurities) addTicker(portfolio, ticker, isin string) {
	byTicker := d.tickers[portfolio]
	if byTicker == nil {
		byTicker = map[string]string{}
		d.tickers[portfolio] = byTicker
	}
	if held, ok := byTicker[ticker]; ok && held != isin {
		isin = ""
	}
	byTicker[ticker] = isin
}

// addPortfolio places a cash account in a portfolio, once.
func (d *dividendSecurities) addPortfolio(account, portfolio string) {
	if account == "" || portfolio == "" {
		return
	}
	for _, p := range d.portfolios[account] {
		if p == portfolio {
			return
		}
	}
	d.portfolios[account] = append(d.portfolios[account], portfolio)
}

// addCustodyTickers reads the printed statements of assets: the ticker
// closing each custody position's description, under its portfolio; each
// cash position's account under the same portfolio; and every currency
// they print. One table names all three, so they agree on how a
// portfolio is spelled.
func (r *webReader) addCustodyTickers(ctx context.Context, d *dividendSecurities) error {
	if r == nil || r.db == nil {
		return nil
	}
	ok, err := r.hasTable(ctx, "historical_position_snapshots")
	if err != nil || !ok {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT portfolio_external_id, account_external_id, instrument_isin,
       description, currency_iso, market_value_currency
  FROM historical_position_snapshots`)
	if err != nil {
		return fmt.Errorf("ubs-web addCustodyTickers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var portfolio, account, isin, description, ccy, valuedIn sql.NullString
		if err := rows.Scan(&portfolio, &account, &isin, &description, &ccy, &valuedIn); err != nil {
			return fmt.Errorf("ubs-web addCustodyTickers scan: %w", err)
		}
		for _, c := range []string{ccy.String, valuedIn.String} {
			if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
				d.currencies[c] = true
			}
		}
		if !isin.Valid {
			d.addPortfolio(account.String, portfolio.String)
			continue
		}
		if ticker := closingTicker(description.String); ticker != "" && looksLikeISIN(isin.String) {
			d.addTicker(portfolio.String, ticker, isin.String)
		}
	}
	return rows.Err()
}

// dividendConfirmations maps an MT940 dividend line's event id onto the
// confirmation whose cash leg it is (road 1).
type dividendConfirmations map[string]cashConfirmation

// buildDividendConfirmations pairs the feed's dividend lines with the
// confirmations of the cash they credited, on the four facts both state:
// the cash account, the payment day, the amount and its currency.
//
// Only a credit pairs: a confirmation states a credit, and a reversal's
// mark would read the wrong way round. A pair holds where one line and
// one confirmation's statement answer the key (pairOf). Two
// confirmations that state the same security and figures are one
// statement, such as a confirmation the bank sent again; any other second
// answer on either side leaves the key unpaired, and its lines take the
// other roads.
//
// The whole silver, unwindowed, for the settlement fold's reason: which
// line pairs with which confirmation depends on silver's contents, never
// on the slice of time a load covers.
func (r *psnReader) buildDividendConfirmations(ctx context.Context, ibans map[string]string) (dividendConfirmations, error) {
	all, err := r.cashConfirmations(ctx)
	if err != nil {
		return nil, err
	}
	confirmations := map[bookingKey][]cashConfirmation{}
	for _, c := range all {
		posted := c.move.posted
		if k, ok := newBookingKey(cashIBAN(ibans, c.move.cashAccount), c.move.paymentDay, &posted, c.move.postedCcy); ok {
			confirmations[k] = append(confirmations[k], c)
		}
	}
	out := dividendConfirmations{}
	if len(confirmations) == 0 {
		return out, nil
	}
	lines := map[bookingKey][]string{}
	err = r.eachCashMovement(ctx, "ubs buildDividendConfirmations", func(row psnCashRow) error {
		// The cash the line credits, as the stream projects it
		// (psnCashRow.project).
		tx, _, ok := row.dividendCredit()
		if !ok {
			return nil
		}
		if k, ok := txBookingKey(tx); ok {
			lines[k] = append(lines[k], row.eventID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for k, ids := range lines {
		c, ok := pairOf(confirmations[k])
		if ok && len(ids) == 1 {
			out[ids[0]] = c
		}
	}
	return out, nil
}

// pairOf is the confirmation a line answering the key pairs with: the one
// statement of those that stand (oneStatement). Where none stands, it is
// the first by id of those withdrawn, where they name one security
// (oneSecurity): it names the line's security and nothing else
// (psnDividends.settle).
func pairOf(cs []cashConfirmation) (cashConfirmation, bool) {
	var standing, withdrawn []cashConfirmation
	for _, c := range cs {
		if c.stands {
			standing = append(standing, c)
		} else {
			withdrawn = append(withdrawn, c)
		}
	}
	if len(standing) > 0 {
		return oneStatement(standing)
	}
	return oneSecurity(withdrawn)
}

// oneStatement is the confirmation that answers a key: the one there is,
// or the first by id of several that state the same security and the
// same gross for the cash the key holds (statedGross). false for none, or
// for two that differ.
func oneStatement(cs []cashConfirmation) (cashConfirmation, bool) {
	return firstAgreeing(cs, func(c, first cashConfirmation) bool {
		return c.isin == first.isin && c.statedGross() == first.statedGross()
	})
}

// oneSecurity is the first by id of the confirmations, where all name one
// security. false for none, or for two securities.
func oneSecurity(cs []cashConfirmation) (cashConfirmation, bool) {
	return firstAgreeing(cs, func(c, first cashConfirmation) bool { return c.isin == first.isin })
}

// firstAgreeing is the first by id of cs, where every other agrees with
// it. false for none, or for one that does not.
func firstAgreeing(cs []cashConfirmation, agrees func(c, first cashConfirmation) bool) (cashConfirmation, bool) {
	if len(cs) == 0 {
		return cashConfirmation{}, false
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].eventID < cs[j].eventID })
	for _, c := range cs[1:] {
		if !agrees(c, cs[0]) {
			return cashConfirmation{}, false
		}
	}
	return cs[0], true
}

// reversalOf is the security an MT940 reversal of a credit names by road 2
// or 3, where it may reverse a dividend credit, and whether the line says
// so itself: a `DIV…` narrative, or the `NDIV` type code. A line that does
// not needs a dividend credit to take back (matchReversals).
//
// false for any other line: one not marked `RC`; one whose narrative
// places it as another kind than a dividend, or, where the narrative
// places none, whose type code does (an interest, a fee, a tax, a trade's
// `NSEC`); and one that names no security either road resolves. The
// narrative outranks the type code, as in the kind map. tx is the line as
// buildTransaction projects it.
func (d *dividendSecurities) reversalOf(tx canonical.TransactionChange, p cashMovementPayload) (isin string, saysDividend, ok bool) {
	if !isCreditReversal(p.CreditDebit) || tx.NetAmount == nil {
		return "", false, false
	}
	if tx.Kind != canonical.TxKindWithdrawal && tx.Kind != canonical.TxKindDividend {
		return "", false, false
	}
	code, coded := txnTypeKind(p.TxnType, "C")
	if coded && code != canonical.TxKindDividend && tx.Kind != canonical.TxKindDividend {
		return "", false, false
	}
	id, _ := d.resolve(tx.AccountExternalID, statedValor(p.CustomerRef), p.Narrative)
	if id == nil {
		return "", false, false
	}
	return *id, tx.Kind == canonical.TxKindDividend || coded, true
}

// creditReversal is the security whose dividend the MT940 reversal of a
// credit with the event id takes back (matchReversals). A reversed
// redemption, coupon or capital repayment neither says it is a dividend
// nor finds a dividend credit to take back, and stays as the kind map
// reads it.
func (d *dividendSecurities) creditReversal(eventID string) (*string, bool) {
	t, ok := d.reversals[eventID]
	if !ok {
		return nil, false
	}
	isin := t.isin
	return &isin, true
}

// creditKey is one dividend credit, as a reversal's evidence: the
// security, the account, the currency and the amount credited.
type creditKey struct{ isin, account, currency, amount string }

// newCreditKey is the key of a credit of amount, a magnitude; false where
// it names no security or the amount has no cents.
func newCreditKey(isin, account, currency string, amount canonical.Decimal) (creditKey, bool) {
	amount = amount.Abs()
	cents, ok := bookingCents(&amount)
	if isin == "" || !ok {
		return creditKey{}, false
	}
	return creditKey{isin: isin, account: account, currency: bookingCurrency(currency), amount: cents}, true
}

// creditRail is a rail a dividend credit is recorded on. One credit can
// stand on several: its confirmation, its MT940 line, its export row and
// its statement copy.
type creditRail int

const (
	railConfirmation creditRail = iota
	railFeed
	railExport
	railStatement
	creditRails
)

// railCounts is how many credits of one key each rail records on one day.
type railCounts [creditRails]int

// standing is how many credits stand: as many as the rail recording the
// most. A credit recorded on two rails is one credit.
func (n railCounts) standing() int {
	most := 0
	for _, c := range n {
		most = max(most, c)
	}
	return most
}

// bookedGross is a dividend credit gold books at a gross: its day and the
// gross, a magnitude.
type bookedGross struct {
	day   int64
	gross canonical.Decimal
}

// addCredit records a dividend credit of the security on the day, on the
// rail.
func (d *dividendSecurities) addCredit(rail creditRail, isin, account, currency string, amount canonical.Decimal, at int64) {
	k, ok := newCreditKey(isin, account, currency, amount)
	if !ok {
		return
	}
	days := d.credits[k]
	if days == nil {
		days = map[int64]railCounts{}
		d.credits[k] = days
	}
	n := days[utcDay(at)]
	n[rail]++
	days[utcDay(at)] = n
}

// addBooked records that gold books the dividend credit of amount on the
// day at gross.
func (d *dividendSecurities) addBooked(isin, account, currency string, amount canonical.Decimal, at int64, gross canonical.Decimal) {
	if k, ok := newCreditKey(isin, account, currency, amount); ok {
		d.booked[k] = append(d.booked[k], bookedGross{day: utcDay(at), gross: gross})
	}
}

// takenCredit is the dividend credit a reversal takes back: its key and
// its day. found is false for a reversal that says it reverses a dividend
// where no credit stands to take back; it takes back the amount it moves.
type takenCredit struct {
	isin  string
	key   creditKey
	day   int64
	found bool
}

// matchReversals pairs each MT940 reversal of a dividend credit
// (reversalOf) with the credit it takes back, one to one and in time
// order: the newest credit of its security, account, currency and amount
// on or before its day that no earlier reversal took back. Two reversals
// on one day go in id order. A reversal that finds no credit is a dividend
// reversal only where its line says so.
//
// The whole silver, unwindowed, for the settlement fold's reason: which
// credit a reversal takes back depends on silver's contents, never on the
// slice of time a load covers.
func (d *dividendSecurities) matchReversals(ctx context.Context, r *psnReader) error {
	type reversal struct {
		id, isin    string
		day         int64
		key         creditKey
		keyed, says bool
	}
	var reversals []reversal
	err := r.eachCashMovement(ctx, "ubs matchReversals", func(row psnCashRow) error {
		tx, p, ok := row.project()
		if !ok {
			return nil
		}
		isin, says, ok := d.reversalOf(tx, p)
		if !ok {
			return nil
		}
		k, keyed := newCreditKey(isin, tx.AccountExternalID, tx.Currency, *tx.NetAmount)
		reversals = append(reversals, reversal{
			id: row.eventID, isin: isin, day: utcDay(tx.OccurredAt), key: k, keyed: keyed, says: says,
		})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(reversals, func(i, j int) bool {
		if reversals[i].day != reversals[j].day {
			return reversals[i].day < reversals[j].day
		}
		return reversals[i].id < reversals[j].id
	})
	taken := map[creditKey]map[int64]int{}
	for _, rev := range reversals {
		t := takenCredit{isin: rev.isin, key: rev.key}
		if rev.keyed {
			t.day, t.found = d.takeCredit(rev.key, rev.day, taken)
		}
		if t.found || rev.says {
			d.reversals[rev.id] = t
		}
	}
	return nil
}

// takeCredit takes back the newest credit of the key on or before the day
// that taken does not hold yet, and records it there. false where none is
// left.
func (d *dividendSecurities) takeCredit(k creditKey, day int64, taken map[creditKey]map[int64]int) (int64, bool) {
	var days []int64
	for at := range d.credits[k] {
		if at <= day {
			days = append(days, at)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i] > days[j] })
	for _, at := range days {
		if taken[k][at] < d.credits[k][at].standing() {
			if taken[k] == nil {
				taken[k] = map[int64]int{}
			}
			taken[k][at]++
			return at, true
		}
	}
	return 0, false
}

// reversedGross is the gross the credit a reversal takes back
// (matchReversals) was booked at, where gold books it at one. nil where it
// keeps the amount credited, and where two credits of that day read two
// grosses: which one is reversed is not known.
func (d *dividendSecurities) reversedGross(reversalID string) *canonical.Decimal {
	t, ok := d.reversals[reversalID]
	if !ok || !t.found {
		return nil
	}
	var gross *canonical.Decimal
	for _, c := range d.booked[t.key] {
		switch {
		case c.day != t.day:
		case gross == nil:
			g := c.gross
			gross = &g
		case !gross.Equal(c.gross):
			return nil
		}
	}
	return gross
}

// addFeedCredits records the dividend credits the feed states: the
// posted cash of each dividend confirmation that stands, once however
// often the bank sent it (repeats), and of each one a reversal withdraws,
// for the bank paid it before it took it back; and each MT940 dividend
// credit a road names the security of. The amount is the one credited,
// which is what a reversal line moves.
func (d *dividendSecurities) addFeedCredits(ctx context.Context, r *psnReader) error {
	if r == nil || r.db == nil {
		return nil
	}
	ibans, err := r.cashAccountIBANs(ctx)
	if err != nil {
		return err
	}
	confirmations, err := r.cashConfirmations(ctx)
	if err != nil {
		return err
	}
	for _, c := range confirmations {
		kind, ok := corporateActionCashKind(c.caev)
		if ok && kind == canonical.TxKindDividend && (c.stands && !c.repeats || c.reversed) {
			d.addCredit(railConfirmation, c.isin, cashIBAN(ibans, c.move.cashAccount), c.move.postedCcy, c.move.posted, c.move.paymentDay)
		}
	}
	return r.eachCashMovement(ctx, "ubs addFeedCredits", func(row psnCashRow) error {
		tx, p, ok := row.dividendCredit()
		if !ok {
			return nil
		}
		if id, _ := d.resolve(tx.AccountExternalID, statedValor(p.CustomerRef), p.Narrative); id != nil {
			d.addCredit(railFeed, *id, tx.AccountExternalID, tx.Currency, *tx.NetAmount, tx.OccurredAt)
		}
		return nil
	})
}

// addWebCredits records the dividend credits the web silver states: each
// export or statement dividend that credits, under the security it names
// (webDividendSecurity).
func (d *dividendSecurities) addWebCredits(ctx context.Context, r *webReader) error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.eachWebTx(ctx, "ubs-web addWebCredits", "", nil, func(row webTxRow) error {
		net, ok := row.dividendCredit()
		if !ok {
			return nil
		}
		rail := railExport
		if isStatementEraID(row.txID) {
			rail = railStatement
		}
		pl, pdfBackfill := decodeWebTxEra(row.payload)
		if id, _ := d.webDividendSecurity(row.account, pl, pdfBackfill); id != nil {
			d.addCredit(rail, *id, row.account, row.currency, *net, row.valueDate)
		}
		return nil
	})
}

// webDividendSecurity is the security a web dividend row names: the ISIN
// its Description1 states, else roads 2 and 3 over its narrative
// (webDividendNarrative), else the valor the statement parser promoted.
// The hint is a stated valor no road resolves.
func (d *dividendSecurities) webDividendSecurity(account string, pl webTxPayload, pdfBackfill bool) (*string, string) {
	if id, _ := extractInstrumentFromDescription1(pl); id != nil {
		return id, ""
	}
	narrative := webDividendNarrative(pl, pdfBackfill)
	if id, hint := d.resolve(account, statedValor(narrative), narrative); id != nil || hint != "" {
		return id, hint
	}
	return resolveValor(d.valors, pl.SecurityValor)
}

// psnDividends is what the feed's transaction stream links a dividend
// line by and books its gross from.
type psnDividends struct {
	confirmations dividendConfirmations
	securities    *dividendSecurities
}

// settle links an MT940 dividend line to its security and, where its
// confirmation states the gross, books it gross. It returns the
// withholding row to book beside it, where one was withheld.
//
// The line's payload is left as the feed wrote it, so the amount the
// bank credited stays readable there; a line paired with a confirmation
// that stands also names it. One withdrawn names the line's security and
// nothing else. p is the payload decoded.
func (d psnDividends) settle(tx *canonical.TransactionChange, p cashMovementPayload) (tax canonical.TransactionChange, withheld bool, err error) {
	c, paired := d.link(tx, p)
	if !paired || !c.stands {
		return canonical.TransactionChange{}, false, nil
	}
	tx.Payload = spliceStringField(string(tx.Payload), corporateActionKey, c.eventID)
	return bookGross(tx, c)
}

// link names the security of an MT940 dividend line: its confirmation's,
// where the line pairs with one, else roads 2 and 3. A pair is four facts
// that agree, which two securities paying the same amount on one day can
// also do. A valor the line states that names another security than the
// confirmation says the pair is not this line's. A confirmation that names
// no security contradicts nothing.
func (d psnDividends) link(tx *canonical.TransactionChange, p cashMovementPayload) (cashConfirmation, bool) {
	valor := statedValor(p.CustomerRef)
	c, paired := d.confirmations[tx.TransactionExternalID]
	if stated, _ := resolveValor(d.securities.valors, valor); paired && stated != nil && c.isin != "" && *stated != c.isin {
		paired = false
	}
	if paired && c.isin != "" {
		isin := c.isin
		tx.InstrumentExternalID = &isin
	} else {
		tx.InstrumentExternalID, tx.InstrumentHint = d.securities.resolve(tx.AccountExternalID, valor, p.Narrative)
	}
	return c, paired
}

// addFeedGrosses records, over the whole feed, every dividend credit the
// stream books at a gross: an MT940 line at its confirmation's (settle),
// and a confirmation's own cash leg (cashLeg). Each is decided by the
// stream's own path, run on a copy, so the record and the stream agree on
// every credit. Neither path gives a dividend a gross amount but where it
// books the gross (bookAtGross). A reversal in any window takes that
// gross back (reversedGross).
func (d psnDividends) addFeedGrosses(ctx context.Context, r *psnReader, coverage psnCashCoverage, ibans map[string]string) error {
	confirmations, err := r.cashConfirmations(ctx)
	if err != nil {
		return err
	}
	for _, c := range confirmations {
		tx, _, _, ok, err := c.cashLeg(coverage, ibans)
		if err != nil {
			return err
		}
		if ok && tx.Kind == canonical.TxKindDividend && tx.GrossAmount != nil && tx.InstrumentExternalID != nil {
			d.securities.addBooked(*tx.InstrumentExternalID, tx.AccountExternalID, tx.Currency, c.move.posted, tx.OccurredAt, *tx.GrossAmount)
		}
	}
	if len(d.confirmations) == 0 {
		return nil
	}
	return r.eachCashMovement(ctx, "ubs addFeedGrosses", func(row psnCashRow) error {
		if c, ok := d.confirmations[row.eventID]; !ok || !c.stands {
			return nil
		}
		tx, p, ok := row.dividendCredit()
		if !ok {
			return nil
		}
		credited := *tx.NetAmount
		if _, _, err := d.settle(&tx, p); err != nil || tx.GrossAmount == nil || tx.InstrumentExternalID == nil {
			return err
		}
		d.securities.addBooked(*tx.InstrumentExternalID, tx.AccountExternalID, tx.Currency, credited, tx.OccurredAt, *tx.GrossAmount)
		return nil
	})
}

// reverse reads an MT940 reversal of a dividend credit as that security's
// dividend reversal (creditReversal): kind dividend, the instrument
// linked. Its amount is already negative (buildTransaction). Where the
// credit it takes back was booked at a gross, it takes back the gross,
// and returns the tax taken back with it as the row to book beside it
// (bookAtGross). reversed false leaves the line as the kind map read it.
func (d psnDividends) reverse(tx *canonical.TransactionChange) (tax canonical.TransactionChange, withheld, reversed bool, err error) {
	id, ok := d.securities.creditReversal(tx.TransactionExternalID)
	if !ok {
		return canonical.TransactionChange{}, false, false, nil
	}
	tx.Kind = canonical.TxKindDividend
	tx.InstrumentExternalID, tx.InstrumentHint = id, ""
	gross := d.securities.reversedGross(tx.TransactionExternalID)
	if gross == nil {
		return canonical.TransactionChange{}, false, true, nil
	}
	tax, withheld, err = bookAtGross(tx, *gross, "", "")
	return tax, withheld, true, err
}

// dividends is what the feed's stream links and books a dividend line
// by. Without the web pass's securities (psnHints), the feed's own valor
// index answers roads 2 and 3, and no ticker does.
func (r *psnReader) dividends(ctx context.Context, ibans map[string]string, coverage psnCashCoverage, securities *dividendSecurities) (psnDividends, error) {
	confirmations, err := r.buildDividendConfirmations(ctx, ibans)
	if err != nil {
		return psnDividends{}, err
	}
	if securities == nil {
		valors, err := buildValorIndex(ctx, r, nil)
		if err != nil {
			return psnDividends{}, err
		}
		if securities, err = buildDividendSecurities(ctx, r, nil, valors); err != nil {
			return psnDividends{}, err
		}
	}
	d := psnDividends{confirmations: confirmations, securities: securities}
	if err := d.addFeedGrosses(ctx, r, coverage, ibans); err != nil {
		return psnDividends{}, err
	}
	return d, nil
}
