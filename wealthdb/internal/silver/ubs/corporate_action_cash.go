package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A corporate action that pays cash — a dividend, a coupon — reaches
// PSN twice: as the MT566 confirmation on the custody account, and as
// the MT940 line crediting the cash account. Where the feed speaks for
// the cash account, that line is the row gold holds (`dividend`, by
// narrative or by the NDIV floor) and the confirmation stands beside it
// as a `corporate_action` marker with no amount. Where it does not —
// the mandates' minor-currency cash accounts — the confirmation is the
// only record there is, and its CASHMOVE block states what the
// statement line would have:
//
//	:22H::CRDB//CRED          credited
//	:97A::CASH//<account>     the cash account, in the bank's internal form
//	:19B::PSTA//JPY1234560,   the amount posted
//	:19B::GRSS//JPY1234560,   the gross entitlement
//	:19B::TAXR//JPY0,         the tax withheld
//	:98A::PAYD//20260818      the payment day
//
// So for such an account the adapter books the cash leg from the
// confirmation, beside the marker: one `dividend` (or `interest`) row
// on the cash account, on the payment day, with the confirmation's
// figures in its payload. Interest reads the posted amount. A dividend
// reads its gross, with the tax withheld as a `tax` row beside it, as
// an MT940 line paired with its confirmation does (dividends.go).
// Only where the feed does not speak for the account on that day
// (psnCashCoverage) — where it does, the statement line is the row,
// and a second one would double the income.
//
// Only the events that pay cash are read (CAOP CASH, a credit), and
// only the two whose kind is settled by the event indicator: DVCA is a
// dividend, INTR is interest. A capital call confirmed as OTHR, an
// optional dividend taken in shares, an exchange — anything else — is
// left to the rails that already carry it. The cash option's own
// confirmation block (CACONF) states the movement and the rates; another
// option's block, such as the shares a choice of dividend delivered,
// says nothing about the cash.
//
// A confirmation's message function (`:23G:`) says whether its statement
// holds. NEWM is a new confirmation. CANC cancels one the bank sent
// before, and REVR reverses one whose payment the bank took back. Either
// can repeat the figures of the confirmation it withdraws, so read as new
// it would book that cash a second time. Only a confirmation that
// stands books a cash leg or states a gross (withdrawals). Every
// confirmation, a withdrawn one and a cancellation included, keeps its
// marker: it moves no money, and it is the record of what the bank sent.

// corporateActionFields is the MT566 confirmation as the collector
// stores it: the tag sequence verbatim, and the fields it promoted.
type corporateActionFields struct {
	CAEV   string      `json:"caev"`
	ISIN   string      `json:"isin"`
	Fields [][2]string `json:"fields"`
}

// caExchange is the exchange rate a confirmation states
// (`:92B::EXCH//<from>/<to>/<rate>`): one unit of from is rate units of
// to.
type caExchange struct {
	from, to string
	rate     canonical.Decimal
}

// caCashMove is what the cash option's CASHMOVE block states, and the
// rates and the balance the confirmation states beside it.
type caCashMove struct {
	credit                      bool
	cashAccount                 string
	posted, gross, tax          canonical.Decimal
	postedCcy, grossCcy, taxCcy string
	paymentDay                  int64
	// The rates a gross follows from where no amount states it in the
	// currency credited (rateGross): the gross rate per share
	// (`:92F::GRSS//`), the withholding rate in percent (`:92A::TAXR//`)
	// and the exchange rate (`:92B::EXCH//`), each from the cash option's
	// block; and the confirmed balance the event paid on
	// (`:93B::CONB//UNIT/`). nil where unstated.
	grossRate, taxRate, quantity *canonical.Decimal
	grossRateCcy                 string
	exchange                     *caExchange
}

// caMessage is what one confirmation states: its own reference, its
// message function and the confirmation it names in its linkage block,
// the event and the custody account it is for, its cash option and its
// cash movement.
type caMessage struct {
	seme, function, prev string
	corp, safekeeping    string
	name                 string
	payingCash           bool
	hasPosted            bool
	move                 caCashMove
}

// swiftMoney splits a `:QUAL//CCYamount` value into its currency and
// figure. The figure keeps SWIFT's spelling; parseSwiftDecimal reads it.
func swiftMoney(value string) (currency string, amount canonical.Decimal, ok bool) {
	_, rest, found := strings.Cut(value, "//")
	if !found || len(rest) < 4 {
		return "", canonical.Decimal{}, false
	}
	currency, figure := rest[:3], rest[3:]
	amount, err := parseSwiftDecimal(figure)
	if err != nil {
		return "", canonical.Decimal{}, false
	}
	return currency, amount, true
}

// swiftRate reads a `:QUAL//rate` value. A negative rate (`N` before the
// figure) reads as none.
func swiftRate(value string) *canonical.Decimal {
	_, rest, found := strings.Cut(value, "//")
	if !found {
		return nil
	}
	rate, err := parseSwiftDecimal(rest)
	if err != nil {
		return nil
	}
	return &rate
}

// swiftExchange reads a `:QUAL//<from>/<to>/<rate>` value. A rate that is
// not positive reads as none.
func swiftExchange(value string) *caExchange {
	_, rest, _ := strings.Cut(value, "//")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || len(parts[0]) != 3 || len(parts[1]) != 3 {
		return nil
	}
	rate, err := parseSwiftDecimal(parts[2])
	if err != nil || !rate.IsPositive() {
		return nil
	}
	return &caExchange{from: parts[0], to: parts[1], rate: rate}
}

// swiftUnits reads a `:QUAL//UNIT/quantity` value: a number of units. A
// face amount (FAMT) or an amortised value is no number of shares.
func swiftUnits(value string) *canonical.Decimal {
	_, rest, _ := strings.Cut(value, "//")
	kind, figure, found := strings.Cut(rest, "/")
	if !found || kind != "UNIT" {
		return nil
	}
	units, err := parseSwiftDecimal(figure)
	if err != nil {
		return nil
	}
	return &units
}

// swiftDay reads a `:QUAL//YYYYMMDD` date as the Unix time of that day.
func swiftDay(value string) (int64, bool) {
	_, rest, found := strings.Cut(value, "//")
	if !found || len(rest) < 8 {
		return 0, false
	}
	t, err := time.Parse("20060102", rest[:8])
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// qualifier is the `:QUAL//` a SWIFT field value opens with.
func qualifier(value string) string {
	q, _, _ := strings.Cut(strings.TrimPrefix(value, ":"), "//")
	return q
}

// caOption is one confirmation block (CACONF): whether it confirms the
// cash option, and what it states.
type caOption struct {
	cash, hasPosted bool
	move            caCashMove
}

// read takes one field of the block. inMove says it sits in the block's
// CASHMOVE. Of each rate, the first stated is read.
func (o *caOption) read(tag, value string, inMove bool) {
	m := &o.move
	switch {
	case tag == "22H" && qualifier(value) == "CAOP":
		o.cash = strings.HasSuffix(value, "//CASH")
	case tag == "92F" && qualifier(value) == "GRSS" && m.grossRate == nil:
		if ccy, rate, ok := swiftMoney(value); ok {
			m.grossRateCcy, m.grossRate = ccy, &rate
		}
	case tag == "92A" && qualifier(value) == "TAXR" && m.taxRate == nil:
		m.taxRate = swiftRate(value)
	case tag == "92B" && qualifier(value) == "EXCH" && m.exchange == nil:
		m.exchange = swiftExchange(value)
	case !inMove:
	case tag == "22H" && qualifier(value) == "CRDB":
		m.credit = strings.HasSuffix(value, "//CRED")
	case tag == "97A" && qualifier(value) == "CASH":
		_, m.cashAccount, _ = strings.Cut(value, "//")
	case tag == "19B":
		ccy, amount, ok := swiftMoney(value)
		if !ok {
			return
		}
		switch qualifier(value) {
		case "PSTA":
			m.postedCcy, m.posted, o.hasPosted = ccy, amount, true
		case "GRSS":
			m.grossCcy, m.gross = ccy, amount
		case "TAXR":
			m.taxCcy, m.tax = ccy, amount
		}
	case tag == "98A" && (qualifier(value) == "PAYD" || qualifier(value) == "VALU"):
		// The payment day outranks the value day; either names the day
		// the cash arrived, and both are usually the same.
		if day, ok := swiftDay(value); ok && (m.paymentDay == 0 || qualifier(value) == "PAYD") {
			m.paymentDay = day
		}
	}
}

// parseCorporateAction reads a confirmation's tag sequence: its general
// block, its underlying securities, and the first block that confirms the
// cash option. The message function drops its subfunction: `NEWM/DUPL`
// is a new confirmation.
func parseCorporateAction(f corporateActionFields) caMessage {
	var (
		m                        caMessage
		option                   caOption
		inOption, inMove, inLink bool
		balance                  *canonical.Decimal
	)
	closeOption := func() {
		if option.cash && !m.payingCash {
			m.payingCash, m.hasPosted, m.move = true, option.hasPosted, option.move
		}
		option, inOption = caOption{}, false
	}
	for _, field := range f.Fields {
		tag, value := field[0], field[1]
		switch {
		case tag == "16R" || tag == "16S":
			opens := tag == "16R"
			switch value {
			case "CACONF":
				if opens {
					option, inOption = caOption{}, true
				} else {
					closeOption()
				}
			case "CASHMOVE":
				inMove = opens
			case "LINK":
				inLink = opens
			}
		case inOption:
			option.read(tag, value, inMove)
		case tag == "23G":
			m.function, _, _ = strings.Cut(strings.ToUpper(strings.TrimSpace(value)), "/")
		case tag == "20C":
			_, ref, _ := strings.Cut(value, "//")
			ref = strings.TrimSpace(ref)
			switch q := qualifier(value); {
			case q == "SEME":
				m.seme = ref
			case q == "CORP":
				m.corp = ref
			case q == "PREV" && inLink:
				m.prev = ref
			}
		case tag == "97A" && qualifier(value) == "SAFE" && m.safekeeping == "":
			_, safe, _ := strings.Cut(value, "//")
			m.safekeeping = strings.TrimSpace(safe)
		case tag == "93B" && qualifier(value) == "CONB" && balance == nil:
			balance = swiftUnits(value)
		case tag == "35B" && m.name == "":
			// "ISIN <isin>\n<name>\n…": the security's name is the
			// line after the identifier.
			lines := strings.Split(value, "\n")
			if len(lines) > 1 {
				m.name = strings.TrimSpace(lines[1])
			}
		}
	}
	if inOption {
		closeOption()
	}
	m.move.quantity = balance
	return m
}

// creditsCash reports whether the confirmation pays cash and states the
// movement completely enough to book: a credit, the cash account and a
// non-zero amount posted.
func (m caMessage) creditsCash() bool {
	return m.payingCash && m.move.credit && m.move.cashAccount != "" && m.hasPosted && !m.move.posted.IsZero()
}

// isNewConfirmation reports whether a message function makes a new
// confirmation: NEWM, or none stated.
func isNewConfirmation(function string) bool { return function == "" || function == "NEWM" }

// isWithdrawal reports whether a message function withdraws a
// confirmation: CANC cancels one, REVR reverses one.
func isWithdrawal(function string) bool { return function == "CANC" || function == "REVR" }

// caRow is one confirmation event as silver holds it, read.
type caRow struct {
	eventID string
	at      int64
	f       corporateActionFields
	m       caMessage
}

// withdrawalKey is what a cancellation or a reversal that names no
// confirmation is matched on: the event, the custody account, the
// security and the amount posted.
type withdrawalKey struct{ corp, safekeeping, isin, currency, posted string }

// withdrawalKey is the row's key; false where it states no event
// reference or no amount posted.
func (row caRow) withdrawalKey() (withdrawalKey, bool) {
	posted := row.m.move.posted
	cents, ok := bookingCents(&posted)
	if row.m.corp == "" || !row.m.hasPosted || !ok {
		return withdrawalKey{}, false
	}
	return withdrawalKey{
		corp: row.m.corp, safekeeping: row.m.safekeeping, isin: strings.TrimSpace(row.f.ISIN),
		currency: bookingCurrency(row.m.move.postedCcy), posted: cents,
	}, true
}

// withdrawals maps each new confirmation a cancellation or a reversal
// withdraws onto the function that withdraws it, over rows in id order.
//
// One that names the confirmation it withdraws (`:20C::PREV//<SEME>` in
// its linkage block) withdraws that one, and no other where silver holds
// none of that reference. One that names none withdraws one new
// confirmation of its withdrawalKey that nothing withdrew yet: the newest
// at or before its own time, else the first by id. Those go in time order.
// A cancellation or a reversal that names a cancellation or a reversal
// withdraws nothing, and restores nothing.
func withdrawals(rows []caRow) map[string]string {
	bySEME := map[string][]string{}
	byKey := map[withdrawalKey][]caRow{}
	var named, unnamed []caRow
	for _, row := range rows {
		switch {
		case isNewConfirmation(row.m.function):
			if row.m.seme != "" {
				bySEME[row.m.seme] = append(bySEME[row.m.seme], row.eventID)
			}
			if k, ok := row.withdrawalKey(); ok {
				byKey[k] = append(byKey[k], row)
			}
		case !isWithdrawal(row.m.function):
		case row.m.prev != "":
			named = append(named, row)
		default:
			unnamed = append(unnamed, row)
		}
	}
	out := map[string]string{}
	for _, row := range named {
		for _, id := range bySEME[row.m.prev] {
			out[id] = row.m.function
		}
	}
	sort.SliceStable(unnamed, func(i, j int) bool { return unnamed[i].at < unnamed[j].at })
	for _, row := range unnamed {
		k, ok := row.withdrawalKey()
		if !ok {
			continue
		}
		if id, ok := withdrawnBy(byKey[k], row.at, out); ok {
			out[id] = row.m.function
		}
	}
	return out
}

// withdrawnBy is the confirmation of cs, in id order, a withdrawal at
// `at` that names none withdraws: the newest at or before it that gone
// does not hold, else the first by id that gone does not hold.
func withdrawnBy(cs []caRow, at int64, gone map[string]string) (string, bool) {
	newest := -1
	for i, c := range cs {
		if gone[c.eventID] == "" && c.at <= at && (newest < 0 || c.at > cs[newest].at) {
			newest = i
		}
	}
	if newest >= 0 {
		return cs[newest].eventID, true
	}
	for _, c := range cs {
		if gone[c.eventID] == "" {
			return c.eventID, true
		}
	}
	return "", false
}

// corporateActionCashKind is the kind the event indicator settles, or
// false for an indicator the cash leg is not booked for.
func corporateActionCashKind(caev string) (canonical.TxKind, bool) {
	switch strings.ToUpper(strings.TrimSpace(caev)) {
	case "DVCA":
		return canonical.TxKindDividend, true
	case "INTR":
		return canonical.TxKindInterest, true
	}
	return "", false
}

// caCashPayload is what the booked leg's payload states: the
// confirmation it was read from and its figures. The posted amount is
// the cash the account received, which the row reads unless a
// dividend's confirmation states a gross it can book (bookGross).
type caCashPayload struct {
	CorporateAction string `json:"corporate_action"`
	CAEV            string `json:"caev"`
	PostedAmount    string `json:"posted_amount"`
	PostedCurrency  string `json:"posted_currency"`
	GrossAmount     string `json:"gross_amount,omitempty"`
	GrossCurrency   string `json:"gross_currency,omitempty"`
	TaxAmount       string `json:"tax_amount,omitempty"`
	TaxCurrency     string `json:"tax_currency,omitempty"`
}

// cashLegIDSuffix closes the id of the cash leg a confirmation books,
// after the confirmation's own id.
const cashLegIDSuffix = ":cash"

// cashConfirmation is one MT566 confirmation that credited cash: the
// event, the security it names and what its cash movement states.
type cashConfirmation struct {
	eventID string
	caev    string
	isin    string
	name    string
	move    caCashMove
	// stands is false for a cancellation, a reversal and a confirmation
	// one of them withdraws (withdrawals). Only one that stands books a
	// cash leg or states a gross. One withdrawn still names the security
	// of the line it pairs with (pairOf): which security paid is not
	// money.
	stands bool
	// reversed is a confirmation a reversal withdraws: the bank paid it
	// and took the payment back, so it is still a credit a reversal line
	// takes back (addFeedCredits).
	reversed bool
	// repeats is a confirmation that stands and states the posting, the
	// security and the gross of one earlier by id that stands, such as one
	// the bank sent again. It is the same statement (oneStatement): it
	// books no second leg and records no second credit.
	repeats bool
}

// statedGross is the gross the confirmation states for the cash it posted
// (grossOf), "" where it states none.
func (c cashConfirmation) statedGross() string {
	if gross, ok := c.move.grossOf(c.move.posted, c.move.postedCcy); ok {
		return gross.String()
	}
	return ""
}

// statementKey is what two confirmations that state one payment share:
// the cash account, the payment day, the amount posted and its currency,
// the security, and the gross it reads (statedGross).
type statementKey struct {
	cashAccount, posted, currency, isin, gross string
	day                                        int64
}

func (c cashConfirmation) statementKey() statementKey {
	posted := c.move.posted
	cents, _ := bookingCents(&posted)
	return statementKey{
		cashAccount: c.move.cashAccount, posted: cents, currency: bookingCurrency(c.move.postedCcy),
		isin: c.isin, gross: c.statedGross(), day: utcDay(c.move.paymentDay),
	}
}

// cashConfirmations reads every confirmation of the silver and returns
// those that credited cash (creditsCash), in id order, each marked
// whether it stands, was reversed or repeats another. The whole silver,
// unwindowed: a cancellation can arrive in another dump than what it
// withdraws. A movement that states no payment day takes the event's own
// day. The first call reads them and later calls return the same slice.
func (r *psnReader) cashConfirmations(ctx context.Context) ([]cashConfirmation, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if r.confirmations != nil {
		return r.confirmations, nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT event_external_id, timestamp, payload
  FROM events
 WHERE kind = 'corporate_action_confirmation'
 ORDER BY event_external_id`)
	if err != nil {
		return nil, fmt.Errorf("ubs cashConfirmations: %w", err)
	}
	defer rows.Close()
	var all []caRow
	for rows.Next() {
		var (
			row     caRow
			payload string
		)
		if err := rows.Scan(&row.eventID, &row.at, &payload); err != nil {
			return nil, fmt.Errorf("ubs cashConfirmations scan: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &row.f); err != nil {
			continue
		}
		row.m = parseCorporateAction(row.f)
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	gone := withdrawals(all)
	stated := map[statementKey]bool{}
	out := []cashConfirmation{}
	for _, row := range all {
		if !row.m.creditsCash() {
			continue
		}
		move := row.m.move
		if move.paymentDay == 0 {
			move.paymentDay = row.at
		}
		c := cashConfirmation{
			eventID:  row.eventID,
			caev:     strings.ToUpper(strings.TrimSpace(row.f.CAEV)),
			isin:     strings.TrimSpace(row.f.ISIN),
			name:     row.m.name,
			move:     move,
			stands:   isNewConfirmation(row.m.function) && gone[row.eventID] == "",
			reversed: gone[row.eventID] == "REVR",
		}
		if c.stands {
			k := c.statementKey()
			c.repeats = stated[k]
			stated[k] = true
		}
		out = append(out, c)
	}
	r.confirmations = out
	return out, nil
}

// legOf is the account and the kind of the cash leg the confirmation
// books itself (corporateActionCashLegs): where it stands and repeats no
// other, its event indicator settles the kind, and the feed does not
// speak for the account it paid into on the payment day. false where the
// feed's own line carries the payment, or nothing books one.
func (c cashConfirmation) legOf(coverage psnCashCoverage, ibans map[string]string) (string, canonical.TxKind, bool) {
	kind, settled := corporateActionCashKind(c.caev)
	account := cashIBAN(ibans, c.move.cashAccount)
	if !c.stands || c.repeats || !settled || coverage.speaksFor(account, c.move.paymentDay) {
		return "", "", false
	}
	return account, kind, true
}

// corporateActionCashLegs books the cash each confirmation paid into an
// account the feed does not speak for (cashLeg), on a payment day in the
// window: a leg occurs on that day, and a load replaces the window's rows
// by the day they occur. withheld names the legs an export row records
// instead (buildEraFold); they are not booked.
func (r *psnReader) corporateActionCashLegs(ctx context.Context, w canonical.Window, coverage psnCashCoverage, ibans map[string]string, withheld map[string]bool) ([]canonical.TransactionChange, error) {
	confirmations, err := r.cashConfirmations(ctx)
	if err != nil {
		return nil, err
	}
	var out []canonical.TransactionChange
	for _, c := range confirmations {
		if c.move.paymentDay < w.Start || c.move.paymentDay > w.End || withheld[c.eventID+cashLegIDSuffix] {
			continue
		}
		tx, tax, taxed, ok, err := c.cashLeg(coverage, ibans)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, tx)
		if taxed {
			out = append(out, tax)
		}
	}
	return out, nil
}

// cashLegRef is a cash leg a confirmation books, as the era fold reads
// it: its id, and whether it reads a gross.
type cashLegRef struct {
	id      string
	grossed bool
}

// cashLegKeys maps, over the whole silver, the booking key of each cash
// leg the confirmations book (legOf) onto the legs. The key holds the
// amount posted: what moved, and what any other record of the payment
// states, not the gross the leg reads. The era fold folds such a record
// into its leg (buildEraFold).
func (r *psnReader) cashLegKeys(ctx context.Context, coverage psnCashCoverage) (map[bookingKey][]cashLegRef, error) {
	ibans, err := r.cashAccountIBANs(ctx)
	if err != nil {
		return nil, err
	}
	confirmations, err := r.cashConfirmations(ctx)
	if err != nil {
		return nil, err
	}
	out := map[bookingKey][]cashLegRef{}
	for _, c := range confirmations {
		tx, _, _, ok, err := c.cashLeg(coverage, ibans)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		posted := c.move.posted
		if k, ok := newBookingKey(tx.AccountExternalID, c.move.paymentDay, &posted, c.move.postedCcy); ok {
			out[k] = append(out[k], cashLegRef{id: tx.TransactionExternalID, grossed: tx.GrossAmount != nil})
		}
	}
	return out, nil
}

// cashLeg is the row a confirmation books for the cash it paid (legOf):
// the kind its event indicator settles, on the payment day, with the
// confirmation's figures in its payload. A dividend whose confirmation
// states its gross is booked at the gross, with the tax withheld beside
// it as its own row (bookGross). ok is false where it books no leg.
func (c cashConfirmation) cashLeg(coverage psnCashCoverage, ibans map[string]string) (tx, tax canonical.TransactionChange, withheld, ok bool, err error) {
	account, kind, booked := c.legOf(coverage, ibans)
	if !booked {
		return tx, tax, false, false, nil
	}
	move := c.move
	p := caCashPayload{
		CorporateAction: c.eventID,
		CAEV:            c.caev,
		PostedAmount:    move.posted.String(),
		PostedCurrency:  move.postedCcy,
	}
	if move.grossCcy != "" {
		p.GrossAmount, p.GrossCurrency = move.gross.String(), move.grossCcy
	}
	if move.taxCcy != "" {
		p.TaxAmount, p.TaxCurrency = move.tax.String(), move.taxCcy
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return tx, tax, false, false, fmt.Errorf("ubs cash leg payload: %w", err)
	}
	posted := move.posted
	tx = canonical.TransactionChange{
		TransactionExternalID: c.eventID + cashLegIDSuffix,
		OccurredAt:            move.paymentDay,
		AccountExternalID:     account,
		Kind:                  kind,
		Currency:              move.postedCcy,
		NetAmount:             canonical.ApplyCanonicalSign(kind, &posted),
		Description:           textPtr(c.name),
		ProviderCategory:      textPtr(c.caev),
		Payload:               json.RawMessage(encoded),
	}
	if c.isin != "" {
		isin := c.isin
		tx.InstrumentExternalID = &isin
	}
	if kind != canonical.TxKindDividend {
		// Interest keeps the posted amount, its gross beside it.
		if move.grossCcy == move.postedCcy && !move.gross.IsZero() {
			gross := move.gross
			tx.GrossAmount = canonical.ApplyCanonicalSign(kind, &gross)
		}
		return tx, tax, false, true, nil
	}
	tax, withheld, err = bookGross(&tx, c)
	return tx, tax, withheld, err == nil, err
}
