package ubs

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// One booking, one row.
//
// A UBS cash booking can stand on several records. A statement prints it,
// the netbanking export lists it, a payment advice states it, the MT940
// feed carries it, and an MT515 or MT566 confirms the trade or the
// payment behind it. The adapter books
// two records of its own where no feed carries the booking: a conversion's
// other leg (conversionMirrors) and a corporate action's cash leg
// (cashLeg). The portfolio transaction list states a managed portfolio's
// trades once more. The records share no id, so one booking reaches gold
// once only if something decides which record keeps it.
//
// That decision is one ordered table of rules (reconcileRules). Each
// record is read once and becomes a candidate. Each rule names the
// records that lose to it and the records that win, the key a loser and
// its winner share, and how many losers one winner takes. A rule reads
// only the candidates the rules before it left standing, so the order of
// the table is part of each rule's meaning.
//
// The keys are not one identity, because the rails do not state one
// booking alike. A statement and an export state the signed cash; a
// confirmation states an unsigned figure and the day it settles; the
// list states the trade's value, which no cash record shares; a feed
// line repeats the bank's own number for the entry. So each rule keeps
// the key its two records can agree on.
//
// What holds for every rule:
//   - The hard cut decides by authority, not by match: from its PSN start,
//     the feed is the record of an account it speaks for (psnCut).
//   - No rule pairs two records of one kind. Two equal payments on one day
//     are two bookings.
//   - A one-to-one rule counts: N winners take at most N losers, so a
//     second equal booking survives.
//   - The survivor takes each text column it lacks from the records it
//     replaced (carry), per column and only where its own is empty or a
//     bare code.
//   - The whole silver, unwindowed: whether a booking is recorded twice
//     depends on silver's contents alone, never on which slice of time a
//     load covers.

// recordKind is which record of a booking a candidate is.
type recordKind uint8

const (
	// recStatement: a web row reconstructed from a printed statement
	// (isStatementEraID).
	recStatement recordKind = iota
	// recExport: a web row of the netbanking export, the bank's
	// machine-readable record.
	recExport
	// recAdvice: a web row read from a payment advice (sourceAdvicePDF).
	// It carries the bank's own number for the entry, as an export row
	// does, and states nothing else but the movement.
	recAdvice
	// recFeed: an MT940 cash movement.
	recFeed
	// recMirror: a conversion's other leg the adapter books
	// (conversionMirrors).
	recMirror
	// recLeg: the cash leg the adapter books from a corporate-action
	// confirmation (cashLeg).
	recLeg
	// recConfirmation: an MT515 trade confirmation.
	recConfirmation
	// recList: a row of the portfolio transaction list that settles cash
	// (portfolioListRows).
	recList
)

// webRecordKind is the kind of a web row: a statement reconstruction, an
// advice, or an export row. An advice is known by the marker its payload
// carries, the one the loader deletes it by.
func webRecordKind(row *webRow) recordKind {
	switch {
	case isStatementEraID(row.txID):
		return recStatement
	case strings.Contains(row.payload, sourceAdvicePDF):
		return recAdvice
	}
	return recExport
}

// candidate is one record of a booking, as the rules read it.
type candidate struct {
	kind recordKind
	// id is the id the record reaches gold under.
	id string

	// The record itself; which field is set follows the kind.
	web    *webRow           // statement, export, advice
	feed   *feedRow          // feed, confirmation
	mirror *conversionMirror // mirror
	leg    *legRecord        // leg
	list   *listRow          // list

	// lostTo is the rule the record lost to; empty while it stands.
	lostTo string
	// text is what the records it replaced carry onto it; nil while
	// none does.
	text *webTxText
}

// legRecord is what the rules read of a cash leg: the booking it posted,
// and whether it reads a gross.
type legRecord struct {
	key     bookingKey
	keyed   bool
	grossed bool
}

func (c *candidate) stands() bool { return c.lostTo == "" }

// webText is a web record's narrative columns (projectWebTxText).
func (c *candidate) webText() webTxText {
	row := c.web
	text, _, _ := projectWebTxText(row.counterparty.String, row.descKind.String, row.pl, row.pdfBackfill)
	return text
}

// take carries text onto the record: the first copy whole, each later
// one only where the text so far is empty or a bare code (or).
func (c *candidate) take(text webTxText) {
	if c.text != nil {
		text = c.text.or(text)
	}
	c.text = &text
}

// matchKind is how a rule pairs its losers with its winners.
type matchKind uint8

const (
	// byAuthority: the rule's own test (owns) says the loser's account
	// and day are another rail's. No winner is matched.
	byAuthority matchKind = iota
	// oneToOne: per key, the i-th loser goes to the i-th winner. Winners
	// are in id order; losers in id order (byID) or in the order they
	// were read. A loser past the last winner stands.
	oneToOne
	// anyWinner: per key, every loser goes while any winner stands.
	anyWinner
	// firstFree: losers in id order, each to the first winner by id that
	// shares any of its keys and no earlier loser took. A winner takes
	// one loser whichever key it matched on.
	firstFree
)

// reconcileRule is one row of the table.
type reconcileRule struct {
	name    string
	losers  []recordKind
	winners []recordKind
	match   matchKind
	// key is the identity a loser and its winner share; false where the
	// record has none, and takes no part in the rule. A firstFree rule
	// has keys instead, tried together.
	key  func(*candidate) (any, bool)
	keys []func(*candidate) (any, bool)
	// owns is byAuthority's test.
	owns func(*candidate) bool
	// byID orders a oneToOne rule's losers by id; else they keep the
	// order they were read in.
	byID bool
	// carry: the winner takes the loser's text (take).
	carry bool
	// yields reports, for a oneToOne pair, that the winner is the record
	// that goes instead: the loser stands, and nothing is carried.
	yields func(winner, loser *candidate) bool
}

// The rule names. A consumer reads ruleCut apart from the others.
const (
	ruleLegsOverMovement   = "split legs over their movement"
	ruleWebOverAdvice      = "web over advice"
	ruleCut                = "cut"
	ruleLegOverExport      = "leg over export"
	ruleExportOverStmt     = "export over statement"
	ruleFeedOverStmt       = "feed over statement"
	ruleFeedOverWebByRef   = "feed over web by number"
	ruleWebOverMirror      = "web over mirror"
	ruleConfirmationOverMT = "confirmation over settlement line"
	ruleSettledOverList    = "settlement over list"
)

// reconcileRules is the table, in order. ibans resolves the bank's
// internal cash-account ids to the IBANs the registry keys by
// (cashIBAN).
// split names the movements a statement printed as one batch and the
// parser split into its payments (splitMovements).
func reconcileRules(cut psnCut, ibans map[string]string, split map[webTxTextKey]bool) []reconcileRule {
	return []reconcileRule{{
		// A batch the statement printed as one movement and the parser
		// split into its payments is those payments. A statement that
		// printed it whole beside one that listed them adds nothing.
		name:   ruleLegsOverMovement,
		losers: []recordKind{recStatement},
		match:  byAuthority,
		owns: func(c *candidate) bool {
			return split[webTxTextKey{account: c.web.account, txnNo: c.web.txID}]
		},
	}, {
		// An advice states one movement and nothing else. Where the
		// ledger holds it on the same value date or on the same booking
		// date, the ledger's row is the record: it carries the booking
		// type and reconciled against the statement's printed balance.
		name:    ruleWebOverAdvice,
		losers:  []recordKind{recAdvice},
		winners: []recordKind{recStatement, recExport},
		match:   firstFree,
		keys: []func(*candidate) (any, bool){
			func(c *candidate) (any, bool) { return adviceKeyOf(c, false) },
			func(c *candidate) (any, bool) { return adviceKeyOf(c, true) },
		},
	}, {
		// From each relationship's PSN start, a web row on an account
		// the feed speaks for is the feed's to carry.
		name:   ruleCut,
		losers: []recordKind{recStatement, recExport, recAdvice},
		match:  byAuthority,
		owns:   func(c *candidate) bool { return cut.excludes(c.web.account, c.web.valueDate) },
	}, {
		// A cash leg carries the confirmation's security, gross and tax.
		// Its web copy (an export row or an advice) goes, unless only the
		// copy states a gross: the copy then stands, at its gross, and
		// the leg goes.
		name:    ruleLegOverExport,
		losers:  []recordKind{recExport, recAdvice},
		winners: []recordKind{recLeg},
		match:   oneToOne,
		key:     bookingKeyOf,
		byID:    true,
		carry:   true,
		yields: func(leg, export *candidate) bool {
			return !leg.leg.grossed && export.exportGrossed()
		},
	}, {
		// The machine-readable records outrank a reconstruction from a
		// printed statement: the export first, then the feed's records.
		// An advice does not: a statement row it shares a booking with
		// took the advice before, or is a movement of its own.
		name:    ruleExportOverStmt,
		losers:  []recordKind{recStatement},
		winners: []recordKind{recExport},
		match:   oneToOne,
		key:     bookingKeyOf,
		byID:    true,
		carry:   true,
	}, {
		name:    ruleFeedOverStmt,
		losers:  []recordKind{recStatement},
		winners: []recordKind{recFeed, recMirror, recLeg},
		match:   oneToOne,
		key:     bookingKeyOf,
		byID:    true,
		carry:   true,
	}, {
		// The first PSN dump's statements reach back over days the cut
		// leaves to the web side. There the bank's own number for the
		// entry says the feed already holds it.
		name:    ruleFeedOverWebByRef,
		losers:  []recordKind{recStatement, recExport, recAdvice},
		winners: []recordKind{recFeed},
		match:   anyWinner,
		key:     bankRefKeyOf,
	}, {
		// Where a web row carries a conversion's other leg, the mirror of
		// it goes. A web row keys here only as a deposit or a
		// withdrawal, the kinds a mirror books.
		name:    ruleWebOverMirror,
		losers:  []recordKind{recMirror},
		winners: []recordKind{recStatement, recExport, recAdvice},
		match:   anyWinner,
		key:     transferBookingKeyOf,
	}, {
		// A trade reaches the feed twice, as its confirmation and as the
		// MT940 line settling its cash. The confirmation keeps it.
		name:    ruleConfirmationOverMT,
		losers:  []recordKind{recFeed},
		winners: []recordKind{recConfirmation},
		match:   oneToOne,
		key:     func(c *candidate) (any, bool) { return settlementKeyOf(c, ibans) },
	}, {
		// Each securities settlement another rail holds on a cash
		// account, currency and day excuses one list row on that key.
		name:    ruleSettledOverList,
		losers:  []recordKind{recList},
		winners: []recordKind{recStatement, recExport, recAdvice, recConfirmation, recFeed},
		match:   oneToOne,
		key:     func(c *candidate) (any, bool) { return settledDayKeyOf(c, ibans) },
	}}
}

// bookingKeyOf is the booking a record moves (bookingKey): the cash each
// rail's projection moves, a cash leg's the amount it posted.
func bookingKeyOf(c *candidate) (any, bool) {
	switch c.kind {
	case recStatement, recExport, recAdvice:
		row := c.web
		return newBookingKey(row.account, row.valueDate, row.signed, row.currency)
	case recFeed:
		if !c.feed.projected {
			return nil, false
		}
		return txBookingKey(c.feed.tx)
	case recMirror:
		return txBookingKey(c.mirror.tx)
	case recLeg:
		return c.leg.key, c.leg.keyed
	}
	return nil, false
}

// adviceKey is a movement as an advice and the ledger both state it: the
// account, the currency, one of the two dates, the column the figure
// sits in, and the figure's magnitude to the cent. The magnitude, because
// the export signs its cells and the printed eras do not; the column says
// the direction.
type adviceKey struct {
	account, currency string
	day               int64
	outbound          bool
	cents             string
}

// adviceKeyOf is a web row's adviceKey on its value date, or on its
// booking date where byEntry is set.
func adviceKeyOf(c *candidate, byEntry bool) (any, bool) {
	row := c.web
	day := row.valueDate
	if byEntry {
		if !row.bookingDate.Valid {
			return nil, false
		}
		day = row.bookingDate.Int64
	}
	figure, outbound := row.credit, false
	if row.debit.Valid {
		figure, outbound = row.debit, true
	}
	if !figure.Valid {
		return nil, false
	}
	magnitude := canonical.NewDecimalFromFloat(math.Abs(figure.Float64))
	cents, ok := bookingCents(&magnitude)
	if !ok {
		return nil, false
	}
	return adviceKey{account: row.account, currency: row.currency, day: day, outbound: outbound, cents: cents}, true
}

// bankRefKeyOf is the bank's own number for the entry and the account it
// sits on (webTxTextKey): a web row's transaction number, a feed line's
// bank reference.
func bankRefKeyOf(c *candidate) (any, bool) {
	switch c.kind {
	case recStatement, recExport, recAdvice:
		return webTxTextKey{account: c.web.account, txnNo: webTxNumber(c.web.txID)}, true
	case recFeed:
		row := c.feed
		if !row.decoded || row.pl.BankRef == "" {
			return nil, false
		}
		return webTxTextKey{account: row.bookedAccount(), txnNo: row.pl.BankRef}, true
	}
	return nil, false
}

// transferBookingKeyOf is bookingKeyOf for a mirror and for a web row
// that is a deposit or a withdrawal.
func transferBookingKeyOf(c *candidate) (any, bool) {
	if c.web != nil && c.web.kind != canonical.TxKindDeposit && c.web.kind != canonical.TxKindWithdrawal {
		return nil, false
	}
	return bookingKeyOf(c)
}

// settlementKeyOf is a securities settlement as both PSN rails state it
// (settlementKey): a confirmation's cash leg, and an MT940 line the bank
// typed as one (NSEC, or NMSC, under which it books a fund redemption's
// cash).
func settlementKeyOf(c *candidate, ibans map[string]string) (any, bool) {
	switch c.kind {
	case recFeed:
		row := c.feed
		if !row.decoded {
			return nil, false
		}
		p := row.pl
		switch strings.ToUpper(strings.TrimSpace(p.TxnType)) {
		case "NSEC", "NMSC":
		default:
			return nil, false
		}
		currency := p.Funds
		if currency == "" && row.currency.Valid {
			currency = row.currency.String
		}
		return newSettlementKey(cashIBAN(ibans, row.bookedAccount()), currency, p.Amount, row.at)
	case recConfirmation:
		p := c.feed.trade
		return newSettlementKey(cashIBAN(ibans, p.CashAccountExternalID), c.confirmationCurrency(),
			p.NetAmount, p.SettlementDateUnix)
	}
	return nil, false
}

// settledDayKeyOf is the day a securities settlement lands on its cash
// account (settledDayKey): a list row's, and each other rail's trade.
func settledDayKeyOf(c *candidate, ibans map[string]string) (any, bool) {
	switch c.kind {
	case recList:
		l := c.list
		return newSettledDayKey(l.cashAccount, l.currency, l.valueDate), true
	case recStatement, recExport, recAdvice:
		row := c.web
		if row.kind != canonical.TxKindBuy && row.kind != canonical.TxKindSell || row.movesNothing() {
			return nil, false
		}
		return newSettledDayKey(row.account, row.currency, row.valueDate), true
	case recConfirmation:
		// The confirmation dates the trade to its execution and says
		// separately when the cash lands. It is the landing that shares
		// a day with the list's value date.
		at := c.feed.trade.SettlementDateUnix
		if at == 0 {
			at = c.feed.at
		}
		return newSettledDayKey(cashIBAN(ibans, c.feed.trade.CashAccountExternalID), c.confirmationCurrency(), at), true
	case recFeed:
		tx, _, ok := c.feed.project()
		if !ok || tx.Kind != canonical.TxKindBuy && tx.Kind != canonical.TxKindSell {
			return nil, false
		}
		return newSettledDayKey(tx.AccountExternalID, tx.Currency, tx.OccurredAt), true
	}
	return nil, false
}

// confirmationCurrency is the currency a confirmation settles in: the net
// amount's, else the event's.
func (c *candidate) confirmationCurrency() string {
	currency := c.feed.trade.NetCurrency
	if currency == "" && c.feed.currency.Valid {
		currency = c.feed.currency.String
	}
	return currency
}

// exportGrossed reports whether the export pass books an export row at a
// gross (exportGross).
func (c *candidate) exportGrossed() bool {
	row := c.web
	_, ok := exportGross(row.kind, row.pl, row.pdfBackfill, row.currency, row.signed)
	return ok
}

// apply runs one rule over the candidates still standing.
func (rule *reconcileRule) apply(all []*candidate) {
	switch rule.match {
	case byAuthority:
		for _, c := range all {
			if c.stands() && slices.Contains(rule.losers, c.kind) && rule.owns(c) {
				c.lostTo = rule.name
			}
		}
		return
	case firstFree:
		rule.applyFirstFree(all)
		return
	}
	// Both sides are taken before any verdict, so a record this rule
	// takes still counts for it.
	var lostKeys []any
	losers := map[any][]*candidate{}
	winners := map[any][]*candidate{}
	for _, c := range all {
		if !c.stands() {
			continue
		}
		switch {
		case slices.Contains(rule.losers, c.kind):
			if k, ok := rule.key(c); ok {
				if _, seen := losers[k]; !seen {
					lostKeys = append(lostKeys, k)
				}
				losers[k] = append(losers[k], c)
			}
		case slices.Contains(rule.winners, c.kind):
			if k, ok := rule.key(c); ok {
				winners[k] = append(winners[k], c)
			}
		}
	}
	for _, k := range lostKeys {
		ls, ws := losers[k], winners[k]
		if len(ws) == 0 {
			continue
		}
		if rule.match == anyWinner {
			for _, l := range ls {
				l.lostTo = rule.name
			}
			continue
		}
		if rule.byID {
			sort.Slice(ls, func(i, j int) bool { return ls[i].id < ls[j].id })
		}
		sort.Slice(ws, func(i, j int) bool { return ws[i].id < ws[j].id })
		for i := range min(len(ls), len(ws)) {
			w, l := ws[i], ls[i]
			if rule.yields != nil && rule.yields(w, l) {
				w.lostTo = rule.name
				continue
			}
			l.lostTo = rule.name
			if rule.carry {
				w.take(l.webText())
			}
		}
	}
}

// applyFirstFree is apply for a firstFree rule.
func (rule *reconcileRule) applyFirstFree(all []*candidate) {
	var losers []*candidate
	winners := make([]map[any][]*candidate, len(rule.keys))
	for i := range winners {
		winners[i] = map[any][]*candidate{}
	}
	for _, c := range all {
		switch {
		case !c.stands():
		case slices.Contains(rule.losers, c.kind):
			losers = append(losers, c)
		case slices.Contains(rule.winners, c.kind):
			for i, key := range rule.keys {
				if k, ok := key(c); ok {
					winners[i][k] = append(winners[i][k], c)
				}
			}
		}
	}
	for _, at := range winners {
		for _, ws := range at {
			sort.Slice(ws, func(i, j int) bool { return ws[i].id < ws[j].id })
		}
	}
	sort.Slice(losers, func(i, j int) bool { return losers[i].id < losers[j].id })
	taken := map[*candidate]bool{}
	for _, l := range losers {
		var first *candidate
		for i, key := range rule.keys {
			k, ok := key(l)
			if !ok {
				continue
			}
			for _, w := range winners[i][k] {
				if !taken[w] {
					if first == nil || w.id < first.id {
						first = w
					}
					break
				}
			}
		}
		if first != nil {
			taken[first] = true
			l.lostTo = rule.name
		}
	}
}

// recordRef names a candidate: its kind and its id.
type recordRef struct {
	kind recordKind
	id   string
}

// reconciliation is the table's verdict over one connection's silvers.
type reconciliation struct {
	// cut is the hard cut its rule (ruleCut) applied.
	cut psnCut
	// records is every candidate, in the order read.
	records []*candidate
	byRef   map[recordRef]*candidate
	// list is the portfolio list's rows that settle cash, in the list's
	// order, the unplaced and unpriced among them.
	list []*listRow
	// withheld names the mirrors and cash legs that lost: the PSN stream
	// does not book them.
	withheld map[string]bool
	// psnText is the text carried onto a feed line, a mirror or a cash
	// leg, by its id.
	psnText map[string]webTxText
}

// lostTo is the rule the record lost to, "" where it stands or is no
// candidate.
func (rc *reconciliation) lostTo(kind recordKind, id string) string {
	if c, ok := rc.byRef[recordRef{kind, id}]; ok {
		return c.lostTo
	}
	return ""
}

// webCandidate is a web row's candidate; nil where it is none.
func (rc *reconciliation) webCandidate(row *webRow) *candidate {
	return rc.byRef[recordRef{webRecordKind(row), row.emittedID()}]
}

// webLostTo is lostTo for a web row.
func (rc *reconciliation) webLostTo(row *webRow) string {
	if c := rc.webCandidate(row); c != nil {
		return c.lostTo
	}
	return ""
}

// carried is the text the records a web row replaced carry onto it.
func (rc *reconciliation) carried(row *webRow) (webTxText, bool) {
	c := rc.webCandidate(row)
	if c == nil || c.text == nil {
		return webTxText{}, false
	}
	return *c.text, true
}

// reconcile reads every record of the two silvers (either may be absent)
// and runs the table over them.
func reconcile(ctx context.Context, web *webReader, psn *psnReader, rels []RelationshipPair) (*reconciliation, error) {
	rc, ibans, err := readCandidates(ctx, web, psn, rels)
	if err != nil {
		return nil, err
	}
	for _, rule := range reconcileRules(rc.cut, ibans, splitMovements(rc.records)) {
		rule.apply(rc.records)
	}
	rc.index()
	return rc, nil
}

// readCandidates reads the cut and every candidate: the web ledger, the
// feed's cash movements and trade confirmations, the mirrors and cash
// legs the adapter books, and the list rows that settle cash. Each in
// the order its rail is read, the mirrors by id.
func readCandidates(ctx context.Context, web *webReader, psn *psnReader, rels []RelationshipPair) (*reconciliation, map[string]string, error) {
	rc := &reconciliation{}
	var err error
	if rc.cut.startByRel, err = buildPSNStartByWebRel(ctx, psn, rels); err != nil {
		return nil, nil, err
	}
	if web != nil {
		if rc.cut.relOfAccount, err = web.buildAccountToRelMap(ctx); err != nil {
			return nil, nil, err
		}
	}
	if rc.cut.coverage, err = psn.cashCoverage(ctx); err != nil {
		return nil, nil, err
	}
	roster, err := psn.roster(ctx)
	if err != nil {
		return nil, nil, err
	}
	ibans := roster.cashAccountIBANs()

	ledger, err := web.ledger(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range ledger {
		row := &ledger[i]
		rc.records = append(rc.records, &candidate{
			kind: webRecordKind(row), id: row.emittedID(), web: row,
		})
	}
	feed, err := psn.feed(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range feed.events {
		row := &feed.events[i]
		switch row.kind {
		case "cash_movement":
			rc.records = append(rc.records, &candidate{kind: recFeed, id: row.eventID, feed: row})
		case "trade_confirmation":
			if row.err != nil {
				return nil, nil, fmt.Errorf("ubs trade confirmation payload (event_id=%s): %w", row.eventID, row.err)
			}
			rc.records = append(rc.records, &candidate{kind: recConfirmation, id: row.eventID, feed: row})
		}
	}
	if web != nil {
		mirrors, err := psn.conversionMirrors(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range slices.Sorted(maps.Keys(mirrors.byID)) {
			m := mirrors.byID[id]
			rc.records = append(rc.records, &candidate{kind: recMirror, id: id, mirror: &m})
		}
		confirmations, err := psn.cashConfirmations(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range confirmations {
			tx, _, _, ok, err := c.cashLeg(rc.cut.coverage, ibans)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
			leg := &legRecord{grossed: tx.GrossAmount != nil}
			posted := c.move.posted
			leg.key, leg.keyed = newBookingKey(tx.AccountExternalID, c.move.paymentDay, &posted, c.move.postedCcy)
			rc.records = append(rc.records, &candidate{kind: recLeg, id: tx.TransactionExternalID, leg: leg})
		}
		if rc.list, err = web.portfolioListRows(ctx, roster); err != nil {
			return nil, nil, err
		}
		for _, l := range rc.list {
			if l.books() {
				rc.records = append(rc.records, &candidate{kind: recList, id: l.emittedID(), list: l})
			}
		}
	}
	return rc, ibans, nil
}

// splitMovements names, by account and id, the statement movements the
// parser split into payments: a leg `<id>#1` of it stands in silver.
func splitMovements(records []*candidate) map[webTxTextKey]bool {
	out := map[webTxTextKey]bool{}
	for _, c := range records {
		if c.kind != recStatement {
			continue
		}
		if base, ok := strings.CutSuffix(c.web.txID, "#1"); ok {
			out[webTxTextKey{account: c.web.account, txnNo: base}] = true
		}
	}
	return out
}

// index builds the lookups the passes read.
func (rc *reconciliation) index() {
	rc.byRef = make(map[recordRef]*candidate, len(rc.records))
	rc.withheld = map[string]bool{}
	rc.psnText = map[string]webTxText{}
	for _, c := range rc.records {
		rc.byRef[recordRef{c.kind, c.id}] = c
		switch c.kind {
		case recMirror, recLeg:
			if !c.stands() {
				rc.withheld[c.id] = true
			}
			fallthrough
		case recFeed:
			if c.text != nil {
				rc.psnText[c.id] = *c.text
			}
		}
	}
}
