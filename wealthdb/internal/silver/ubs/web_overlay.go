package ubs

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Web-side overlay helpers. The orchestrator pre-fetches per-key
// web payloads once and hands them to the PSN-side fold stream
// (merge.go) so the cross-source join lives in one place.

// webPosKey is the per-(UTC date, ISIN) match key for the
// position-payload overlay. Web silver flattens all securities
// under a single portfolio, so (account, portfolio, ISIN) would be
// ambiguous. ISIN alone
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
	out := make(map[webPosKey]string)
	if err := silver.ScanRows(ctx, r.db, "web positionPayloadByKey", q, func(rows *sql.Rows) error {
		var snap int64
		var isin, payload string
		if err := rows.Scan(&snap, &isin, &payload); err != nil {
			return err
		}
		out[webPosKey{utcDate: utcDay(snap), isin: isin}] = payload
		return nil
	}, wStart, wEnd); err != nil {
		return nil, err
	}
	return out, nil
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
	err := r.eachWebRow(ctx, func(row *webRow) error {
		key := webTxTextKey{account: row.account, txnNo: webTxNumber(row.txID)}
		if held, taken := won[key]; taken {
			if held == key.txnNo || (row.txID != key.txnNo && row.txID > held) {
				return nil
			}
		}
		text, _, _ := projectWebTxText(row.counterparty.String, row.descKind.String, row.pl, row.pdfBackfill)
		out[key], won[key] = text, row.txID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- the booking key -----------------------------------------------------

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
// them (webProjectedNet for the two web eras, feedRow.transaction for the
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
// unrelated to each other. ok=false excludes those from every match, which
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
// 4217 "no currency involved", which is the fallback feedRow.transaction
// stamps on a feed row that carries none.
func bookingCurrency(ccy string) string {
	c := strings.ToUpper(strings.TrimSpace(ccy))
	if c == "" {
		return "XXX"
	}
	return c
}

// psnHints is what the web-side transaction pass decided about rows the
// PSN stream will emit: ids to demote to a non-flow kind (the offset
// veto's PSN half), computed on the web side because it is a decision
// about a PAIR of rows that straddles the two silvers, and the reconcile
// table's verdict over both silvers.
type psnHints struct {
	veto map[string]bool
	// securities is what a dividend line is linked by past its
	// confirmation, and which reversal of a credit is a dividend
	// reversal (dividends.go), read over both silvers. The web pass's
	// offset veto reads the same value, so the veto and the stream
	// agree on which reversal is no deposit or withdrawal.
	securities *dividendSecurities
	// funds is what a cash line no other road links to an instrument is
	// read against: the private-equity funds its account's portfolios
	// hold, and the cash lines the funds' corporate actions pair with
	// (private_funds.go), read over both silvers.
	funds *privateFunds
	// rc is the reconcile table's verdict: which feed lines give way
	// to a confirmation, which mirrors and cash legs another record
	// carries instead (withheld), and the text a dropped copy leaves
	// on the record that kept its booking. Never nil where the window
	// has changes.
	rc *reconciliation
}
