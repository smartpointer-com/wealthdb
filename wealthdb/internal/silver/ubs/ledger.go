package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The two cash rails, each read once per connection.
//
// The web ledger (the ubs-web transactions table) and the PSN events
// are read whole by most passes of a load: the folds, the offset veto,
// the dividend records and the streams. A connection serves one load
// over silvers that do not change under it, so each rail is read once,
// in the silver's own row order, and its payloads decoded once; every
// pass walks the rows. An absent rail has no rows.

// webRow is one row of the web ledger as every pass reads it: the
// columns, the payload decoded, and the kind and cash the booking type
// and the amount cells give it (webProjectedNet).
type webRow struct {
	webTxRow
	// pl is the payload decoded (decodeWebTxPayload); decoded is false
	// where it does not decode, and pl is then the zero value.
	pl      webTxPayload
	decoded bool
	// pdfBackfill is whether the row is read as a PDF backfill
	// (newWebRow).
	pdfBackfill bool
	// hint is what the kind is classified by (webKindHint); kind, net and
	// signed are webProjectedNet over it.
	hint   string
	kind   canonical.TxKind
	net    canonical.Decimal
	signed *canonical.Decimal
}

// newWebRow decodes and projects one row of the web ledger.
//
// It also says whether the row is read as a PDF backfill, and every pass
// reads that here, because the conservative rule it holds — AN
// UNDECODABLE PAYLOAD IS TREATED AS A BACKFILL — belongs in one place:
// re-spelled per pass it is one edit away from a pass that decodes,
// ignores the failure, and takes the permissive branch on the classifier
// that keeps owner capital from being fabricated.
func newWebRow(row webTxRow) webRow {
	out := webRow{webTxRow: row}
	out.pl, out.decoded = decodeWebTxPayload(row.payload)
	out.pdfBackfill = !out.decoded || isPDFCashBackfill(out.pl)
	out.hint = webKindHint(row.descKind, row.counterparty)
	statementEra := isStatementEraID(row.txID)
	out.kind, out.net, out.signed = webProjectedNet(out.hint, statementEra, exportCells(statementEra, out.pdfBackfill),
		row.debit, row.credit)
	return out
}

// exportCells reports whether a row's amount cells are the export's,
// signed by the sheet: a row neither reconstructed from a statement nor
// read from a printed document, whose figures are as printed (an
// advice's among them, which carries the backfill's source marker).
func exportCells(statementEra, pdfBackfill bool) bool { return !statementEra && !pdfBackfill }

// ledger is the web ledger, read on the first call; later calls return
// the same rows. A reader with no silver behind it has none.
func (r *webReader) ledger(ctx context.Context) ([]webRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	return r.rows.Get(func() ([]webRow, error) { return r.readWeb(ctx) })
}

// readWeb is ledger's read.
func (r *webReader) readWeb(ctx context.Context) ([]webRow, error) {
	var out []webRow
	err := silver.ScanRows(ctx, r.db, "ubs-web ledger", `
SELECT transaction_external_id, value_date, account_external_id, currency_iso,
       amount_debit, amount_credit, counterparty, description_kind, payload
  FROM transactions`, func(rows *sql.Rows) error {
		var row webTxRow
		if err := rows.Scan(&row.txID, &row.valueDate, &row.account, &row.currency,
			&row.debit, &row.credit, &row.counterparty, &row.descKind, &row.payload); err != nil {
			return fmt.Errorf("ubs-web ledger scan: %w", err)
		}
		out = append(out, newWebRow(row))
		return nil
	})
	return out, err
}

// eachWebRow calls fn on each row of the web ledger, in the silver's
// row order.
func (r *webReader) eachWebRow(ctx context.Context, fn func(*webRow) error) error {
	rows, err := r.ledger(ctx)
	if err != nil {
		return err
	}
	for i := range rows {
		if err := fn(&rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// feedRow is one event of the PSN silver as every pass reads it. A cash
// movement also carries its payload decoded and the transaction the
// stream projects it to (project).
type feedRow struct {
	eventID, account, kind, payload string
	at                              int64
	currency                        sql.NullString

	// pl is a cash movement's payload decoded; decoded is false where it
	// does not decode.
	pl      cashMovementPayload
	decoded bool
	// tx is a cash movement as buildTransaction projects it; projected
	// is false where it projects to no transaction with an amount.
	tx        canonical.TransactionChange
	projected bool
}

// psnFeed is the PSN events, in the silver's row order, and the cash
// movements among them.
type psnFeed struct {
	events []feedRow
	cash   []*feedRow
}

// feed is the PSN events, read on the first call; later calls return the
// same rows. A reader with no silver behind it has none.
func (r *psnReader) feed(ctx context.Context) (psnFeed, error) {
	if r == nil || r.db == nil {
		return psnFeed{}, nil
	}
	return r.events.Get(func() (psnFeed, error) { return r.readFeed(ctx) })
}

// readFeed is feed's read.
func (r *psnReader) readFeed(ctx context.Context) (psnFeed, error) {
	var out psnFeed
	err := silver.ScanRows(ctx, r.db, "ubs-psn events", `
SELECT event_external_id, timestamp, account_external_id, kind, currency_iso, payload
  FROM events`, func(rows *sql.Rows) error {
		var row feedRow
		if err := rows.Scan(&row.eventID, &row.at, &row.account, &row.kind, &row.currency, &row.payload); err != nil {
			return fmt.Errorf("ubs-psn events scan: %w", err)
		}
		if row.kind == "cash_movement" {
			row.decoded = json.Unmarshal([]byte(row.payload), &row.pl) == nil
			row.tx, row.projected = row.projectCash()
		}
		out.events = append(out.events, row)
		return nil
	})
	if err != nil {
		return psnFeed{}, err
	}
	for i := range out.events {
		if out.events[i].kind == "cash_movement" {
			out.cash = append(out.cash, &out.events[i])
		}
	}
	return out, nil
}

// projectCash is the cash movement as the transaction stream emits it
// (buildTransaction), before any pass reclassifies it or books it at a
// gross. false for a payload that does not decode or states no amount.
func (row *feedRow) projectCash() (canonical.TransactionChange, bool) {
	if !row.decoded {
		return canonical.TransactionChange{}, false
	}
	var ccy *string
	if row.currency.Valid {
		c := row.currency.String
		ccy = &c
	}
	tx, err := buildTransaction(row.eventID, row.at, row.account, "cash_movement", ccy, row.payload, nil)
	if err != nil || tx.NetAmount == nil {
		return canonical.TransactionChange{}, false
	}
	return tx, true
}

// project is the cash movement as the transaction stream emits it, and
// its decoded payload. Every pass that keys on a feed line reads it
// here, so each reads the account, currency, kind and signed cash the
// stream emits. false where the line projects to no transaction.
func (row *feedRow) project() (canonical.TransactionChange, cashMovementPayload, bool) {
	if !row.projected {
		return canonical.TransactionChange{}, row.pl, false
	}
	return row.tx, row.pl, true
}

// dividendCredit is the row projected (project) where it is a dividend
// line that credits, marked `C`; false for any other line.
func (row *feedRow) dividendCredit() (canonical.TransactionChange, cashMovementPayload, bool) {
	tx, p, ok := row.project()
	if !ok || tx.Kind != canonical.TxKindDividend || p.CreditDebit != "C" {
		return canonical.TransactionChange{}, cashMovementPayload{}, false
	}
	return tx, p, true
}

// eachCashMovement calls fn on each cash movement of the feed, in the
// silver's row order.
func (r *psnReader) eachCashMovement(ctx context.Context, fn func(*feedRow) error) error {
	f, err := r.feed(ctx)
	if err != nil {
		return err
	}
	for _, row := range f.cash {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}
