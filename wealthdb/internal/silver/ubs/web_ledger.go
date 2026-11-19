package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// transactionsBeforePSNStart emits web transactions strictly
// before the per-relationship PSN-start cutover. A hard cut (not
// an overlap merge) because the web and PSN sources use entirely
// different transaction_external_id schemes — web uses UBS
// Transaction No. (e.g. `0104030TXNNNNNNN`), PSN events use
// MT-prefixed strings (e.g. `mt515:...`). Any cross-source
// identity match would be heuristic and risk double-counting.
//
// Which is a statement about ROWS. The era text fold
// (psnWebTextFold) does match one feed's entry to the other's,
// on the bank's own transaction number, but only to fill a narrative
// column the MT940 row left as a bare code: it emits no row, drops
// none, and touches no amount, date, kind or id, so a false match
// costs one wrong narrative rather than a duplicated or vanished
// booking.
//
// The rows the reconcile table (reconcile.go) gives another record of
// the booking DO drop, and each of its rules decides on an exact
// identity rather than a heuristic on ids: a statement movement printed
// whole beside its split legs, an advice the ledger holds, a statement
// reconstruction whose account, value day, signed amount and currency
// match an export or feed record, an export copy of a confirmation's
// cash leg, and a row the MT940 feed holds under the bank's own number
// for the entry. Those
// verdicts reach the offset veto, which must not count a row nothing
// emits.
//
// The second return value is what this pass decided that the PSN stream
// needs (psnHints).
func (r *webReader) transactionsBeforePSNStart(ctx context.Context, w canonical.Window, psn *psnReader, rc *reconciliation) (silver.TransactionStream, psnHints, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), psnHints{}, nil
	}
	pass, err := r.newWebTxPass(ctx, psn, rc)
	if err != nil {
		return nil, psnHints{}, err
	}
	out := canonical.TransactionBatch{}
	zeros, folded := 0, 0
	err = r.eachWebRow(ctx, func(row *webRow) error {
		if row.valueDate < w.Start || row.valueDate > w.End {
			return nil
		}
		e, skip, err := pass.emit(row)
		switch {
		case err != nil:
			return err
		case skip == webFolded:
			folded++
			return nil
		case skip == webNoBooking:
			zeros++
			return nil
		case skip != webEmitted:
			return nil
		}
		out.Transactions = append(out.Transactions, e.tx)
		if e.withheld {
			out.Transactions = append(out.Transactions, e.tax)
		}
		return nil
	})
	if err != nil {
		return nil, psnHints{}, err
	}
	if zeros > 0 {
		log.Printf("ubs adapter: dropped %d zero-amount row(s): no money moved, not a booking", zeros)
	}
	if folded > 0 {
		log.Printf("ubs adapter: folded %d web row(s) into another record of the same booking — one booking, one row", folded)
	}
	return silver.NewTransactionStream(out), psnHints{veto: pass.psnVeto, securities: pass.dividends, rc: rc}, nil
}

// webTxPass is what the web cash pass resolves once per read, before it
// walks the rows: the reconcile table's verdict, the indexes a row is
// resolved against, and the veto that decides how a row is emitted.
type webTxPass struct {
	// rc decides which rows are emitted: the cut and the rest of the
	// reconcile table.
	rc       *reconciliation
	ownIBANs map[string]bool
	// valorToISIN turns the valor beside a statement-era trade's free
	// text into the instrument gold holds; such a trade carries no id.
	valorToISIN map[string]string
	// dividends links a dividend line that states no ISIN to its
	// security, by its valor or its ticker (dividends.go).
	dividends        *dividendSecurities
	mt940Start       int64
	mortgageAccounts map[string]string
	offsetVeto       map[string]bool
	psnVeto          map[string]bool
}

func (r *webReader) newWebTxPass(ctx context.Context, psn *psnReader, rc *reconciliation) (*webTxPass, error) {
	p := webTxPass{rc: rc}
	var err error
	if p.ownIBANs, err = r.buildOwnIBANSet(ctx); err != nil {
		return nil, err
	}
	if p.valorToISIN, err = r.valorIndex(ctx, psn); err != nil {
		return nil, err
	}
	if p.dividends, err = buildDividendSecurities(ctx, psn, r, p.valorToISIN); err != nil {
		return nil, err
	}
	if p.mt940Start, err = r.mt940FeedStart(ctx); err != nil {
		return nil, err
	}
	if p.mortgageAccounts, err = r.buildMortgageAccountIndex(ctx); err != nil {
		return nil, err
	}
	// The reconcile table's verdict comes BEFORE the offset veto: a web
	// row it gives another record is not in the ledger, so it must not
	// consume an offset-veto match either. The veto's universe is the
	// EMITTED rows, and a suppressed row is represented there by its PSN
	// counterpart; leave it in and it can win the mirror its counterpart
	// needed, demoting one leg of a pair while the other keeps its flow
	// kind — the one-sided phantom flow the veto exists to prevent.
	if err := r.recordExportGrosses(ctx, &p); err != nil {
		return nil, err
	}
	if p.offsetVeto, p.psnVeto, err = r.buildSameDayOffsetVeto(ctx, psn, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// project turns one web row into the transaction gold holds before any
// gross (emit), or reports false for a statement's period summary, which
// is not a booking.
func (p *webTxPass) project(row *webRow) (canonical.TransactionChange, bool) {
	pl, pdfBackfill := row.pl, row.pdfBackfill
	kind, net := row.kind, row.net
	// The row's own amount, so the transaction shares no value with the
	// ledger every pass reads.
	signed := *row.signed
	netAmount := &signed
	netPtr := net

	// newWebRow reads an undecodable payload as a PDF backfill,
	// which routes the row through pdfCashIsExternal (INTERNAL on the
	// zero value) rather than letting it skip the gate and keep its
	// deposit/withdrawal kind.
	if row.movesNothing() {
		return canonical.TransactionChange{}, false
	}
	statedMortgage := p.resolveCounterAccount(&pl)
	returnsInternal := p.returnsInternal(row, kind, pl, pdfBackfill)

	// The verdict is stamped here, before the sign is pinned: a
	// payload that cannot carry it demotes the kind instead
	// (markReturnsInternal), and the sign must then be read off the
	// kind the row ENDS with.
	rowPayload := withBankRef(withCounterAccount(
		withResolvedMortgage(json.RawMessage(row.payload), statedMortgage, pl.CounterAccount),
		pl.CounterAccount), webBankRef(row.txID))
	counterCcy, counterAmt := counterLegFromNarrative(pl)
	rowPayload = withCounterLeg(rowPayload, counterCcy, counterAmt)
	if returnsInternal {
		rowPayload, kind = markReturnsInternal(rowPayload, kind)
	}
	if !webReversal(row.descKind.String, isStatementEraID(row.txID), row.debit, row.credit) {
		netAmount = canonical.ApplyCanonicalSign(kind, &netPtr)
	}

	// The narrative columns, the instrument id and the payer's message
	// all fall out of one projection (projectWebTxText, which carries
	// the per-column contract); the era text fold (merge.go) reads the
	// same projection, so one booking's text is the same string
	// whichever side of the seam it reaches gold from. The kind is
	// already classified above and no text read here can move it.
	text, instrumentID, message := projectWebTxText(row.counterparty.String, row.descKind.String, pl, pdfBackfill)
	// The statement era's own road to the instrument. An id the export
	// era stated in Description1 outranks a looked-up one, and nothing
	// is added beside a resolved id — the instrument's own row answers
	// the taxonomy.
	var (
		assetClass canonical.AssetClass
		vehicle    canonical.Vehicle
		instrHint  string
	)
	switch {
	case instrumentID != nil:
		// The export stated it in Description1.
	case kind == canonical.TxKindDividend:
		// A dividend states its security's valor as `VN <valor>`, which
		// the statement parser does not promote; else the ticker closing
		// the narrative names it, else the valor the parser promoted.
		instrumentID, instrHint = p.dividends.webDividendSecurity(row.account, pl, pdfBackfill)
	default:
		instrumentID, instrHint = resolveValor(p.valorToISIN, pl.SecurityValor)
		// An unidentified security is not an unknown ASSET CLASS: what
		// a trade says it traded, where the booking type says it, draws
		// it in its real class rather than as an untracked destination.
		// Trades only — a cash movement whose narrative happens to
		// carry one of these tokens traded no security.
		if instrumentID == nil && (kind == canonical.TxKindBuy || kind == canonical.TxKindSell) {
			assetClass, vehicle = unlinkedSecurityTaxonomy(
				pl.BookingType, strings.Join(pl.Continuation, " ")+" "+text.description)
		}
	}
	// This export row kept a booking whose statement copy the reconcile
	// table dropped. Per column and only downward (richerText), the
	// statement's reading fills what this row left empty or as a bare
	// code; the amount, the value date, the kind and the id stay this
	// row's.
	if alt, ok := p.rc.carried(row); ok {
		text = text.or(alt)
	}
	description := silver.StrPtrIfNonEmpty(text.description)
	payee := silver.StrPtrIfNonEmpty(text.counterparty)
	category := silver.StrPtrIfNonEmpty(text.providerCategory)
	return canonical.TransactionChange{
		TransactionExternalID: row.emittedID(),
		OccurredAt:            row.valueDate,
		AccountExternalID:     row.account,
		InstrumentExternalID:  instrumentID,
		AssetClass:            assetClass,
		Vehicle:               vehicle,
		InstrumentHint:        instrHint,
		Kind:                  kind,
		Currency:              row.currency,
		NetAmount:             netAmount,
		Description:           description,
		Memo:                  silver.StrPtrIfNonEmpty(message),
		Counterparty:          payee,
		// The bank's own booking type, verbatim (a `;Reversal` suffix
		// included) — the closest thing a bank statement has to a
		// provider category, and what the spending provider tier
		// translates. The payer's message never enters it.
		ProviderCategory: category,
		Payload:          rowPayload,
	}, true
}

// webSkip is why the web pass emits nothing for a row (emit), or
// webEmitted where it emits one.
type webSkip int

const (
	webEmitted webSkip = iota
	// webCut: the row falls on the feed's side of the cut (psnCut).
	webCut
	// webFolded: another record of the booking is the one gold keeps
	// (reconcile).
	webFolded
	// webNoBooking: the row moves no money (movesNothing), no booking
	// (project).
	webNoBooking
)

// webEmission is what the web pass emits for one row: the transaction,
// and the withholding row beside it where it books a dividend at the
// gross its export row states (grossed, withheld).
type webEmission struct {
	tx, tax  canonical.TransactionChange
	grossed  bool
	withheld bool
}

// skip is why the pass emits nothing for a row whatever it projects to:
// the cut, else another rule of the reconcile table. webEmitted where
// none drops it. emit and the offset veto's web half (webOffsetLegs)
// both read it, so the veto counts no row the stream drops.
func (p *webTxPass) skip(row *webRow) webSkip {
	switch p.rc.webLostTo(row) {
	case "":
		return webEmitted
	case ruleCut:
		return webCut
	}
	return webFolded
}

// emit is what the pass emits for a web row: the reconcile table's
// verdict (skip), the projection, and an export dividend's gross. The
// stream and the record of grosses (recordExportGrosses) both read it, so
// they agree on every row.
func (p *webTxPass) emit(row *webRow) (webEmission, webSkip, error) {
	if skip := p.skip(row); skip != webEmitted {
		return webEmission{}, skip, nil
	}
	tx, ok := p.project(row)
	if !ok {
		return webEmission{}, webNoBooking, nil
	}
	e := webEmission{tx: tx}
	gross, ok := exportGross(tx.Kind, row.pl, row.pdfBackfill, tx.Currency, tx.NetAmount)
	if !ok {
		return e, webEmitted, nil
	}
	var err error
	e.grossed = true
	e.tax, e.withheld, err = bookAtGross(&e.tx, gross, "", "")
	return e, webEmitted, err
}

// recordExportGrosses records every export dividend credit the pass
// emits at a gross (emit), over every window. A reversal of the credit on
// the feed takes that gross back (reversedGross). Only a row the
// projection's own kind reads as a dividend credit can be one, so the
// rest are not projected. It needs the reconcile table's verdict (skip),
// not the offset veto: the veto changes only deposits and withdrawals.
func (r *webReader) recordExportGrosses(ctx context.Context, p *webTxPass) error {
	return r.eachWebRow(ctx, func(row *webRow) error {
		net, ok := row.dividendCredit()
		if !ok {
			return nil
		}
		e, skip, err := p.emit(row)
		if err != nil || skip != webEmitted || !e.grossed || e.tx.InstrumentExternalID == nil {
			return err
		}
		p.dividends.addBooked(*e.tx.InstrumentExternalID, row.account, row.currency, *net, row.valueDate, *e.tx.GrossAmount)
		return nil
	})
}

// resolveCounterAccount fills the row's counter account from whichever
// era stated it, and returns the mortgage stamp it replaced, if any.
//
// The statement parser fills the field; the export feed states the same
// fact in free text (counterAccountFromNarrative), and deriving it here
// lets one field answer for both eras, downstream and in the payload.
//
// A mortgage payment names its mortgage rather than an IBAN. The export
// era states it in a field the composed description drops; the statement
// era fills the counter account with the stamp's own text — the
// mortgage's name, not the id silver holds it under. Either way the stamp
// runs through the mortgage index and the id replaces the name. Only a
// mortgage silver already holds resolves.
func (p *webTxPass) resolveCounterAccount(pl *webTxPayload) (statedMortgage string) {
	if pl.CounterAccount == "" {
		pl.CounterAccount = counterAccountFromNarrative(*pl)
	}
	if k := mortgageRefFromNarrative(pl.CounterAccount); k != "" {
		if id := p.mortgageAccounts[k]; id != "" {
			statedMortgage, pl.CounterAccount = pl.CounterAccount, id
		}
	} else if pl.CounterAccount == "" {
		if k := mortgageRefFromNarrative(pl.Description2); k != "" {
			pl.CounterAccount = p.mortgageAccounts[k]
		}
	}
	return statedMortgage
}

// returnsInternal classifies a deposit or withdrawal as INTERNAL (conduit
// churn) rather than EXTERNAL (owner capital crossing the boundary).
// A row is internal when:
//
//  1. the same-day offset veto (buildSameDayOffsetVeto) found its mirror
//     on another own account, in either feed;
//  2. its counter account is one the relationship owns, whatever the
//     era — demote-only, so a KNOWN own counter can take a row out of
//     the flow series but never put one in; or
//  3. it is a PDF backfill that pdfCashIsExternal does not promote
//     (default INTERNAL; its doc carries the conduit model).
//
// The verdict travels as its OWN flag rather than a rewritten kind.
// "Is this owner capital crossing the boundary?" (returns) and "is this
// a spending row?" (the spending population selects on kind) are two
// questions: a card purchase is not owner capital, and it is spending.
func (p *webTxPass) returnsInternal(row *webRow, kind canonical.TxKind, pl webTxPayload, pdfBackfill bool) bool {
	if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
		return false
	}
	railEra := p.mt940Start > 0 && row.valueDate >= p.mt940Start
	return p.offsetVeto[row.emittedID()] ||
		p.ownIBANs[normalizeIBAN(pl.CounterAccount)] ||
		pdfBackfill && !pdfCashIsExternal(pl, p.ownIBANs, kind == canonical.TxKindWithdrawal, railEra)
}

// webTxRow is the columns of one row of the web silver's transactions
// table, which webRow carries decoded.
type webTxRow struct {
	txID, account, currency, payload string
	valueDate                        int64
	bookingDate                      sql.NullInt64
	debit, credit                    sql.NullFloat64
	counterparty, descKind           sql.NullString
}

// movesNothing reports whether the row moves no money and is no
// booking. A statement line of amount zero moves nothing, whatever it
// says: a service-price close, a low booking amount, or the "Turnover
// total" line a statement prints before its closing balance, which the
// collector's parser attaches to the booking that precedes it. Silver
// keeps them as printed. The same lines under a real amount are
// bookings and are kept (bookingLines; docs/adapters/ubs.md §7). The CSV
// feed prints a period close as a zero-amount service-price row with no
// booking type, and an advice's zero line restates a summary.
func (row *webRow) movesNothing() bool {
	return row.net.IsZero() && (isStatementEraID(row.txID) ||
		row.pdfBackfill && isStatementSummary(row.pl) ||
		!row.pdfBackfill && isServicePriceClose(row.hint))
}

// emittedID is the id the row reaches gold under. Web silver keys a
// transaction by (number, account), so each leg of an FX trade or other
// multi-leg event is its own row; gold keys by the id alone, so the
// account is folded in. The bank's "Transaction no." stays in the
// payload for queries that reassemble a trade.
func (row webTxRow) emittedID() string { return row.txID + "@" + row.account }

// dividendCredit is the cash a web row credits where its projection
// (webProjectedNet) reads it as a dividend that credits; false for any
// other row.
func (row *webRow) dividendCredit() (*canonical.Decimal, bool) {
	if row.kind != canonical.TxKindDividend || row.signed == nil || !row.signed.IsPositive() {
		return nil, false
	}
	return row.signed, true
}
