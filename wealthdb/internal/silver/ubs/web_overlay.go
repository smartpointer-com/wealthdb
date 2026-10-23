package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Web-side overlay helpers. The orchestrator pre-fetches per-key
// web payloads once and hands them to the PSN-side fold stream
// (merge.go) so the cross-source join lives in one place.

// webPosKey is the per-(UTC date, ISIN) match key for the
// position-payload overlay. Web silver flattens all securities
// under a single portfolio, so we can't key
// by (account, portfolio, ISIN) without ambiguity. ISIN alone
// plus the day is enough granularity per source — overlay
// payload semantics don't depend on uniqueness, only on having
// useful cost_price/lending_value to fold.
type webPosKey struct {
	utcDate int64
	isin    string
}

// positionPayloadByKey returns web's per-(UTC date, ISIN) JSON
// payload. Used by the orchestrator's PSN-side fold to inject
// web-only fields (cost_price, lending_value, lending_value_ratio,
// market_value_base) into PSN's position payloads during the
// overlap window.
func (r *webReader) positionPayloadByKey(ctx context.Context, wStart, wEnd int64) (map[webPosKey]string, error) {
	if r == nil {
		return nil, nil
	}
	const q = `
SELECT snapshot_at, instrument_isin, payload
  FROM positions
 WHERE instrument_isin IS NOT NULL
   AND snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("web positionPayloadByKey: %w", err)
	}
	defer rows.Close()
	out := make(map[webPosKey]string)
	for rows.Next() {
		var snap int64
		var isin, payload string
		if err := rows.Scan(&snap, &isin, &payload); err != nil {
			return nil, err
		}
		out[webPosKey{utcDate: utcDay(snap), isin: isin}] = payload
	}
	return out, rows.Err()
}

// webCashKey is the (UTC date, account, currency) key for web's
// cash-position rows. Web uses IBAN as account_external_id so
// this matches PSN's cash_balances directly post-canonicalisation.
type webCashKey struct {
	utcDate  int64
	account  string
	currency string
}

// cashPayloadByKey returns web's per-(UTC date, account, ccy)
// JSON payload for cash positions. Folded into PSN's cash
// balances during the overlap.
func (r *webReader) cashPayloadByKey(ctx context.Context, wStart, wEnd int64) (map[webCashKey]string, error) {
	if r == nil {
		return nil, nil
	}
	const q = `
SELECT snapshot_at, account_external_id, currency_iso, payload
  FROM positions
 WHERE instrument_isin IS NULL
   AND snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("web cashPayloadByKey: %w", err)
	}
	defer rows.Close()
	out := make(map[webCashKey]string)
	for rows.Next() {
		var snap int64
		var acct, ccy, payload string
		if err := rows.Scan(&snap, &acct, &ccy, &payload); err != nil {
			return nil, err
		}
		out[webCashKey{utcDate: utcDay(snap), account: acct, currency: ccy}] = payload
	}
	return out, rows.Err()
}

// webTxTextKey identifies one booking across the era seam: the bank's
// own number for the entry — the account statement's "Transaction
// no.", which web silver uses as its transaction_external_id and which
// the MT940 :61: line repeats as its bank reference — and the account
// it was booked on.
//
// The number alone would not be a key: UBS stamps both legs of an
// inter-account transfer with one number, and the two legs have
// different payees to say. The account separates them.
type webTxTextKey struct {
	account string
	txnNo   string
}

// buildSeamBankRefs is the set of bookings the MT940 feed already holds,
// keyed by the bank's own number for the entry and the account it sits
// on — the same identity psnWebTextFoldStream carries text along.
//
// The hard cut is placed at the first PSN DUMP, because that is the day
// PSN's coverage becomes complete and a cut placed any earlier would
// drop web bookings PSN never carried. But the first dump's MT940
// statements reach back over the days before it, so the seam has a
// short window where both feeds hold the same entry and neither side's
// window excludes it. The web copy is the one dropped, matching what
// the cut does on every later day: the MT940 row reaches gold and the
// export's text is folded onto it.
//
// The bank reference is an exact identity, not a signature over amounts
// — the export prints it as "Transaction no." and the `:61:` line
// repeats it verbatim — so this drops only an entry the two feeds agree
// is one booking. Paired with the account because an inter-account
// transfer's two legs share the reference and are two bookings.
func (r *webReader) buildSeamBankRefs(ctx context.Context, psn *psnReader) (map[webTxTextKey]bool, error) {
	out := map[webTxTextKey]bool{}
	err := psn.eachCashMovement(ctx, "ubs-psn buildSeamBankRefs", func(row psnCashRow) error {
		var m cashMovementPayload
		if err := json.Unmarshal([]byte(row.payload), &m); err != nil || m.BankRef == "" {
			return nil
		}
		acct := row.account
		if m.Account != "" {
			acct = m.Account
		}
		out[webTxTextKey{account: acct, txnNo: m.BankRef}] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// transactionTextByKey returns the narrative columns every web
// transaction projects (projectWebTxText), keyed by (account,
// "Transaction no."). The PSN-side text fold (merge.go) reads it for
// the entries the hard cut suppresses on the web side, where the
// MT940 feed recorded the same booking as a bare code.
//
// The whole table, unwindowed and uncut, for the same reason
// buildSameDayOffsetVeto reads it whole: what an entry's narrative is
// depends only on silver's contents, never on which slice of time a
// load happens to cover. Nothing here decides whether a row is
// emitted; the hard cut still owns that.
func (r *webReader) transactionTextByKey(ctx context.Context) (map[webTxTextKey]webTxText, error) {
	if r == nil {
		return nil, nil
	}
	// The key is the BANK's number, so the several rows a collector
	// suffix splits one number into all answer to it: a cross-border
	// payment and the correspondent's charge, a deposit product's whole
	// ledger. The row holding the bare number is the one the number
	// names — by the collector's rule the group's principal movement —
	// so it wins outright, and among suffixed rows the lowest id wins,
	// because the query is unordered and a fold that depends on scan
	// order is a fold that changes under a silver rewrite.
	out := map[webTxTextKey]webTxText{}
	won := map[webTxTextKey]string{}
	err := r.eachWebTx(ctx, "ubs-web transactionTextByKey", "", nil, func(row webTxRow) error {
		key := webTxTextKey{account: row.account, txnNo: webTxNumber(row.txID)}
		if held, taken := won[key]; taken {
			if held == key.txnNo || (row.txID != key.txnNo && row.txID > held) {
				return nil
			}
		}
		p, pdfBackfill := decodeWebTxEra(row.payload)
		text, _, _ := projectWebTxText(row.counterparty.String, row.descKind.String, p, pdfBackfill)
		out[key], won[key] = text, row.txID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- the era fold ---------------------------------------------------------

// statementIDPrefix marks a `ubs-web` transaction the collector
// reconstructed from a printed Account Statement rather than read from a
// machine-readable feed. The collector stamps those ids itself (a content
// hash of the printed row), so the prefix is the era a row belongs to,
// stated in the id and readable without decoding a payload.
const statementIDPrefix = "stmt:"

// isStatementEraID reports whether a web transaction id names a statement
// reconstruction. The other web ids are the bank's own "Transaction no."
// and never carry the prefix, and the MT940 feed's ids live in a different
// silver entirely, so the test is exact.
func isStatementEraID(txID string) bool { return strings.HasPrefix(txID, statementIDPrefix) }

// bookingKey identifies one cash booking independently of which era
// recorded it: the account it moved on, the value day, the signed amount
// and the currency. Three eras reach gold's cash ledger — statement
// reconstructions, the account-statement export and the MT940 feed — and
// their id schemes are disjoint, so identity across them can only be the
// booking's own facts. Nothing about the narrative enters it: the same
// entry is worded differently in each era by construction.
//
// Every component is taken from the adapter's OWN projection of the row
// rather than re-read from the silver columns, because the eras write
// those columns to different conventions and only the projection resolves
// them (webProjectedNet for the two web eras, buildTransaction for the
// feed). What the key compares is therefore the cash each row moves: a
// dividend gold books at its gross keys on the amount credited, which its
// two rows move together (bookAtGross).
type bookingKey struct {
	account  string
	day      int64
	amount   string
	currency string
}

// newBookingKey is the key of amount moved on account on the day of at,
// in ccy. false where the amount is no booking (bookingCents).
func newBookingKey(account string, at int64, amount *canonical.Decimal, ccy string) (bookingKey, bool) {
	cents, ok := bookingCents(amount)
	if !ok {
		return bookingKey{}, false
	}
	return bookingKey{account: account, day: utcDay(at), amount: cents, currency: bookingCurrency(ccy)}, true
}

// txBookingKey is the key of a projected row: its account, day, signed
// net amount and currency.
func txBookingKey(tx canonical.TransactionChange) (bookingKey, bool) {
	return newBookingKey(tx.AccountExternalID, tx.OccurredAt, tx.NetAmount, tx.Currency)
}

// bookingCents renders a projected amount as the exact-match half of a
// bookingKey, rounded to the minor unit a bank books in so a figure a
// float column carried compares byte-for-byte with a decimal parsed from
// text.
//
// A zero amount is not a booking anything can be matched on: it names no
// sum, and a day can carry many zero-amount period-close lines that are
// unrelated to each other. ok=false excludes those from the fold, which
// costs nothing — a duplicated zero moves no total.
func bookingCents(amt *canonical.Decimal) (string, bool) {
	if amt == nil || amt.IsZero() {
		return "", false
	}
	r := amt.Round(2)
	if r.IsZero() {
		return "", false
	}
	return r.String(), true
}

// bookingCurrency normalises a currency for the key the way the adapter
// itself does: trimmed and upper-cased, and an absent one read as ISO
// 4217 "no currency involved", which is the fallback buildTransaction
// stamps on a feed row that carries none.
func bookingCurrency(ccy string) string {
	c := strings.ToUpper(strings.TrimSpace(ccy))
	if c == "" {
		return "XXX"
	}
	return c
}

// eraFold is the cross-era cash dedup: which web rows describe a booking
// another era also records, and what text each one leaves behind for the
// row that keeps it. The rows dropped are statement rows, and the export
// copies of a confirmation's cash leg.
type eraFold struct {
	// drop holds the emitted web keys (`<txn no>@<account>`) of the web
	// rows folded away.
	drop map[string]bool
	// web maps the emitted web key of a surviving export row to the text
	// of the statement row folded into it.
	web map[string]webTxText
	// psn maps the id of a surviving feed row (an MT940 line, a conversion
	// mirror, a confirmation's cash leg) to the text of the rows folded
	// into it.
	psn map[string]webTxText
	// withheld names the cash legs whose export copy is the record
	// instead: the leg reads no gross and the copy states one.
	withheld map[string]bool
}

// carry records text for the feed row to take. A second copy folded into
// the row fills only what the first left empty or as a bare code.
func (f *eraFold) carry(id string, text webTxText) {
	if have, ok := f.psn[id]; ok {
		text = have.or(text)
	}
	f.psn[id] = text
}

// psnHints is what the web-side transaction pass decided about rows the
// PSN stream will emit: ids to demote to a non-flow kind (the offset
// veto's PSN half) and text to carry onto a row whose statement or export
// duplicate the era fold dropped. Both are computed on the web side
// because both are decisions about a PAIR of rows that straddles the two
// silvers.
type psnHints struct {
	veto  map[string]bool
	carry map[string]webTxText
	// withheld names the rows the PSN stream books beside the feed that
	// an export row records instead: conversion mirrors
	// (buildSameDayOffsetVeto) and cash legs (buildEraFold). The stream
	// leaves them out.
	withheld map[string]bool
	// securities is what a dividend line is linked by past its
	// confirmation, and which reversal of a credit is a dividend
	// reversal (dividends.go), read over both silvers. The web pass's
	// offset veto reads the same value, so the veto and the stream
	// agree on which reversal is no deposit or withdrawal.
	securities *dividendSecurities
}

// webTxOutcome is what the web cash pass decided that a later pass
// needs. Both halves are by-products of the row loop rather than
// separate reads: the pass has already resolved every cut, fold and
// classification by the time it emits, and re-deriving any of that
// downstream would be re-deriving it from different inputs.
type webTxOutcome struct {
	// hints is what the PSN stream reads (psnHints).
	hints psnHints
	// settled counts the securities settlements the pass EMITTED, per
	// cash account, currency and settlement day. The portfolio pass
	// folds its own record of a trade against it — see
	// portfolioSettledDays.
	settled map[settledDayKey]int
}

// buildEraFold pairs each statement reconstruction with the export or feed
// record of the same booking, and each export copy of a confirmation's
// cash leg with the leg, so one booking reaches gold as one row.
//
// The statement archive, the account-statement export and the MT940 feed
// cover overlapping periods and share no id, so an entry printed on a
// statement and also exported (or fed) reaches the cash ledger twice —
// once under each era's id — and every total built over that ledger counts
// it twice. The fold is the cross-era identity the id schemes cannot
// express: same account, same value day, same signed amount, same
// currency.
//
// The statement copy is the one dropped. The export and the feed are the
// bank's own machine-readable record of the entry; the statement row is
// reconstructed from a printed document, one parse further from the bank.
// Which of the two survives is therefore not a per-row judgement — the era
// decides it. The one export row dropped is the copy of a cash leg
// (below).
//
// Two rows of the SAME era are never folded. Two identical payments on one
// day are an ordinary thing for a ledger to hold, and within one era they
// carry distinct ids because they are distinct bookings; only the
// cross-era signature says "recorded twice". Pairing is 1:1 and
// deterministic (ids sorted, exports before feed rows), so a day holding
// two statement rows and one export row folds exactly one of them and
// leaves the other standing.
//
// A confirmation's cash leg (cashLegKeys) is the feed's record of a
// payment on an account the feed does not speak for, where the cut gives
// the export every day. It is keyed on the amount it posted, which is what
// the export row moved. The export copy folds into it, 1:1, before any
// statement row is paired: the leg carries the confirmation's security,
// gross and tax. A statement copy then folds onto it as onto any feed row.
// Where the leg reads no gross and its export copy states one
// (webStatedGross), the copy is the record instead: it stays, at its
// gross, and the leg is withheld.
//
// The whole silver, unwindowed, for the reason buildSameDayOffsetVeto reads
// it whole: whether an entry is recorded twice depends only on silver's
// contents, never on which slice of time a load happens to cover. The
// per-relationship hard cut IS applied, because a row the cut suppresses is
// not in the ledger and must not consume a match.
func (r *webReader) buildEraFold(ctx context.Context, psn *psnReader, cut psnCut) (*eraFold, error) {
	out := &eraFold{
		drop:     map[string]bool{},
		web:      map[string]webTxText{},
		psn:      map[string]webTxText{},
		withheld: map[string]bool{},
	}
	if r == nil {
		return out, nil
	}

	legs, err := psn.cashLegKeys(ctx, cut.coverage)
	if err != nil {
		return nil, err
	}

	// foldRow is a web row the fold may drop, the text it leaves, and for
	// an export copy of a cash leg whether it states a gross.
	type foldRow struct {
		key     string
		text    webTxText
		grossed bool
	}
	statements := map[bookingKey][]foldRow{}
	exports := map[bookingKey][]foldRow{}
	err = r.eachWebTx(ctx, "ubs-web buildEraFold", "", nil, func(row webTxRow) error {
		if cut.excludes(row.account, row.valueDate) {
			return nil
		}
		// Classified by the same hint the transaction pass uses, so the
		// key carries the cash the row is emitted with: a dividend's
		// amount credited, before any gross.
		hint := webKindHint(row.descKind, row.counterparty)
		kind, _, net := webProjectedNet(hint, isStatementEraID(row.txID), row.debit, row.credit)
		k, ok := newBookingKey(row.account, row.valueDate, net, row.currency)
		if !ok {
			return nil
		}
		f := foldRow{key: row.emittedID()}
		exported := !isStatementEraID(row.txID)
		if exported && len(legs[k]) == 0 {
			exports[k] = append(exports[k], f)
			return nil
		}
		// A statement row and an export copy of a cash leg leave their text.
		p, pdfBackfill := decodeWebTxEra(row.payload)
		f.text, _, _ = projectWebTxText(row.counterparty.String, row.descKind.String, p, pdfBackfill)
		if !exported {
			statements[k] = append(statements[k], f)
			return nil
		}
		// The gross the export pass books the copy at (exportGross).
		_, f.grossed = exportGross(kind, p, pdfBackfill, row.currency, net)
		exports[k] = append(exports[k], f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for k, ls := range legs {
		exp := exports[k]
		sort.Slice(ls, func(i, j int) bool { return ls[i].id < ls[j].id })
		sort.Slice(exp, func(i, j int) bool { return exp[i].key < exp[j].key })
		n := min(len(ls), len(exp))
		var kept []foldRow
		for i := range n {
			if !ls[i].grossed && exp[i].grossed {
				out.withheld[ls[i].id] = true
				kept = append(kept, exp[i])
				continue
			}
			out.drop[exp[i].key] = true
			out.carry(ls[i].id, exp[i].text)
		}
		exports[k] = append(kept, exp[n:]...)
	}
	if len(statements) == 0 {
		return out, nil
	}

	feed := map[bookingKey][]string{}
	err = psn.eachCashMovement(ctx, "ubs-psn buildEraFold", func(row psnCashRow) error {
		// The cash the line moves, as the stream projects it
		// (psnCashRow.project).
		tx, _, ok := row.project()
		if !ok {
			return nil
		}
		if k, ok := txBookingKey(tx); ok {
			feed[k] = append(feed[k], row.eventID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A conversion mirror is the feed's record of the other account's
	// booking (conversionMirrors), and a statement copy of that booking
	// folds onto it like any other. Keyed as the mirror is emitted.
	mirrors, err := psn.conversionMirrors(ctx)
	if err != nil {
		return nil, err
	}
	for id, m := range mirrors.byID {
		if k, ok := txBookingKey(m.tx); ok {
			feed[k] = append(feed[k], id)
		}
	}
	for k, ls := range legs {
		for _, l := range ls {
			if !out.withheld[l.id] {
				feed[k] = append(feed[k], l.id)
			}
		}
	}

	for k, ss := range statements {
		exp, fd := exports[k], feed[k]
		if len(exp)+len(fd) == 0 {
			continue
		}
		sort.Slice(ss, func(i, j int) bool { return ss[i].key < ss[j].key })
		sort.Slice(exp, func(i, j int) bool { return exp[i].key < exp[j].key })
		sort.Strings(fd)
		for i, s := range ss {
			switch {
			case i < len(exp):
				out.drop[s.key] = true
				out.web[exp[i].key] = s.text
			case i-len(exp) < len(fd):
				out.drop[s.key] = true
				out.carry(fd[i-len(exp)], s.text)
			}
		}
	}
	return out, nil
}
