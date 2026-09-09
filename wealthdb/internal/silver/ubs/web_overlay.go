package ubs

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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
	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, account_external_id, counterparty, description_kind, payload
  FROM transactions`)
	if err != nil {
		return nil, fmt.Errorf("ubs-web transactionTextByKey: %w", err)
	}
	defer rows.Close()
	out := map[webTxTextKey]webTxText{}
	for rows.Next() {
		var (
			txID, acct, payload   string
			counterparty, kindStr sql.NullString
		)
		if err := rows.Scan(&txID, &acct, &counterparty, &kindStr, &payload); err != nil {
			return nil, fmt.Errorf("ubs-web transactionTextByKey scan: %w", err)
		}
		p, pdfBackfill := decodeWebTxEra(payload)
		text, _, _ := projectWebTxText(counterparty.String, kindStr.String, p, pdfBackfill)
		out[webTxTextKey{account: acct, txnNo: txID}] = text
	}
	return out, rows.Err()
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
// feed). What the key compares is therefore what gold would hold.
type bookingKey struct {
	account  string
	day      int64
	amount   string
	currency string
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

// eraFold is the cross-era cash dedup: which statement rows describe a
// booking another era also records, and what text each one leaves behind
// for the row that keeps it.
type eraFold struct {
	// drop holds the emitted web keys (`<txn no>@<account>`) of statement
	// rows folded away.
	drop map[string]bool
	// web maps the emitted web key of a surviving export row to the text
	// of the statement row folded into it.
	web map[string]webTxText
	// psn maps a surviving MT940 event id to the same.
	psn map[string]webTxText
}

// psnHints is what the web-side transaction pass decided about rows the
// PSN stream will emit: ids to demote to a non-flow kind (the offset
// veto's PSN half) and text to carry onto a row whose statement duplicate
// the era fold dropped. Both are computed on the web side because both
// are decisions about a PAIR of rows that straddles the two silvers.
type psnHints struct {
	veto  map[string]bool
	carry map[string]webTxText
}

// buildEraFold pairs each statement reconstruction with the export or feed
// record of the same booking, so one booking reaches gold as one row.
//
// The statement archive, the account-statement export and the MT940 feed
// cover overlapping periods and share no id, so an entry printed on a
// statement and also exported (or fed) reaches the cash ledger twice —
// once under each era's id — and every total built over that ledger counts
// it twice. The fold is the cross-era identity the id schemes cannot
// express: same account, same value day, same signed amount, same
// currency.
//
// The statement copy is the one dropped, always. The export and the feed
// are the bank's own machine-readable record of the entry; the statement
// row is reconstructed from a printed document, one parse further from the
// bank. Which of the two survives is therefore not a per-row judgement —
// the era decides it.
//
// Two rows of the SAME era are never folded. Two identical payments on one
// day are an ordinary thing for a ledger to hold, and within one era they
// carry distinct ids because they are distinct bookings; only the
// cross-era signature says "recorded twice". Pairing is 1:1 and
// deterministic (ids sorted, exports before feed rows), so a day holding
// two statement rows and one export row folds exactly one of them and
// leaves the other standing.
//
// The whole silver, unwindowed, for the reason buildSameDayOffsetVeto reads
// it whole: whether an entry is recorded twice depends only on silver's
// contents, never on which slice of time a load happens to cover. The
// per-relationship hard cut IS applied, because a row the cut suppresses is
// not in the ledger and must not consume a match.
func (r *webReader) buildEraFold(ctx context.Context, psn *psnReader, cutoff map[string]int64, accountToRel map[string]string) (*eraFold, error) {
	out := &eraFold{
		drop: map[string]bool{},
		web:  map[string]webTxText{},
		psn:  map[string]webTxText{},
	}
	if r == nil {
		return out, nil
	}

	type statementRow struct {
		key  string
		text webTxText
	}
	statements := map[bookingKey][]statementRow{}
	exports := map[bookingKey][]string{}

	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, account_external_id, value_date,
       currency_iso, amount_debit, amount_credit, counterparty,
       description_kind, payload
  FROM transactions`)
	if err != nil {
		return nil, fmt.Errorf("ubs-web buildEraFold: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			txID, acct, ccy, payload string
			valueDate                int64
			debit, credit            sql.NullFloat64
			counterparty, kindStr    sql.NullString
		)
		if err := rows.Scan(&txID, &acct, &valueDate, &ccy, &debit, &credit,
			&counterparty, &kindStr, &payload); err != nil {
			return nil, fmt.Errorf("ubs-web buildEraFold scan: %w", err)
		}
		// Emitted-universe filter: mirror the transaction loop's hard cut
		// at the per-relationship PSN cutover.
		if rel, ok := accountToRel[acct]; ok {
			if cut := cutoff[rel]; cut > 0 && valueDate >= cut {
				continue
			}
		}
		_, _, net := webProjectedNet(kindStr.String, isStatementEraID(txID), debit, credit)
		amount, ok := bookingCents(net)
		if !ok {
			continue
		}
		k := bookingKey{
			account:  acct,
			day:      utcDay(valueDate),
			amount:   amount,
			currency: bookingCurrency(ccy),
		}
		emitted := txID + "@" + acct
		if !isStatementEraID(txID) {
			exports[k] = append(exports[k], emitted)
			continue
		}
		p, pdfBackfill := decodeWebTxEra(payload)
		text, _, _ := projectWebTxText(counterparty.String, kindStr.String, p, pdfBackfill)
		statements[k] = append(statements[k], statementRow{key: emitted, text: text})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(statements) == 0 {
		return out, nil
	}

	feed := map[bookingKey][]string{}
	if psn != nil {
		prows, err := psn.db.QueryContext(ctx, `
SELECT event_external_id, timestamp, account_external_id, currency_iso, payload
  FROM events
 WHERE kind = 'cash_movement'`)
		if err != nil {
			return nil, fmt.Errorf("ubs-psn buildEraFold: %w", err)
		}
		defer prows.Close()
		for prows.Next() {
			var (
				eventID, acct string
				ccy           sql.NullString
				ts            int64
				payload       string
			)
			if err := prows.Scan(&eventID, &ts, &acct, &ccy, &payload); err != nil {
				return nil, fmt.Errorf("ubs-psn buildEraFold scan: %w", err)
			}
			// The feed row is projected by the very builder the PSN
			// transaction stream emits from, so the account, currency and
			// signed amount the key carries are the ones gold would hold —
			// MT940's positive-figure-plus-direction convention resolved
			// exactly once, in the one place that owns it.
			var ccyPtr *string
			if ccy.Valid {
				c := ccy.String
				ccyPtr = &c
			}
			tx, err := buildTransaction(eventID, ts, acct, "cash_movement", ccyPtr, payload)
			if err != nil {
				continue
			}
			amount, ok := bookingCents(tx.NetAmount)
			if !ok {
				continue
			}
			k := bookingKey{
				account:  tx.AccountExternalID,
				day:      utcDay(tx.OccurredAt),
				amount:   amount,
				currency: bookingCurrency(tx.Currency),
			}
			feed[k] = append(feed[k], eventID)
		}
		if err := prows.Err(); err != nil {
			return nil, err
		}
	}

	for k, ss := range statements {
		exp, fd := exports[k], feed[k]
		if len(exp)+len(fd) == 0 {
			continue
		}
		sort.Slice(ss, func(i, j int) bool { return ss[i].key < ss[j].key })
		sort.Strings(exp)
		sort.Strings(fd)
		for i, s := range ss {
			switch {
			case i < len(exp):
				out.drop[s.key] = true
				out.web[exp[i]] = s.text
			case i-len(exp) < len(fd):
				out.drop[s.key] = true
				out.psn[fd[i-len(exp)]] = s.text
			}
		}
	}
	return out, nil
}
