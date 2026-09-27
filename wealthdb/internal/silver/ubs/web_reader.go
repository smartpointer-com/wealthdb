package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// webReader reads from the ubs-web silver SQLite. It owns
// the splice cutoff: every record passed downstream has either no
// known PSN counterpart (no cutoff) or a value/snapshot timestamp
// strictly less than PSN-start for its banking relationship.
//
// PSN-start per relationship is derived per Snapshots/Transactions
// call from the configured RelationshipPair list and the *psnReader
// handle (buildPSNStartByWebRel).
type webReader struct {
	db *sql.DB
}

func (r *webReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// Status mirrors the canonical.Status contract using web silver
// columns. Snapshot times come from `dump_runs`; transaction
// times come from `transactions.value_date`. The LatestChange-
// Number is MAX of either (whichever advanced).
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	// dump_runs always has at least one row per snapshot, even
	// when transactions/documents tables are empty for that run.
	err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(snapshot_at), -1),
               COALESCE(MAX(snapshot_at), -1)
          FROM dump_runs`).Scan(&out.OldestSnapshotAt, &out.LatestSnapshotAt)
	if err != nil {
		return canonical.Status{}, fmt.Errorf("ubs-web Status snapshots: %w", err)
	}
	err = r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(value_date), -1),
               COALESCE(MAX(value_date), -1)
          FROM transactions`).Scan(&out.OldestTransactionAt, &out.LatestTransactionAt)
	if err != nil {
		return canonical.Status{}, fmt.Errorf("ubs-web Status transactions: %w", err)
	}
	// Historical (PDF) snapshots extend OldestSnapshotAt back —
	// PDFs cover dates that pre-date the first live web dump.
	histLo, histHi, err := r.historicalRange(ctx)
	if err != nil {
		return canonical.Status{}, err
	}
	if histLo >= 0 && (out.OldestSnapshotAt == -1 || histLo < out.OldestSnapshotAt) {
		out.OldestSnapshotAt = histLo
	}
	if histHi > out.LatestSnapshotAt {
		out.LatestSnapshotAt = histHi
	}
	out.LatestChangeNumber = max(out.LatestSnapshotAt, out.LatestTransactionAt)
	return out, nil
}

// ChangeWindow returns the union of new snapshots and new
// transactions strictly after `since`. Matches the PSN reader's
// convention so the merge layer's combine is straightforward.
//
// Historical (PDF-reconstructed) data extends Start backwards
// when there's any new live content. The historical_position_
// snapshots and historical_cash_balances tables key on
// as_of_date / period_end / period_start (well before the live
// dump_run watermark), so if the loader's delete-then-insert
// window stayed at live-Start, re-emitting historical rows on
// the next load would collide on the gold PK. Including the
// historical range in Start guarantees the window-DELETE covers
// any existing historical gold rows before they're re-inserted.
//
// NewChangeNumber stays a live-time concept (MAX over snapshot_at
// / value_date) — the watermark advances only when live data
// advances. This means re-running with no new dump_run is a
// no-op even when historical data is present.
func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	lo, hi, err := r.span(ctx, "ubs-web ChangeWindow", []string{
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM dump_runs WHERE snapshot_at > ?`,
		`SELECT MIN(value_date), MAX(value_date) FROM transactions WHERE value_date > ?`,
	}, since)
	if err != nil || lo < 0 {
		return canonical.Window{NewChangeNumber: since}, err
	}
	w := canonical.Window{HasChanges: true, Start: lo, End: hi, NewChangeNumber: max(since, hi)}
	// Cards emit at their own dates — a billing period ends on a date no
	// dump run need share — so the window has to reach them or gold's
	// delete-then-reinsert would leave duplicates behind. The managed
	// portfolios' trades are the same case, reaching years back past the
	// oldest live dump.
	for _, extra := range []func(context.Context) (int64, int64, error){
		r.historicalRange, r.cardRange, r.portfolioTxnRange,
	} {
		lo, hi, err := extra(ctx)
		if err != nil {
			return canonical.Window{}, err
		}
		if lo >= 0 && lo < w.Start {
			w.Start = lo
		}
		if hi > w.End {
			w.End = hi
		}
	}
	return w, nil
}

// span runs MIN/MAX queries, each with the same args, and returns the
// widest range they report, or (-1, -1) when every one is empty.
func (r *webReader) span(ctx context.Context, what string, queries []string, args ...any) (lo, hi int64, err error) {
	lo, hi = -1, -1
	for _, q := range queries {
		var a, b sql.NullInt64
		if err := r.db.QueryRowContext(ctx, q, args...).Scan(&a, &b); err != nil {
			return -1, -1, fmt.Errorf("%s: %w", what, err)
		}
		if a.Valid && (lo < 0 || a.Int64 < lo) {
			lo = a.Int64
		}
		if b.Valid && b.Int64 > hi {
			hi = b.Int64
		}
	}
	return lo, hi, nil
}

// snapshotsForOverlap emits dimensions (portfolios, accounts,
// instruments) from web's live snapshots inside the window, with
// each row filtered to snapshot_at < PSN-start for its banking
// relationship. Positions and cash flow through PSN's snapshot
// stream instead; the orchestrator wraps PSN with a fold stream
// that injects web's per-(date, key) payload into PSN's rows.
//
// This split keeps PSN's faithful safekeeping / portfolio
// structure (web silver flattens all securities under one
// PrtflId) while still preserving web-only
// fields like cost_price inside `payload.web`. Web's instrument
// descriptions are a strict upgrade over PSN's InstrNm.LngNm-
// English (which is colon-formatted and less user-readable), so
// they're emitted here and let the per-column upsert guard pick
// the latest snapshot's name.
//
// Historical PDF snapshots are handled by snapshotsHistorical
// (historical.go); transactions are handled by
// transactionsBeforePSNStart.
func (r *webReader) snapshotsForOverlap(
	ctx context.Context,
	w canonical.Window,
	cutoffByWebRel map[string]int64,
	psnTaxPair map[string]taxPair,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := r.dumpRunTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	// Dimensions only — portfolios, accounts, instruments. Positions / cash
	// come from the PSN side (with web payload folded in by the orchestrator).
	if err := r.appendWebPortfolios(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebAccounts(ctx, w, byTime, cutoffByWebRel); err != nil {
		return nil, err
	}
	if err := r.appendWebInstruments(ctx, w, byTime, psnTaxPair); err != nil {
		return nil, err
	}

	// Mortgages: unique to the web side — PSN doesn't carry them
	// at all. They flow through regardless of the PSN cutoff;
	// emitted as a triple per row (account + instrument +
	// position) so they show up as a single negative-value
	// liability holding in gold positions.
	if err := r.appendWebMortgages(ctx, w, byTime); err != nil {
		return nil, err
	}

	// Cards: web-only for the same reason as mortgages — PSN carries
	// none — so they too flow through regardless of the PSN cutoff. The
	// roster gives the current balance; the invoices give the history,
	// at the billing dates the bank drew them on rather than at the
	// dates a run happened to fetch them, so appendCardStatementBalances
	// may add batch times of its own.
	if err := r.appendWebCards(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := r.appendCardStatementBalances(ctx, w, byTime); err != nil {
		return nil, err
	}

	// Re-read the batch times: the statement balances above can add a
	// period end that was not a dump time.
	allTimes := make([]int64, 0, len(byTime))
	for t := range byTime {
		allTimes = append(allTimes, t)
	}
	sort.Slice(allTimes, func(i, j int) bool { return allTimes[i] < allTimes[j] })

	batches := make([]canonical.SnapshotBatch, 0, len(allTimes))
	for _, t := range allTimes {
		b := byTime[t]
		if len(b.Portfolios)+len(b.Accounts)+len(b.Instruments)+
			len(b.Positions)+len(b.CashBalances) == 0 {
			continue
		}
		batches = append(batches, *b)
	}
	return silver.NewSnapshotStream(batches), nil
}

// appendWebInstruments emits one InstrumentChange per (snapshot,
// ISIN) in the window using the web positions.description as the
// instrument's user-facing Name. ISINs that appear multiple
// times in the same snapshot (cross-portfolio holdings) coalesce
// to one emission with the description from the first row seen —
// the descriptions don't vary by portfolio.
//
// psnTaxPair maps ISIN → PSN's CFI/UAC-derived (asset_class, vehicle)
// pair. Web doesn't know an instrument's taxonomy (no CFI), and a
// naive (other, other) emission would overwrite PSN's specific pair
// via the per-column upsert guard (web's last_seen_at is typically
// later than PSN's). Stamping PSN's pair keeps the cross-source upsert
// idempotent on asset_class / vehicle while letting web win on Name.
// ISINs PSN has never seen fall back to the description-template
// classifier (taxonomyPairForWebDescription), then (other, other).
func (r *webReader) appendWebInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, psnTaxPair map[string]taxPair) error {
	const q = `
SELECT snapshot_at, instrument_isin, currency_iso, description
  FROM positions
 WHERE instrument_isin IS NOT NULL
   AND snapshot_at BETWEEN ? AND ?
 GROUP BY snapshot_at, instrument_isin`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebInstruments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap        int64
			isin        string
			ccy         string
			description sql.NullString
		)
		if err := rows.Scan(&snap, &isin, &ccy, &description); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		acNew := canonical.AssetClassOther
		vehicle := canonical.VehicleOther
		if tp, ok := psnTaxPair[isin]; ok && tp.AssetClass != "" {
			acNew = tp.AssetClass
			vehicle = tp.Vehicle
		} else if ac, veh, ok := taxonomyPairForWebDescription(description.String); ok {
			acNew, vehicle = ac, veh
		}
		isinCopy := isin
		// UBS web descriptions encode the listing ticker in
		// trailing parens — e.g. "Reg.shs Example AG
		// (XMPL)". Extract it and surface as the instrument's
		// Symbol so dividend / coupon transactions joined by
		// ISIN get a populated symbol column. Descriptions
		// without a trailing (TICKER) (ETFs identified only by
		// long-form name) keep Symbol nil.
		symbol := tickerFromDescription(description.String)
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			ISIN:                 &isinCopy,
			Symbol:               symbol,
			Name:                 silver.StrPtrIfNonEmpty(description.String),
			Currency:             silver.StrPtrIfNonEmpty(ccy),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
	}
	return rows.Err()
}

// transactionsBeforePSNStart emits web transactions strictly
// before the per-relationship PSN-start cutover. A hard cut (not
// an overlap merge) because the web and PSN sources use entirely
// different transaction_external_id schemes — web uses UBS
// Transaction No. (e.g. `0104030TXNNNNNNN`), PSN events use
// MT-prefixed strings (e.g. `mt515:...`). Any cross-source
// identity match would be heuristic and risk double-counting.
//
// Which is a statement about ROWS. The era text fold
// (psnWebTextFoldStream) does match one feed's entry to the other's,
// on the bank's own transaction number, but only to fill a narrative
// column the MT940 row left as a bare code: it emits no row, drops
// none, and touches no amount, date, kind or id, so a false match
// costs one wrong narrative rather than a duplicated or vanished
// booking.
//
// Two folds DO drop a web row for being a second record of a booking,
// and both decide on an exact identity rather than a heuristic on ids.
// The era fold (buildEraFold) works inside the web silver's own two
// eras against the machine-readable records: a statement reconstruction
// whose account, value day, signed amount and currency match an export
// or MT940 row is not emitted, because the row that matched it already
// carries the booking; it never folds two rows of one era. The seam
// (buildSeamBankRefs) covers the days the first PSN dump's statements
// reach back over, where the cut excludes neither copy, and matches on
// the bank's own number for the entry. Both verdicts reach the offset
// veto, which must not count a row nothing emits.
//
// The second return value is what this pass decided that a LATER pass
// needs (webTxOutcome).
func (r *webReader) transactionsBeforePSNStart(ctx context.Context, w canonical.Window, psn *psnReader, rels []silver.RelationshipPair) (silver.TransactionStream, webTxOutcome, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), webTxOutcome{}, nil
	}
	pass, err := r.newWebTxPass(ctx, psn, rels)
	if err != nil {
		return nil, webTxOutcome{}, err
	}
	out := canonical.TransactionBatch{}
	// Per (cash account, currency, settlement day), how many securities
	// settlements this pass EMITS. The portfolio pass folds its own
	// copy of a trade against it, and only an emitted row may be
	// counted: a booking this pass dropped is one gold will not hold,
	// and folding against it would lose the trade from both rails.
	settled := map[settledDayKey]int{}
	summaries, folded := 0, 0
	err = r.eachWebTx(ctx, "ubs-web Transactions", "WHERE value_date BETWEEN ? AND ?", []any{w.Start, w.End}, func(row webTxRow) error {
		switch {
		case pass.cut.excludes(row.account, row.valueDate):
			return nil
		case pass.foldedAway(row):
			folded++
			return nil
		}
		tx, ok := pass.project(row)
		if !ok {
			summaries++
			return nil
		}
		out.Transactions = append(out.Transactions, tx)
		if tx.Kind == canonical.TxKindBuy || tx.Kind == canonical.TxKindSell {
			settled[newSettledDayKey(row.account, row.currency, row.valueDate)]++
		}
		return nil
	})
	if err != nil {
		return nil, webTxOutcome{}, err
	}
	if summaries > 0 {
		log.Printf("ubs adapter: dropped %d period-close row(s) — a zero-amount summary or service-price line, not a booking", summaries)
	}
	if folded > 0 {
		log.Printf("ubs adapter: folded %d web row(s) into another feed's record of the same booking — one booking, one row", folded)
	}
	return silver.NewTransactionStream(out), webTxOutcome{
		hints:   psnHints{veto: pass.psnVeto, carry: pass.fold.psn, withheld: pass.withheld},
		settled: settled,
	}, nil
}

// webTxPass is what the web cash pass resolves once per read, before it
// walks the rows: the cut, the indexes a row is resolved against, and
// the folds and veto that decide which rows are emitted and how.
type webTxPass struct {
	cut      psnCut
	ownIBANs map[string]bool
	// valorToISIN turns the valor beside a statement-era trade's free
	// text into the instrument gold holds; such a trade carries no id.
	valorToISIN      map[string]string
	mt940Start       int64
	mortgageAccounts map[string]string
	fold             *eraFold
	seam             map[webTxTextKey]bool
	offsetVeto       map[string]bool
	psnVeto          map[string]bool
	// withheld names the conversion mirrors the export already records
	// (buildSameDayOffsetVeto); the PSN stream does not emit them.
	withheld map[string]bool
}

func (r *webReader) newWebTxPass(ctx context.Context, psn *psnReader, rels []silver.RelationshipPair) (*webTxPass, error) {
	var (
		p   webTxPass
		err error
	)
	if p.cut.startByRel, err = buildPSNStartByWebRel(ctx, psn, rels); err != nil {
		return nil, err
	}
	if p.cut.relOfAccount, err = r.buildAccountToRelMap(ctx); err != nil {
		return nil, err
	}
	if p.cut.coverage, err = psn.cashCoverage(ctx); err != nil {
		return nil, err
	}
	if p.ownIBANs, err = r.buildOwnIBANSet(ctx); err != nil {
		return nil, err
	}
	if p.valorToISIN, err = buildValorIndex(ctx, psn, r); err != nil {
		return nil, err
	}
	if p.mt940Start, err = r.mt940FeedStart(ctx); err != nil {
		return nil, err
	}
	if p.mortgageAccounts, err = r.buildMortgageAccountIndex(ctx); err != nil {
		return nil, err
	}
	// Both folds run BEFORE the offset veto, and their verdicts reach it
	// together: a web row either fold suppresses is not in the ledger, so
	// it must not consume an offset-veto match either. The veto's universe
	// is the EMITTED rows, and a suppressed row is represented there by
	// its PSN counterpart; leave it in and it can win the mirror its
	// counterpart needed, demoting one leg of a pair while the other keeps
	// its flow kind — the one-sided phantom flow the veto exists to
	// prevent.
	if p.fold, err = r.buildEraFold(ctx, psn, p.cut); err != nil {
		return nil, err
	}
	if p.seam, err = r.buildSeamBankRefs(ctx, psn); err != nil {
		return nil, err
	}
	suppressed := make(map[string]bool, len(p.fold.drop)+len(p.seam))
	for k := range p.fold.drop {
		suppressed[k] = true
	}
	for k := range p.seam {
		suppressed[k.txnNo+"@"+k.account] = true
	}
	if p.offsetVeto, p.psnVeto, p.withheld, err = r.buildSameDayOffsetVeto(ctx, psn, p.cut, suppressed); err != nil {
		return nil, err
	}
	return &p, nil
}

// foldedAway reports whether another feed's record of the row's booking
// is the one gold keeps. The era fold (buildEraFold) drops a statement
// reconstruction of a booking the export or the MT940 feed also records;
// the seam (buildSeamBankRefs) drops a web row the MT940 feed's first
// statements reach back over, where the cut excludes neither copy. Both
// are counted so the drop is visible on the load summary.
func (p *webTxPass) foldedAway(row webTxRow) bool {
	return p.fold.drop[row.emittedID()] ||
		p.seam[webTxTextKey{account: row.account, txnNo: webTxNumber(row.txID)}]
}

// project turns one web row into the transaction gold holds, or reports
// false for a statement's period summary, which is not a booking.
func (p *webTxPass) project(row webTxRow) (canonical.TransactionChange, bool) {
	hint := webKindHint(row.descKind, row.counterparty)
	kind, net, netAmount := webProjectedNet(hint, isStatementEraID(row.txID), row.debit, row.credit)
	netPtr := net

	// A statement's period summary is not a booking. The "Turnover
	// total" line a statement prints before its closing balance carries
	// no date, so the collector's parser attaches it to the booking that
	// precedes it — at a period close the zero-amount service-price or
	// interest line — and that row reaches silver with the totals as its
	// only narrative and an amount of zero. The same line under a real
	// fee or interest amount is a booking and is kept (bookingLines;
	// docs/adapters/ubs.md §7). The CSV feed prints the same close as a
	// zero-amount service-price row with no booking type.
	//
	// decodeWebTxEra reads an undecodable payload as a PDF backfill,
	// which routes the row through pdfCashIsExternal (INTERNAL on the
	// zero value) rather than letting it skip the gate and keep its
	// deposit/withdrawal kind.
	pl, pdfBackfill := decodeWebTxEra(row.payload)
	if net.IsZero() && (pdfBackfill && isStatementSummary(pl) || !pdfBackfill && isServicePriceClose(hint)) {
		return canonical.TransactionChange{}, false
	}
	statedMortgage := p.resolveCounterAccount(&pl)
	returnsInternal := p.returnsInternal(row, kind, pl, pdfBackfill)

	// The verdict is stamped here, before the sign is pinned: a
	// payload that cannot carry it degrades to the older demotion,
	// and the sign must then be read off the kind the row ENDS with.
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
	if instrumentID == nil {
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
	description := silver.StrPtrIfNonEmpty(text.description)
	payee := silver.StrPtrIfNonEmpty(text.counterparty)
	category := silver.StrPtrIfNonEmpty(text.providerCategory)
	// This export row kept a booking whose statement copy the era fold
	// dropped. Per column and only downward (richerText), the statement's
	// reading fills what this row left empty or as a bare code; the
	// amount, the value date, the kind and the id stay this row's.
	if alt, ok := p.fold.web[row.emittedID()]; ok {
		description = richerText(description, alt.description)
		payee = richerText(payee, alt.counterparty)
		category = richerText(category, alt.providerCategory)
	}
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
func (p *webTxPass) returnsInternal(row webTxRow, kind canonical.TxKind, pl webTxPayload, pdfBackfill bool) bool {
	if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
		return false
	}
	railEra := p.mt940Start > 0 && row.valueDate >= p.mt940Start
	return p.offsetVeto[row.emittedID()] ||
		p.ownIBANs[normalizeIBAN(pl.CounterAccount)] ||
		pdfBackfill && !pdfCashIsExternal(pl, p.ownIBANs, kind == canonical.TxKindWithdrawal, railEra)
}

// webTxRow is one row of the web silver's transactions table, as every
// pass over it reads the row.
type webTxRow struct {
	txID, account, currency, payload string
	valueDate                        int64
	debit, credit                    sql.NullFloat64
	counterparty, descKind           sql.NullString
}

// emittedID is the id the row reaches gold under. Web silver keys a
// transaction by (number, account), so each leg of an FX trade or other
// multi-leg event is its own row; gold keys by the id alone, so the
// account is folded in. The bank's "Transaction no." stays in the
// payload for queries that reassemble a trade.
func (row webTxRow) emittedID() string { return row.txID + "@" + row.account }

// eachWebTx calls fn on each row of the web transactions table that
// `where` (a WHERE clause over it, or "") selects.
func (r *webReader) eachWebTx(ctx context.Context, what, where string, args []any, fn func(webTxRow) error) error {
	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, value_date, account_external_id, currency_iso,
       amount_debit, amount_credit, counterparty, description_kind, payload
  FROM transactions `+where, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row webTxRow
		if err := rows.Scan(&row.txID, &row.valueDate, &row.account, &row.currency,
			&row.debit, &row.credit, &row.counterparty, &row.descKind, &row.payload); err != nil {
			return fmt.Errorf("%s scan: %w", what, err)
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

// psnCut is the hard cut at each banking relationship's PSN start: a
// web row on or after it, on an account the MT940 feed speaks for, is
// PSN's to carry. Every pass that reasons about the emitted rows
// applies it, so none counts a row nothing emits.
type psnCut struct {
	startByRel   map[string]int64  // web relationship → PSN start (buildPSNStartByWebRel)
	relOfAccount map[string]string // web account → web relationship
	// coverage is which accounts the feed speaks for, and from when
	// (psnCashCoverage). The feed is delivered per account, so a
	// relationship's PSN start says nothing about an account the
	// delivery leaves out — and the cut yields a web row to the feed
	// only where the feed holds the account's bookings at all.
	coverage psnCashCoverage
}

// excludes reports whether a web row on account at `at` falls on PSN's
// side of the cut: on or after the relationship's PSN start, on an
// account the feed speaks for by that day. A relationship with no PSN
// start cuts nothing, and neither does one for an account the feed
// never reaches — the cash accounts behind a managed portfolio, mostly
// — because there is nothing on the other side to arbitrate against.
func (c psnCut) excludes(account string, at int64) bool {
	cut := c.startByRel[c.relOfAccount[account]]
	if cut <= 0 || at < cut {
		return false
	}
	return c.coverage.speaksFor(account, at)
}

// counterAccountInNarrative finds the counter account the EXPORT feed states
// in its own narrative. The statement era carries it in a field of its own
// (the PDF parser's `counter_account`); the CSV era does not, and writes it
// into free text instead, in a fixed three-part shape:
//
//	Reason for payment: <purpose>; Account no. IBAN: <iban>; Transaction no. <n>
//
// It is the bank's own statement of where the money went, and it is the
// strongest evidence of an own-account move there is — stronger than a
// narrative regex, and available on rows whose other leg the product does not
// collect at all. Left in the free text it reached gold as prose and nothing
// could read it.
//
// The IBAN alone is taken, never the purpose or the payee: those name a
// person, and this value is compared against the relationship's own account
// ids. A narrative that states no IBAN returns empty, which is the common
// case — most rows pay a third party whose account gold does not hold.
// Case-insensitive because normalizeIBAN raises the case anyway, and a
// matcher stricter than the normaliser it feeds would drop a row for a
// difference that makes no difference.
var counterAccountInNarrative = regexp.MustCompile(`(?i)IBAN:\s*([A-Z]{2}[0-9]{2}[0-9A-Z ]{10,32}?)\s*(?:;|$)`)

// counterAccountFromNarrative returns the normalised counter IBAN a row's
// narrative names, or "" where it names none.
func counterAccountFromNarrative(p webTxPayload) string {
	for _, s := range []string{p.Description3, p.Description1} {
		if m := counterAccountInNarrative.FindStringSubmatch(s); m != nil {
			return normalizeIBAN(m[1])
		}
	}
	return ""
}

// counterLegInNarrative finds the line on which a statement writes the OTHER
// LEG of a movement that converted currency: what the money became, or came
// from, and the rate between the two.
//
//	CCY 1 234.56 Rate 1.234567
//
// It is the counter account's sibling, and a strictly stronger claim. A
// counter account says WHERE the money went; this says what the other side of
// the booking IS — its currency and its figure — which is the one fact that
// can join two legs no amount test can compare, because a conversion's two
// legs never carry the same number.
//
// Anchored on the whole line, and on the rate that closes it. The amount uses
// a space as its thousands separator and a dot for decimals, and a minor unit
// the currency does not have is simply absent, so the rate is what marks the
// end of the figure and keeps a line of running text from being read as one.
// A row stating no conversion — the overwhelming majority — matches nothing
// and is left exactly as it was.
var counterLegInNarrative = regexp.MustCompile(
	`^([A-Z]{3}) ([0-9][0-9 ]*(?:\.[0-9]+)?) Rate [0-9]+(?:\.[0-9]+)?$`)

// counterLegFromNarrative returns the currency and amount a row's narrative
// states for the other leg of its movement, or two empty strings where it
// states none.
//
// The statement era writes each continuation line separately, which is where
// the line is looked for; the export era's two narrative columns are read
// after it, so a feed that ever spells the same fact there is covered by the
// same rule rather than by a second one.
func counterLegFromNarrative(p webTxPayload) (currency, amount string) {
	lines := make([]string, 0, len(p.Continuation)+2)
	lines = append(lines, p.Continuation...)
	lines = append(lines, p.Description3, p.Description1)
	for _, s := range lines {
		if m := counterLegInNarrative.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			return m[1], strings.ReplaceAll(m[2], " ", "")
		}
	}
	return "", ""
}

// counterCurrencyKey and counterAmountKey carry the stated other leg into
// gold. Two keys rather than one object because the splice below writes a
// string value, and because either half is meaningless without the other:
// a consumer takes both or neither.
const (
	counterCurrencyKey = `"counter_currency":`
	counterAmountKey   = `"counter_amount":`
)

// withCounterLeg stamps the stated other leg onto a row's payload. Both keys
// or neither: a currency with no figure names no leg.
func withCounterLeg(payload json.RawMessage, currency, amount string) json.RawMessage {
	if currency == "" || amount == "" {
		return payload
	}
	out := spliceStringField(string(payload), counterAmountKey, amount)
	return spliceStringField(string(out), counterCurrencyKey, currency)
}

// mortgageStampInNarrative matches the reference UBS prints for the mortgage a
// payment services — `HYPOTHEK <base>.<tranche> <sequence>`.
//
// The export era states it in `Description2` and nowhere else. The statement
// era composed the same stamp into the description itself, which is why a
// mortgage payment booked before 2024 is recognised by the narrative rule and
// one booked after is not: the bank stopped printing the word where the
// composed description could reach it, and nothing else on the row said so.
var mortgageStampInNarrative = regexp.MustCompile(`(?i)\bHYPOTHEK\s+([0-9]+\s*\.\s*[A-Z0-9]+\s+[0-9]{4})\b`)

// mortgageRefShape is the reference itself, once the spaces are gone:
// a digit run, a dot, a tranche code, and a four-digit sub-account sequence.
var mortgageRefShape = regexp.MustCompile(`^([0-9]+)\.([A-Z0-9]+?)([0-9]{4})$`)

// mortgageRefKey folds either spelling of a mortgage reference onto one
// lookup key, so a stamp the bank prints can be matched against an account id
// gold holds without either being rewritten into the other.
//
// The two spellings differ in the branch. An account id is
// `BBBB AAAAAAAA.MMM NNNN` — a four-digit branch, then the account base
// zero-padded to eight. The stamp carries the base alone, unpadded. So the
// fold drops the branch (withBranch) and then the padding, leaving the part
// both spellings agree on. Nothing is constructed and no branch is guessed:
// an unknown stamp simply finds no account.
func mortgageRefKey(ref string, withBranch bool) string {
	m := mortgageRefShape.FindStringSubmatch(
		strings.ToUpper(strings.ReplaceAll(ref, " ", "")))
	if m == nil {
		return ""
	}
	base := m[1]
	if withBranch && len(base) > 8 {
		base = base[len(base)-8:]
	}
	if base = strings.TrimLeft(base, "0"); base == "" {
		return ""
	}
	return base + "." + m[2] + m[3]
}

// mortgageRefFromNarrative returns the folded key a narrative's HYPOTHEK stamp
// names, or "" for a narrative carrying none.
func mortgageRefFromNarrative(s string) string {
	m := mortgageStampInNarrative.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return mortgageRefKey(m[1], false)
}

// counterAccountKey is the payload key the statement era already uses, so a
// consumer reads ONE field whichever feed produced the row.
const counterAccountKey = `"counter_account":`

// withCounterAccount stamps a narrative-derived counter account onto a row's
// payload. The statement era's own value is the parser's, and spliceStringField
// refuses to overwrite a key the payload already carries, which is what keeps a
// derived value from displacing a stated one.
func withCounterAccount(payload json.RawMessage, iban string) json.RawMessage {
	return spliceStringField(string(payload), counterAccountKey, iban)
}

// withResolvedMortgage rewrites a STATED counter account that names a
// mortgage in prose to the id silver holds that mortgage under.
//
// It replaces where withCounterAccount refuses to, and what is being
// replaced is the difference. That function guards the statement
// parser's answer against a value derived from free text. This one
// touches no derived value at all: it rewrites the parser's own answer
// from the mortgage's NAME to the mortgage's ID — the same fact, in the
// spelling every consumer joins on. A name nothing can join to is not
// an answer worth protecting, and leaving it made the field look
// resolved while resolving to nothing.
//
// Surgical on purpose. The stamp occurs in the narrative lines too,
// where it belongs and is what the bank printed; only the value of the
// counter account key moves, and the rest of the payload stays the
// bytes the collector wrote.
func withResolvedMortgage(payload json.RawMessage, stamp, id string) json.RawMessage {
	if stamp == "" || id == "" || stamp == id {
		return payload
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return payload
	}
	re, err := regexp.Compile(`"counter_account"\s*:\s*"` + regexp.QuoteMeta(stamp) + `"`)
	if err != nil {
		return payload
	}
	return json.RawMessage(re.ReplaceAllLiteralString(
		string(payload), `"counter_account":`+string(encoded)))
}

// webTxNumber returns the bank's own "Transaction no." from a web row's
// silver id.
//
// The id IS that number, except on the rows whose number the bank gave
// to several movements at once — a deposit product stamps the number
// derived from its serial on every movement of its life, and a
// cross-border payment carries the correspondent's charge under the
// number of the payment it belongs to. The collector tells those apart
// with a suffix of its own (`_assign_export_txn_ids`), which is not part
// of anything the bank wrote and has to come off wherever the id is read
// AS a number: the reference this adapter publishes for the matcher to
// pair legs on, the PSN seam fold, and the narrative overlay. Left on,
// the row simply matches nothing — silently, because a reference that
// pairs nothing looks exactly like a movement that has no twin.
//
// A statement-era id comes back whole. Those are the collector's own
// content hashes, `#` and all, where the suffix names a LEG of a split
// movement; folding two legs onto one key would be a different bug.
func webTxNumber(txID string) string {
	if isStatementEraID(txID) {
		return txID
	}
	if i := strings.IndexByte(txID, '#'); i >= 0 {
		return txID[:i]
	}
	return txID
}

// bankRefKey is the payload key the MT940 feed already writes its `:61:`
// account-servicing-institution reference to (cashMovementPayload.BankRef),
// so a consumer reads ONE field whichever feed produced the row — the same
// bargain counterAccountKey strikes.
const bankRefKey = `"bank_ref":`

// webBankRef returns the bank reference a web transaction id names, or "" for
// an id that names none.
//
// A web row's silver id IS the bank's own "Transaction no.": the number the
// account statement prints against the entry, which UBS stamps on BOTH sides
// of a move between two accounts of one relationship — the very fact the
// ubs-web silver schema makes its transactions primary key compound to
// accommodate. So the reference is already in hand here and needs no parsing;
// what it needs is to travel as a FIELD, because gold's id for the row is the
// per-leg composition and nothing downstream should have to take that apart
// to find the number inside it.
//
// The statement era is the exception and is refused. Those ids are the
// collector's own content hash of a printed row (statementIDPrefix), not
// anything the bank wrote, so stamping one would claim an identity no bank
// ever asserted. A hash is unique per row and would pair nothing, which makes
// the refusal cheap — but a reference that names nothing has no business
// being offered as one.
func webBankRef(txID string) string {
	if isStatementEraID(txID) {
		return ""
	}
	return webTxNumber(txID)
}

// withBankRef stamps the bank's own reference for the entry onto a row's
// payload, by the same rule and for the same reason as withCounterAccount:
// one key, whichever feed wrote the row.
func withBankRef(payload json.RawMessage, ref string) json.RawMessage {
	return spliceStringField(string(payload), bankRefKey, ref)
}

// spliceStringField writes one string-valued key into a silver payload by
// splicing it in after the opening brace, rather than decoding the object and
// re-marshalling it. The payload is silver's JSON verbatim, and a round trip
// through a map would reorder and re-space every other key, making each row's
// payload churn on a change that added nothing.
//
// Three refusals, all of them leaving the payload byte for byte as it came:
// an empty value (there is nothing to state), a payload that is not a JSON
// object (a row whose payload never decoded is not one to start editing), and
// a payload that already carries the key — a parsed value is the feed's own
// and a derived one must never overwrite it.
//
// The value is marshalled rather than quoted, so a reference or an account id
// carrying a quote or a backslash produces valid JSON instead of a payload
// that no longer parses.
func spliceStringField(payload, key, value string) json.RawMessage {
	trimmed := strings.TrimSpace(payload)
	if value == "" || !strings.HasPrefix(trimmed, "{") || strings.Contains(trimmed, key) {
		return json.RawMessage(payload)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(payload)
	}
	field := key + string(encoded)
	if trimmed == "{}" {
		return json.RawMessage("{" + field + "}")
	}
	return json.RawMessage("{" + field + "," + trimmed[1:])
}

// returnsFlowInternal is the payload key carrying the conduit verdict to the
// returns engine. It is the vehicle for the "silver-side pre-tagging" the UBS
// ReturnsPolicy's ExternalOnly describes; the tag used to be a rewritten
// `kind`, which the spending population reads too and therefore could not
// share.
const returnsFlowInternal = `"returns_flow":"internal"`

// withReturnsFlow stamps the conduit verdict onto a row's payload. Only an
// INTERNAL verdict is written: external is the meaning of the key's absence,
// so nothing changes for the rows — every source but this one — that never
// classify a flow at all.
//
// The payload is the silver JSON object verbatim, so the key is spliced after
// the opening brace rather than round-tripped through a map: re-marshalling
// would reorder and re-space every other key and make each row's payload
// differ from the record silver holds, for one added field.
func withReturnsFlow(payload string, internal bool) (json.RawMessage, bool) {
	if !internal {
		return json.RawMessage(payload), true
	}
	trimmed := strings.TrimSpace(payload)
	if trimmed == "{}" {
		return json.RawMessage("{" + returnsFlowInternal + "}"), true
	}
	if strings.HasPrefix(trimmed, "{") {
		return json.RawMessage("{" + returnsFlowInternal + "," + trimmed[1:]), true
	}
	// Not an object — absent, or malformed enough that decoding failed.
	// The verdict has nowhere to live, and a row that silently loses it
	// would be counted as owner capital. Report the failure so the caller
	// falls back to the older, cruder carrier: the kind itself.
	return json.RawMessage(payload), false
}

// markReturnsInternal stamps the conduit verdict on a row, degrading to the
// kind when the payload cannot hold it. Losing the verdict would let conduit
// churn into the return, which is the worse error of the two: a row demoted
// to `other` is merely absent from spending, where the older behaviour left
// every such row anyway.
func markReturnsInternal(payload json.RawMessage, kind canonical.TxKind) (json.RawMessage, canonical.TxKind) {
	if out, ok := withReturnsFlow(string(payload), true); ok {
		return out, kind
	}
	return payload, canonical.TxKindOther
}

// dumpRunTimesInWindow returns the chronologically-sorted set of
// snapshot_at values in [w.Start, w.End] from dump_runs.
func (r *webReader) dumpRunTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `SELECT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ? ORDER BY snapshot_at`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs-web dumpRunTimesInWindow: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// appendWebPortfolios projects web silver portfolios into
// PortfolioChange. Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, portfolio_external_id, banking_relationship_id,
       description, payload
  FROM portfolios
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebPortfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap               int64
			extID, payload     string
			relID, description sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &relID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				continue
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
			PortfolioExternalID: extID,
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// appendWebAccounts projects web silver accounts (cash and
// safekeeping; the schema's CHECK constraint already excludes
// card accounts). Cutoff applies via banking_relationship_id.
func (r *webReader) appendWebAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, cutoff map[string]int64) error {
	const q = `
SELECT snapshot_at, account_external_id, kind, currency_iso,
       banking_relationship_id, portfolio_external_id,
       description, payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                 int64
			extID, kind, payload                 string
			ccy, relID, portfolioID, description sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &kind, &ccy, &relID, &portfolioID, &description, &payload); err != nil {
			return err
		}
		if relID.Valid {
			if cut := cutoff[relID.String]; cut > 0 && snap >= cut {
				continue
			}
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var ak canonical.AccountKind
		switch kind {
		case "cash":
			ak = canonical.AccountKindCash
		case "safekeeping":
			ak = canonical.AccountKindSafekeeping
		default:
			ak = canonical.AccountKindOther
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         ak,
			DisplayName:         silver.StrPtrIfNonEmpty(description.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(ccy.String),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// buildAccountToRelMap returns a web account_external_id → web
// banking_relationship_id lookup, taking the latest snapshot per
// account. Used by Transactions to find each row's relationship
// for the cutoff filter.
func (r *webReader) buildAccountToRelMap(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT a.account_external_id, a.banking_relationship_id
  FROM accounts a
  JOIN (SELECT account_external_id, MAX(snapshot_at) AS s
          FROM accounts GROUP BY account_external_id) m
    ON a.account_external_id = m.account_external_id
   AND a.snapshot_at = m.s
 WHERE a.banking_relationship_id IS NOT NULL`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("buildAccountToRelMap: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var acct, rel string
		if err := rows.Scan(&acct, &rel); err != nil {
			return nil, err
		}
		out[acct] = rel
	}
	return out, rows.Err()
}

// buildOwnIBANSet returns the set of the relationship's OWN account IBANs, keyed
// by the normalized (spaces stripped, upper-cased) IBAN — exactly the shape a
// transaction payload's counter_account normalizes to. account_external_id IS the
// IBAN (schema: "IBAN no-spaces upper"), so this is a name-free, PII-free key:
// membership decides internal-vs-external without any holder name. Used by the
// PDF-backfill cash-flow classifier to recognise inter-own-account moves as a
// SUPPLEMENT to the parser's internal_transfer boolean: it can demote a KNOWN own
// counter to internal, but it cannot promote — the parser flag is checked first
// and is authoritative, so an own mandate/portfolio destination absent from
// `accounts` is still vetoed to internal by that flag, not by this set.
func (r *webReader) buildOwnIBANSet(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT account_external_id FROM accounts`)
	if err != nil {
		return nil, fmt.Errorf("buildOwnIBANSet: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var iban string
		if err := rows.Scan(&iban); err != nil {
			return nil, err
		}
		out[normalizeIBAN(iban)] = true
	}
	return out, rows.Err()
}

// offsetVetoEps bounds the amount mismatch for the same-day offset veto. Book
// transfers preserve the amount exactly, so the tolerance only absorbs float
// scanning noise, never fees.
const offsetVetoEps = 0.01

// offsetLeg is one deposit/withdrawal-kinded cash row in the same-day offset
// probe. vetoKey carries the leg's emitted TransactionExternalID form —
// txID@acct for web rows, the event id for PSN rows — routed to the matching
// feed's veto map on a pair. Legs the classifier already demotes (parser
// internal flag, deep era, non-rail shape) still participate: an internal
// leg's same-day mirror on another own account is genuinely internal too
// (the parser-flagged UEBERTRAG orders whose receiving legs land in the
// MT940 feed are the live proof), and since a pair always drops on BOTH
// sides, even a coincidental false pair costs only a net-zero same-day pair.
type offsetLeg struct {
	vetoKey string
	psnLeg  bool
	txID    string
	acct    string
	amt     float64
	// day, ccy and the stated counter leg are what the CONVERSION phase
	// needs: it pairs across currencies, so it cannot read them off the
	// per-(day, currency) bucket the other two phases are grouped into.
	day       int64
	ccy       string
	statedCcy string
	statedAmt string
}

// offsetBooking is a leg's own booking — account, value day, currency
// and signed figure — the identity two records of one entry share.
type offsetBooking struct {
	acct string
	day  int64
	ccy  string
	amt  string
}

func (l offsetLeg) booking() offsetBooking {
	return offsetBooking{acct: l.acct, day: l.day, ccy: bookingCurrency(l.ccy),
		amt: strconv.FormatFloat(l.amt, 'f', 2, 64)}
}

// buildSameDayOffsetVeto pairs cash rows that offset each other on the same
// value day — same currency, equal amount (within offsetVetoEps), opposite
// direction, different own account — and returns the emitted-row keys of the
// paired legs per feed (web: txID@acct; PSN: event id) so BOTH transaction
// loops can demote them to a non-flow kind. Demoting both sides is what makes
// a pair — true or false — cost at most a net-zero same-day pair dropped from
// the flow stream; a one-sided demotion would fabricate a phantom external
// flow, the exact failure the veto exists to prevent.
//
// An intra-relationship move recorded without a counter IBAN is
// indistinguishable from an external payment on the row alone (both feeds
// record many shapes with a free-text beneficiary only), but the receiving own
// account books the mirror leg on the same value day. That mirror's membership
// in the own-account universe is the IBAN-free form of the same-relationship
// test, and it works across the feed seams (PDF ↔ MT940 ↔ PSN) where the ID
// schemes differ. The probe covers the EMITTED universe: web rows below their
// relationship's PSN cutover (a suppressed web row is represented by its PSN
// duplicate and must not consume a match) plus PSN cash movements, over the
// FULL silver rather than the load window — a row's classification depends
// only on silver contents, never on load slicing; a mirror leg that lands in
// a later dump is picked up on the next `reload`. `suppressed` names the web
// rows removed from that universe by either fold — the era fold
// (buildEraFold) and the seam (buildSeamBankRefs) — for the same reason the
// cut removes rows: their booking is already represented by the row that kept
// it. Every fold that drops a web row belongs in this set; one that is left
// out silently consumes matches on behalf of a row nothing emits.
//
// Matching is 1:1 and deterministic: first the currency conversions
// (vetoConversions), then, per value day and currency, every bank-linked
// twin (a shared Transaction no. — UBS stamps both sides of an
// inter-account transfer with one number) and the loose offsets among the
// legs left (pairSameDayOffsets).
func (r *webReader) buildSameDayOffsetVeto(ctx context.Context, psn *psnReader, cut psnCut, suppressed map[string]bool) (webVeto, psnVeto, withheld map[string]bool, err error) {
	legs, err := r.webOffsetLegs(ctx, cut, suppressed)
	if err != nil {
		return nil, nil, nil, err
	}
	psnLegs, err := psn.offsetLegs(ctx, cut.coverage)
	if err != nil {
		return nil, nil, nil, err
	}
	// A conversion mirror stands in for a booking no feed carries. Where
	// the export DOES carry it — the account is outside the MT940
	// delivery but inside the export's — the export's row is the bank's
	// own record and the mirror is withheld, here and in the PSN stream
	// alike, so the booking is one leg in the veto's universe and one
	// row in gold.
	withheld = map[string]bool{}
	exported := map[offsetBooking]bool{}
	for _, l := range legs {
		exported[l.booking()] = true
	}
	for _, l := range psnLegs {
		if isMirrorID(l.txID) && exported[l.booking()] {
			withheld[l.txID] = true
			continue
		}
		legs = append(legs, l)
	}

	webVeto, psnVeto = map[string]bool{}, map[string]bool{}
	record := func(l offsetLeg) {
		if l.psnLeg {
			psnVeto[l.vetoKey] = true
		} else {
			webVeto[l.vetoKey] = true
		}
	}
	consumed := vetoConversions(slices.Clone(legs), record)
	pairSameDayOffsets(legs, consumed, record)
	return webVeto, psnVeto, withheld, nil
}

// webOffsetLegs reads the web half of the offset veto's universe: every
// deposit/withdrawal row the transaction pass emits.
func (r *webReader) webOffsetLegs(ctx context.Context, cut psnCut, suppressed map[string]bool) ([]offsetLeg, error) {
	var legs []offsetLeg
	err := r.eachWebTx(ctx, "buildSameDayOffsetVeto (web)", "", nil, func(row webTxRow) error {
		if cut.excludes(row.account, row.valueDate) {
			return nil
		}
		// A web row either fold suppressed is likewise not in the ledger:
		// its booking is represented by the row that kept it, and letting
		// the suppressed copy stand here would let one booking consume two
		// mirrors.
		//
		// Both spellings, because the two folds key differently: the PSN
		// fold keys on the BANK's number, which is what a collector
		// suffix hangs off (webTxNumber strips it), while fold.drop keys
		// on the emitted id whole. A dump that leaves every member of a
		// transaction-number group suffixed would otherwise suppress a
		// row in the emit loop and leave it standing here.
		if suppressed[row.emittedID()] || suppressed[webTxNumber(row.txID)+"@"+row.account] {
			return nil
		}
		// The amount the row will REACH GOLD with, not the raw column
		// difference: the two web eras write the amount columns to
		// different conventions, and only the adapter's own projection
		// resolves them (the rule `bookingKey` names for the era fold).
		// Differencing the raw columns read every export-era withdrawal
		// as a positive figure, where it could mirror nothing.
		hint := webKindHint(row.descKind, row.counterparty)
		kind, _, netAmount := webProjectedNet(hint, isStatementEraID(row.txID), row.debit, row.credit)
		if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal || netAmount == nil {
			return nil
		}
		statedCcy, statedAmt := "", ""
		if decoded, ok := decodeWebTxPayload(row.payload); ok {
			statedCcy, statedAmt = counterLegFromNarrative(decoded)
		}
		legs = append(legs, offsetLeg{
			vetoKey: row.emittedID(), txID: row.txID, acct: row.account,
			amt: netAmount.InexactFloat64(), day: row.valueDate / 86400, ccy: row.currency,
			statedCcy: statedCcy, statedAmt: statedAmt,
		})
		return nil
	})
	return legs, err
}

// offsetLegs reads the PSN half of the offset veto's universe: every
// deposit/withdrawal cash movement.
func (r *psnReader) offsetLegs(ctx context.Context, coverage psnCashCoverage) ([]offsetLeg, error) {
	var legs []offsetLeg
	err := r.eachCashMovement(ctx, "buildSameDayOffsetVeto (psn)", func(row psnCashRow) error {
		var p cashMovementPayload
		if err := json.Unmarshal([]byte(row.payload), &p); err != nil || p.Amount == nil {
			return nil
		}
		kind := cashMovementKind(p.Narrative, p.CreditDebit, p.TxnType)
		if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
			return nil
		}
		amt, _ := p.Amount.Float64()
		if p.CreditDebit == "D" && amt > 0 {
			amt = -amt
		}
		acct, ccy := row.account, row.currency.String
		if p.Account != "" {
			acct = p.Account
		}
		if p.Funds != "" {
			ccy = p.Funds
		}
		// The other leg a converted movement states (`/OCMT/`), for the
		// conversion phase — the fact the statement era writes as
		// `CCY amount Rate`, in the feed's own spelling of it.
		statedCcy, statedAmt := statedConversion(p.Narrative)
		if statedAmt != "" {
			if d, err := parseSwiftDecimal(statedAmt); err == nil {
				statedAmt = d.String()
			} else {
				statedCcy, statedAmt = "", ""
			}
		}
		legs = append(legs, offsetLeg{
			vetoKey: row.eventID, psnLeg: true, txID: row.eventID, acct: acct,
			amt: amt, day: row.at / 86400, ccy: ccy,
			statedCcy: statedCcy, statedAmt: statedAmt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The conversion mirrors, as legs of their own: the other side of a
	// stated conversion, where no feed booked it (conversionMirrors).
	mirrors, err := r.conversionMirrors(ctx, coverage)
	if err != nil {
		return nil, err
	}
	for _, m := range mirrors.byID {
		amt, _ := m.tx.NetAmount.Float64()
		legs = append(legs, offsetLeg{
			vetoKey: m.tx.TransactionExternalID, psnLeg: true, txID: m.tx.TransactionExternalID,
			acct: m.tx.AccountExternalID, amt: amt, day: m.tx.OccurredAt / 86400, ccy: m.tx.Currency,
		})
	}
	return legs, nil
}

// pairSameDayOffsets matches the legs the conversion phase left, per
// value day and currency: greedily, first every bank-linked twin, then
// loose equal-and-opposite offsets. The twin phase covers the whole
// bucket before the loose one starts, so a twin-less leg that merely
// sorts earlier can never steal another leg's twin.
func pairSameDayOffsets(legs []offsetLeg, consumed map[string]bool, record func(offsetLeg)) {
	type groupKey struct {
		day int64
		ccy string
	}
	groups := map[groupKey][]offsetLeg{}
	for _, l := range legs {
		if !consumed[l.vetoKey] {
			k := groupKey{day: l.day, ccy: l.ccy}
			groups[k] = append(groups[k], l)
		}
	}
	less := func(s []offsetLeg) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].acct != s[j].acct {
				return s[i].acct < s[j].acct
			}
			return s[i].txID < s[j].txID
		}
	}
	for _, group := range groups {
		var debits, credits []offsetLeg
		for _, l := range group {
			if l.amt < 0 {
				debits = append(debits, l)
			} else if l.amt > 0 {
				credits = append(credits, l)
			}
		}
		sort.Slice(debits, less(debits))
		sort.Slice(credits, less(credits))
		used := make([]bool, len(credits))
		matched := make([]bool, len(debits))
		for _, twinsOnly := range []bool{true, false} {
			for di, d := range debits {
				if matched[di] {
					continue
				}
				for ci, c := range credits {
					if used[ci] || c.acct == d.acct || math.Abs(d.amt+c.amt) > offsetVetoEps ||
						twinsOnly && c.txID != d.txID {
						continue
					}
					matched[di], used[ci] = true, true
					record(d)
					record(c)
					break
				}
			}
		}
	}
}

// vetoConversions demotes both legs of an own-account move that CONVERTED
// CURRENCY, and returns the legs it consumed so the phases after it do not
// pair them with anything else.
//
// It is the veto's blind spot, closed. The two phases below it bucket by
// (day, currency) and match equal, opposite amounts, so neither can see a
// movement whose two legs are denominated differently — the veto's own note
// about FX legs says exactly that. And the gate the veto backstops
// (pdfCashIsExternal) reads a statement-era arrival credit as an interbank
// arrival on its booking type alone, because the intra-relationship shapes
// that booking could smuggle in are supposed to be peeled off HERE. A
// conversion between two accounts of one relationship is precisely such a
// shape, and until now nothing peeled it: the paying leg was demoted by the
// gate's conservative default while its receiving twin was promoted to
// external, which is the one-sided demotion the whole mechanism exists to
// prevent — a phantom arrival of owner capital, counted as return.
//
// The link it reads is the bank's own: on one of the two rows the statement
// writes what the other row holds, its currency and its figure
// (counterLegFromNarrative). A description is weaker than the shared
// transaction number the twin phase uses, so it is guarded harder — the
// described leg must be the ONLY leg of the day answering to it, and the only
// one so described. Anything else demotes NOTHING, because a demotion is only
// safe in pairs.
func vetoConversions(all []offsetLeg, record func(offsetLeg)) map[string]bool {
	type ownKey struct {
		day int64
		ccy string
		amt string
	}
	key := func(day int64, ccy string, amt float64) ownKey {
		return ownKey{day, ccy, strconv.FormatFloat(math.Abs(amt), 'f', 2, 64)}
	}
	// Ordered before anything is read. The caller flattens a map, and a map
	// is not an order; the phases after this one tolerate that because they
	// act within a bucket, but this one reaches across buckets and would
	// otherwise let two equally eligible legs resolve differently per run.
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.acct != b.acct {
			return a.acct < b.acct
		}
		return a.vetoKey < b.vetoKey
	})

	owners, described := map[ownKey]int{}, map[ownKey]int{}
	at := map[ownKey]int{}
	stated := func(l offsetLeg) (ownKey, bool) {
		if l.statedCcy == "" || l.statedAmt == "" {
			return ownKey{}, false
		}
		amt, err := strconv.ParseFloat(l.statedAmt, 64)
		if err != nil || amt == 0 {
			return ownKey{}, false
		}
		return key(l.day, l.statedCcy, amt), true
	}
	for i, l := range all {
		k := key(l.day, l.ccy, l.amt)
		owners[k]++
		at[k] = i
		if sk, ok := stated(l); ok {
			described[sk]++
		}
	}

	consumed := map[string]bool{}
	for _, l := range all {
		if consumed[l.vetoKey] || l.amt == 0 {
			continue
		}
		sk, ok := stated(l)
		if !ok || owners[sk] != 1 || described[sk] != 1 {
			continue
		}
		other := all[at[sk]]
		// A movement crosses two accounts and nets out across them. Same
		// account, same sign, or a leg already spoken for is not one.
		if other.acct == l.acct || consumed[other.vetoKey] || other.amt == 0 ||
			(other.amt < 0) == (l.amt < 0) {
			continue
		}
		consumed[l.vetoKey], consumed[other.vetoKey] = true, true
		record(l)
		record(other)
	}
	return consumed
}

// buildPSNStartByWebRel resolves the PSN-start cutoff per web
// banking_relationship_id using the config relationships pairing
// and the *psnReader (if configured). Returns an empty map when
// psn is nil (degenerate splice — every web row passes).
//
// Resolution order per relationship:
//  1. RelationshipPair.PSNStartOverride if non-zero.
//  2. MIN(snapshot_at) in PSN for the paired PSNID, otherwise.
//
// A relationship with no PSN counterpart (PSNID empty, or empty
// MIN) gets cutoff=0 → no filter applied to its web rows.
func buildPSNStartByWebRel(ctx context.Context, psn *psnReader, rels []silver.RelationshipPair) (map[string]int64, error) {
	out := make(map[string]int64, len(rels))
	if psn == nil {
		return out, nil
	}
	for _, p := range rels {
		if p.WebID == "" {
			continue
		}
		if p.PSNStartOverride > 0 {
			out[p.WebID] = p.PSNStartOverride
			continue
		}
		if p.PSNID == "" {
			continue
		}
		var minSnap sql.NullInt64
		err := psn.db.QueryRowContext(ctx, `
            SELECT MIN(snapshot_at)
              FROM cash_accounts
             WHERE relationship_id = ?`, p.PSNID).Scan(&minSnap)
		if err != nil {
			return nil, fmt.Errorf("PSN-start lookup for %q: %w", p.PSNID, err)
		}
		if minSnap.Valid {
			out[p.WebID] = minSnap.Int64
		}
	}
	return out, nil
}

// buildHistoricalCutoffs projects the per-relationship PSN-start
// cutoff onto the two key spaces the historical stream reads under.
// The historical stream is the pre-PSN backfill; without a cutoff
// its quarter-end rows collide with PSN's daily snapshot on the
// gold (source, snapshot_at, safekeeping, isin) / cash PK once PSN
// coverage laps a statement's period-end date.
//
// Portfolio side (historical_position_snapshots): the PDF row's
// portfolio_external_id is the PSN-aligned 'BBBBAAAAAAAANN' form,
// NOT the 4-char code that live web uses ('RNNN' / 'NNNN'). PSN
// silver's portfolios table owns that id space and its
// relationship_id, so the mapping comes from psn — mapping through
// live web's portfolios table misses every historical row because
// the two ID spaces don't overlap.
//
// Account side (historical_cash_balances): IBANs, which live web's
// accounts table already keys on with banking_relationship_id
// attached.
func (r *webReader) buildHistoricalCutoffs(
	ctx context.Context,
	cutoffByWebRel map[string]int64,
	psn *psnReader,
	rels []silver.RelationshipPair,
) (portfolioCutoff, accountCutoff map[string]int64, err error) {
	portfolioCutoff = map[string]int64{}
	accountCutoff = map[string]int64{}
	if len(cutoffByWebRel) == 0 {
		return portfolioCutoff, accountCutoff, nil
	}
	if psn != nil {
		cutoffByPSNRel := make(map[string]int64, len(rels))
		for _, p := range rels {
			if p.PSNID == "" || p.WebID == "" {
				continue
			}
			if c := cutoffByWebRel[p.WebID]; c > 0 {
				cutoffByPSNRel[p.PSNID] = c
			}
		}
		if err := populateCutoffMap(ctx, psn.db,
			`SELECT DISTINCT portfolio_external_id, relationship_id
			   FROM portfolios`,
			cutoffByPSNRel, portfolioCutoff); err != nil {
			return nil, nil, fmt.Errorf("historical cutoff (psn portfolios): %w", err)
		}
	}
	if err := populateCutoffMap(ctx, r.db,
		`SELECT account_external_id, banking_relationship_id FROM accounts
		  WHERE banking_relationship_id IS NOT NULL`,
		cutoffByWebRel, accountCutoff); err != nil {
		return nil, nil, fmt.Errorf("historical cutoff (web accounts): %w", err)
	}
	return portfolioCutoff, accountCutoff, nil
}

func populateCutoffMap(
	ctx context.Context, db *sql.DB, q string,
	cutoffByRel map[string]int64, out map[string]int64,
) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return err
		}
		if c := cutoffByRel[rel]; c > 0 {
			out[id] = c
		}
	}
	return rows.Err()
}

// webProjectedNet is the signed amount a `ubs-web` cash row projects into
// gold, plus the kind and the raw column net it was derived from. It is the
// single definition of that derivation: the transaction loop emits what it
// returns and the era fold (buildEraFold) keys on it, so one booking's
// amount is the same number whichever era recorded it.
//
// It has to be a shared definition because the two web eras do not agree on
// the sign convention of the silver amount columns. The statement
// reconstruction writes the figure a statement PRINTS, and a statement
// prints a debit as a positive figure in its debit column; the export
// carries the sheet's own cell, which already states the direction in its
// sign. The raw column net therefore comes out with opposite signs for one
// booking, and a key built on it would never pair the two.
//
// What both eras do agree on is WHICH column carries the figure, and that is
// what webKind reads for direction. So the direction comes from the kind and
// the magnitude from the figure (canonical.ApplyCanonicalSign) — never from
// abs(), which would erase the difference between a payment and its
// reversal. A kind with no pinned direction (interest, fx, `other`) keeps
// the source's sign, so there the two eras can still disagree and such a
// pair simply does not fold: the fold never guesses.
//
// Reversal rows bypass the sign helper, which would otherwise mask the
// bank's correction by forcing the amount back to the base kind's normal
// direction — and a correction forced back into its base direction is not a
// correction, it is a SECOND COPY of the thing it cancels. Their kind still
// maps to the underlying canonical kind so they net against the originals
// when summed.
//
// A reversal announces itself in one of two ways, and both are read here.
// The export names it in the booking type (`<base>;Reversal`). The
// statement does not: it prints a NEGATIVE FIGURE IN THE COLUMN THE
// ORIGINAL WAS PRINTED IN — a negative in the debit column is money coming
// back, a negative in the credit column is money going out again. That
// reading is only available in the statement era, which is why the caller
// has to say which era the row belongs to: the export's amount columns
// already carry the direction in their sign, so there a negative debit is
// an ordinary payment out and means nothing of the kind.
func webProjectedNet(descKind string, statementEra bool, debit, credit sql.NullFloat64) (canonical.TxKind, canonical.Decimal, *canonical.Decimal) {
	var net canonical.Decimal
	if credit.Valid {
		net = net.Add(canonical.NewDecimalFromFloat(credit.Float64))
	}
	if debit.Valid {
		net = net.Sub(canonical.NewDecimalFromFloat(debit.Float64))
	}
	kind := webKind(descKind, debit.Valid, credit.Valid)
	signed := net
	if webReversal(descKind, statementEra, debit, credit) {
		return kind, net, &signed
	}
	return kind, net, canonical.ApplyCanonicalSign(kind, &signed)
}

// webReversal reports whether a row is the bank correcting a booking it has
// already made. Both places that pin a sign ask this ONE question, because
// a row that answers yes in one of them and no in the other is signed twice
// by two different rules and the second wins.
//
// A web era says it in one of two ways. The export names it in the booking
// type (`<base>;Reversal`). The statement cannot — its booking type is
// whatever the bank printed and its amount columns hold magnitudes — so it
// prints a NEGATIVE FIGURE IN THE COLUMN THE ORIGINAL WENT IN: a negative
// in the debit column is money coming back, a negative in the credit column
// is money going out again. That second reading is only available in the
// statement era, which is why the caller has to say which era the row
// belongs to; the export's cells carry the direction in their own sign, so
// there a negative debit is an ordinary payment out and means nothing of
// the kind.
func webReversal(descKind string, statementEra bool, debit, credit sql.NullFloat64) bool {
	if _, ok := stripReversalSuffix(descKind); ok {
		return true
	}
	return statementEra && (debit.Valid && debit.Float64 < 0 || credit.Valid && credit.Float64 < 0)
}

// webKindHint is the text webKind classifies a row by: the booking type
// when it is one the vocabulary knows; otherwise the first segment of
// the promoted counterparty when THAT is one. Some CSV-feed rows leave
// the booking-type column empty, or fill it with a reference — a
// safe-box number, an interest period — and carry the product in
// Description1, the very text the promotion reads as the payee. Read
// there for the kind as well as for the payee (projectWebTxText), a
// custody price or a safe-box rental is a fee and not a withdrawal.
// When neither text is a type the booking type is returned as given,
// so the direction decides exactly as before.
func webKindHint(descKind, counterparty sql.NullString) string {
	if isBookingType(descKind.String) {
		return descKind.String
	}
	if head := firstSegment(counterparty.String); isBookingType(head) {
		return head
	}
	return descKind.String
}

// webKind maps the web silver's `description_kind` string plus
// debit/credit indicators to a canonical.TxKind. Conservative —
// unknown or ambiguous shapes route to TxKindOther rather than invent
// semantics that PSN's own MT940 events would contradict.
//
// A bank-side correction wears a `<base>;Reversal` suffix (e.g.
// `Dividend;Reversal`, a clawback of a dividend booking) and takes the
// same canonical kind as the event it reverses, so the two net out when
// summed by kind. Whether a row IS a reversal is webReversal's
// question, not this one's: the two eras state it differently, and this
// function sees neither the amounts nor the era.
//
// The match is case-insensitive on the whole (trimmed) string. The
// MT940 CSV feed and the PDF Account-Statement backfill spell the same
// concept differently ("Dividend" vs "DIVIDEND"), so an upper-cased
// EXACT match classifies both, and exact (not prefix) matching keeps
// the two vocabularies from colliding: MT940's multi-token forms
// ("UCCDD…; order") never equal a bare PDF booking type ("ORDER").
func webKind(descKind string, hasDebit, hasCredit bool) canonical.TxKind {
	// Strip a `;Reversal` suffix and classify the base, so a reversal
	// flavour the bank invents later needs no entry of its own.
	if base, ok := stripReversalSuffix(descKind); ok {
		return webKind(base, hasDebit, hasCredit)
	}
	bookingType := strings.ToUpper(strings.TrimSpace(descKind))
	if kind, ok := webKindByType[bookingType]; ok {
		return kind
	}
	if webSecuritiesTypes[bookingType] {
		return securitiesSide(hasDebit, hasCredit)
	}
	// Everything else, the bare payment order ("ORDER") among it, is
	// classified by direction: credit-only is a deposit, debit-only a
	// withdrawal, anything else stays "other". A payment order names
	// neither an instrument nor a side — no quantity, no price, a
	// narrative naming the party paid — and what it pays is often
	// another account of the same relationship, whose mirror books as
	// a plain credit.
	switch {
	case hasCredit && !hasDebit:
		return canonical.TxKindDeposit
	case hasDebit && !hasCredit:
		return canonical.TxKindWithdrawal
	}
	return canonical.TxKindOther
}

// webKindByType maps the booking types whose kind does not depend on
// the direction, upper-cased.
var webKindByType = map[string]canonical.TxKind{
	// Income / cost: NOT capital flows; excluded from returns.
	"DIVIDEND":                            canonical.TxKindDividend,
	"REVERSAL DIVIDEND":                   canonical.TxKindDividend,
	"COUPON":                              canonical.TxKindCoupon,
	"INTEREST":                            canonical.TxKindInterest,
	"INTEREST CALCULATION BALANCE":        canonical.TxKindInterest,
	"CALL DEPOSIT INTEREST PAYMENT":       canonical.TxKindInterest,
	"FIXED TERM DEPOSIT INTEREST PAYMENT": canonical.TxKindInterest,
	"FEE":                                 canonical.TxKindFee,
	"FEES":                                canonical.TxKindFee,
	"CUSTODY PRICE":                       canonical.TxKindFee,
	"ADR/GDR HANDLING FEES":               canonical.TxKindFee,
	"THIRD-PARTY CHARGES":                 canonical.TxKindFee,
	"RENTAL FEE SAFE BOX":                 canonical.TxKindFee,
	"BALANCE CLOSING OF SERVICE PRICES":   canonical.TxKindFee,
	"ADVICE":                              canonical.TxKindFee,
	"UBS ADVICE":                          canonical.TxKindFee,
	// The discretionary mandate's periodic management charge. Like
	// "Custody Price" it names the product charged for and carries no
	// instrument, quantity or price. `CAN` cancels a charge already
	// billed and `REC` re-bills the corrected figure; the
	// cancellation's inflow survives because the statement era prints
	// it as a negative debit, which webReversal reads.
	"UBS MANAGE":     canonical.TxKindFee,
	"CAN UBS MANAGE": canonical.TxKindFee,
	"REC UBS MANAGE": canonical.TxKindFee,
	// Currency conversion between the holder's own accounts — an
	// internal reshuffle, not a capital flow. The MT940 feed names the
	// instrument, the older PDF backfill only says "FOREX". Enumerated
	// so the multi-token MT940 forms do not fall through to the
	// direction fallback and book as deposits / withdrawals.
	"FOREX PURCHASE":        canonical.TxKindFx,
	"FOREX SALE":            canonical.TxKindFx,
	"PURCHASE FX SPOT":      canonical.TxKindFx,
	"SALE FX SPOT":          canonical.TxKindFx,
	"PURCHASE FX FORWARD":   canonical.TxKindFxForward,
	"SALE FX FORWARD":       canonical.TxKindFxForward,
	"PURCHASE FROM FX SWAP": canonical.TxKindFxSwap,
	"SALE FROM FX SWAP":     canonical.TxKindFxSwap,
	"BUY":                   canonical.TxKindBuy,
	"SECURITIES PURCHASE":   canonical.TxKindBuy,
	"SELL":                  canonical.TxKindSell,
	"SECURITIES SALE":       canonical.TxKindSell,
	// Mobile payments cross the relationship boundary like a card
	// payment. Named so the kind follows the booking type: the silver
	// row carries an unsigned figure in either column, and
	// ApplyCanonicalSign orients the amount by the kind, so a reversal
	// keeps its inflow whichever column printed it.
	"PAYMENT UBS TWINT":  canonical.TxKindWithdrawal,
	"DEBIT UBS TWINT":    canonical.TxKindWithdrawal,
	"CREDIT UBS TWINT":   canonical.TxKindDeposit,
	"REVERSAL UBS TWINT": canonical.TxKindDeposit,
}

// webSecuritiesTypes are the booking types that settle a securities
// trade, whose side (buy or sell) follows the cash direction. They
// reallocate between cash and instruments and are excluded from flows.
//
// Private-market vehicles settle the same way under their own
// vocabulary: a capital call buys fund units and a distribution sells
// them, both against the cash account in the same portfolio as the
// units. Left to the direction fallback they would be deposits and
// withdrawals, which returns count as external capital — each leg of
// one internal move booked as though the other did not exist.
var webSecuritiesTypes = map[string]bool{
	"SHARE":                               true,
	"MUTUAL FUNDS":                        true,
	"INVESTMENT FUNDS":                    true,
	"UBS INVESTMENT FUNDS":                true,
	"STRUCTURED PRODUCTS":                 true,
	"PURCHASE":                            true,
	"SALE":                                true,
	"PRECIOUS METAL BUY":                  true,
	"PRECIOUS METAL SELL":                 true,
	"BUY PM SPOT W/O VAT":                 true,
	"SELL PM SPOT W/O VAT":                true,
	"SUBSCRIPTION RIGHT":                  true,
	"CAPITAL CALL":                        true,
	"ISSUE WITHOUT RIGHTS":                true,
	"PURCHASE FROM ISSUE WITH PREPAYMENT": true,
	"CASH SETTLEMENT":                     true,
	"CASH DISTRIBUTION":                   true,
}

// securitiesSide maps a securities-settlement row to buy (cash out /
// debit) or sell (cash in / credit). Both kinds are excluded from
// the returns flow set, so the side is for analytics only.
func securitiesSide(hasDebit, hasCredit bool) canonical.TxKind {
	if hasCredit && !hasDebit {
		return canonical.TxKindSell
	}
	return canonical.TxKindBuy
}

// mt940FeedStart returns the earliest value_date carried by the MT940/CSV
// feed (rows without the PDF backfill's source marker), or 0 when the silver
// holds no MT940 rows at all. This is the era boundary for the classifier's
// rail promotion: from this day on, PDF rows are per-account gap-fills inside
// an era whose arrivals the MT940 feed counts, so they adopt MT940 semantics;
// before it, the deep backfill era stays conservative (see pdfCashIsExternal).
// A PDF-only silver (no MT940 coverage yet) therefore never promotes.
func (r *webReader) mt940FeedStart(ctx context.Context) (int64, error) {
	var start sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
SELECT MIN(value_date) FROM transactions
 WHERE payload NOT LIKE '%account_statement_pdf%'`).Scan(&start)
	if err != nil {
		return 0, fmt.Errorf("mt940FeedStart: %w", err)
	}
	return start.Int64, nil
}

// webTxPayload is every field the per-row helpers read out of a web
// transaction's silver payload. The row is decoded once and the decoded
// value handed to each helper, rather than each helper decoding the
// same JSON into a struct of its own.
type webTxPayload struct {
	Source           string   `json:"source"`
	BookingType      string   `json:"booking_type"`
	InternalTransfer bool     `json:"internal_transfer"`
	CounterAccount   string   `json:"counter_account"`
	Continuation     []string `json:"continuation"`
	// SecurityValor is the Swiss valor the statement parser lifts off a
	// trade's continuation line. Statement era only: the export era
	// states its instrument as an ISIN in Description1 and takes the
	// other road. The caption the parser promotes beside it is for a
	// person reading silver, not for this adapter — the valor is the
	// identity and the caption only a label.
	SecurityValor string `json:"security_valor"`
	Description1  string `json:"Description1"`
	Description2  string `json:"Description2"`
	Description3  string `json:"Description3"`
}

// decodeWebTxPayload decodes a silver payload, discarding a partial
// fill on any error and reporting the failure rather than returning a
// value indistinguishable from a payload whose fields are absent. The
// distinction is load-bearing on the external/internal gate: an absent
// source means "not a PDF backfill", which SKIPS that gate, so a
// decode failure read as an absent field would take the permissive
// branch on the classifier that keeps owner capital from being
// fabricated. Callers treat ok=false as the conservative case; the
// zero value itself is conservative only for the fields the caller
// reaches after that (never external, no caption, no narrative lines).
func decodeWebTxPayload(payload string) (webTxPayload, bool) {
	var p webTxPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return webTxPayload{}, false
	}
	return p, true
}

// decodeWebTxEra decodes a row's payload and reports whether it is to be
// read as a PDF backfill. It pairs the two questions every caller asks
// together, because the conservative rule that joins them —
// AN UNDECODABLE PAYLOAD IS TREATED AS A BACKFILL — belongs in one
// place: re-spelled per call site it is one edit away from a caller
// that decodes, ignores the failure, and takes the permissive branch on
// the classifier that keeps owner capital from being fabricated.
func decodeWebTxEra(payload string) (webTxPayload, bool) {
	p, decoded := decodeWebTxPayload(payload)
	return p, !decoded || isPDFCashBackfill(p)
}

// isPDFCashBackfill reports whether a transaction came from the
// Account-Statement PDF backfill (source="account_statement_pdf"). Gates the per-row external/internal
// classifier (pdfCashIsExternal); the MT940 feed carries no such
// marker and needs no per-row classifier beyond the same-day
// offset veto.
func isPDFCashBackfill(p webTxPayload) bool {
	return p.Source == "account_statement_pdf"
}

// normalizeIBAN strips spaces and upper-cases an IBAN-shaped string so a payload's
// formatted counter_account ("CH.. .... ....") compares byte-for-byte with an
// account_external_id (already no-spaces upper). Name-free.
func normalizeIBAN(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

// outboundPaymentRailBookings are the Account-Statement booking types of
// owner-initiated payments through the domestic payment rails and debit
// channels: payment orders (e-banking / paper / telephone / standing),
// e-bill (PayNet), direct debit (LSV), QR-bill, ATM cash, card and TWINT
// debits. These pay a beneficiary OUTSIDE the relationship by construction —
// moves between own accounts book as transfer forms that the collector's
// internal markers flag, carry an own counter IBAN, or mirror on the
// receiving own account and fall to the same-day offset veto — so a debit
// with one of these types that survives those checks is a boundary-crossing
// payment even when the statement records no counter IBAN (it usually
// doesn't: the beneficiary appears in free text only). Matching is exact on
// the upper-cased booking type, mirroring webKind's exact-match rationale.
var outboundPaymentRailBookings = map[string]bool{
	"E-BANKING PAYMENT ORDER":    true,
	"MULTI E-BANKING ORDER":      true,
	"PAYMENT ORDER":              true,
	"SPECIAL PAYMENT ORDER":      true,
	"PAYMENT ORDER BY TELEPHONE": true,
	"VARIOUS STANDING ORDERS":    true,
	"PAYNET ORDER":               true,
	"MULTI PAYNET ORDER":         true,
	"DIRECT DEBIT":               true,
	"QR-BILL (INSTANT PAYMENT)":  true,
	"ATM WITHDRAWAL":             true,
	"UBS BANCOMAT WITHDRAWAL":    true,
	"PAYMENT TO CARD":            true,
	"DEBIT CARD PAYMENT":         true,
	"PAYMENT UBS TWINT":          true,
	"DEBIT UBS TWINT":            true,
}

// inboundArrivalBookings are the booking types of payments ARRIVING through
// the interbank rails: the generic credit-transfer booking (an incoming
// SIC/SWIFT wire), its e-banking flavour, salary, and the two TWINT inflows
// — a mobile payment received, an outbound one reversed — which mirror the
// TWINT debits in the outbound set. The mirror of the outbound set — the
// MT940 feed counts the identical arrivals as deposits, so demoting the PDF
// era's copies would understate inflows against counted outflows and
// fabricate return. Own-product cash parkings (CALL DEPOSIT / FIXED TERM
// DEPOSIT increases, decreases, repayments) deliberately stay OUT of the
// set: they settle an own product inside the relationship.
var inboundArrivalBookings = map[string]bool{
	"CREDIT":             true,
	"E-BANKING CREDIT":   true,
	"SALARY PAYMENT":     true,
	"CREDIT UBS TWINT":   true,
	"REVERSAL UBS TWINT": true,
}

// pdfCashIsExternal decides whether a PDF-backfill cash movement is a genuine
// boundary-crossing (EXTERNAL) owner-capital flow or internal churn. outbound
// reports the row's direction (debit side); railEra reports whether the row
// falls on/after the MT940 feed's first covered day (mt940FeedStart).
//
// UBS cash/current accounts are conduits: external cash lands and is routed into
// securities / mandates / FX inside the relationship, and the securities value
// spine carries the return. Feeding that internal churn into a flow-based return
// double-counts capital. The rule is therefore CONSERVATIVE toward internal —
// default INTERNAL, mark EXTERNAL only when the movement PROVABLY crosses the
// relationship boundary — because a fabricated external double-counts capital
// (catastrophic — loose org-marker heuristics fabricate externals). It uses
// ONLY the normalized counter_account IBAN, the structured booking type, and
// the row's direction — NO holder name, NO free-text counterparty, i.e. no PII.
//
// Decision order:
//  1. parser-confirmed internal_transfer         ⇒ INTERNAL (authoritative)
//  2. own counter IBAN                           ⇒ INTERNAL
//  3. HYPOTHEK / MATURITY / CLOSING booking      ⇒ INTERNAL (own liability /
//     product settling inside the relationship)
//  4. railEra + direction-matching rail booking  ⇒ EXTERNAL (outbound:
//     outboundPaymentRailBookings; inbound: inboundArrivalBookings). No
//     counter IBAN required — the MT940 feed counts the identical bookings
//     as withdrawals/deposits via its direction fallback, so demoting the
//     gap-fill PDF copies would make returns depend on which feed covered
//     the month, and demoting only ONE direction fabricates return
//     outright. The intra-relationship shapes these rails could smuggle in
//     are peeled off first: rules 1–3 here, plus the same-day offset veto
//     at the call site.
//  5. populated non-own CH/LI counter IBAN       ⇒ EXTERNAL
//  6. everything else                            ⇒ INTERNAL (own-product cash
//     parkings, unrecognized shapes — conservative default)
//
// Rule 4 is era-gated: in the DEEP backfill era (before any MT940 coverage)
// both directions stay conservative-internal. That era's inbound capital is
// carried by the engine's onboarding step-ups (relationship and sub-entity
// debuts), so counting arrival credits as deposits double-counts it — and a
// per-row shape cannot distinguish a mid-life external arrival from migration
// funding a debut books days later. Swallowing both directions keeps the two
// errors offsetting instead of fabricating one-sided return; the honest fix
// for that era is transaction-complete counterparty data, not a looser rule.
func pdfCashIsExternal(p webTxPayload, own map[string]bool, outbound, railEra bool) bool {
	// The collector's own name-free markers (UEBERTRAG/UMBUCHUNG/MANDAT/MANAGE/
	// PORTFOLIO/REDUK on the continuation lines) already identified this row as an
	// intra-relationship mandate-funding / book-transfer move. That is authoritative
	// and VETOES external BEFORE any promotion below: a mandate/portfolio
	// destination absent from `accounts` is a known-internal row that would
	// otherwise pass the EXTERNAL conditions and fabricate owner capital
	// (the conduit direction this model exists to prevent). own-IBAN membership is a
	// supplement that can only DEMOTE a known-own counter to internal; it cannot
	// catch such a row, so the parser flag must gate first.
	if p.InternalTransfer {
		return false // parser-confirmed internal reshuffle ⇒ never external
	}
	ctr := normalizeIBAN(p.CounterAccount)
	if ctr != "" && own[ctr] {
		return false // inter-own-account move: a KNOWN own counter demotes regardless of booking shape
	}
	// Mortgage amortisation + structured-product maturity/closing net inside the
	// relationship (payoff of an own liability / roll of an own product), not owner
	// capital crossing the boundary.
	bt := strings.ToUpper(strings.TrimSpace(p.BookingType))
	if strings.Contains(bt, "HYPOTHEK") || strings.Contains(bt, "MATURITY") || strings.Contains(bt, "CLOSING") {
		return false
	}
	if railEra && outbound && outboundPaymentRailBookings[bt] {
		return true // owner-initiated payment through the rails ⇒ boundary-crossing
	}
	if railEra && !outbound && inboundArrivalBookings[bt] {
		return true // interbank arrival ⇒ boundary-crossing
	}
	if ctr == "" {
		return false // no counterparty IBAN and no rail evidence ⇒ INTERNAL
	}
	if !strings.HasPrefix(ctr, "CH") && !strings.HasPrefix(ctr, "LI") {
		return false // only Swiss/Liechtenstein counterparties count; anything else stays INTERNAL
	}
	return true
}

// extractInstrumentFromDescription1 pulls (ISIN, full caption)
// out of the silver row's payload.Description1 field, when
// present. UBS web statements format this field as
// "<security caption>; <ISIN>" — a typical caption looks like
// "UBS (Lux) Fund Solutions SICAV - UBS Core MSCI EMU UCITS ETF
// EUR dis-dist; LU0000000070". The trailing 12-char alphanumeric
// after "; " is a valid ISIN ~always in observed data; we
// validate by length-and-charset to avoid grabbing other ";"-
// separated comments.
//
// Returns (nil, nil) when the field is missing, empty, or the
// trailing token doesn't look like an ISIN. The full caption
// (everything before the final separator, trimmed) is returned
// even when no ISIN matches — it's still useful as a name
// fallback.
func extractInstrumentFromDescription1(p webTxPayload) (instrumentID, description *string) {
	if p.Description1 == "" {
		return nil, nil
	}
	caption := strings.TrimSpace(p.Description1)
	// Try to split on the last "; ". Anything 12 chars long
	// with the ISIN shape (2 alpha + 10 alnum) is treated as
	// an ISIN; otherwise the caption stays whole.
	if i := strings.LastIndex(caption, "; "); i >= 0 {
		head := strings.TrimSpace(caption[:i])
		tail := strings.TrimSpace(caption[i+2:])
		if looksLikeISIN(tail) {
			id := tail
			desc := head
			return &id, silver.StrPtrIfNonEmpty(desc)
		}
	}
	return nil, silver.StrPtrIfNonEmpty(caption)
}

// webDescription returns the row's narrative — the gold description
// less the payer's message, which travels apart as the change's Memo.
// The Description1 caption (as extractInstrumentFromDescription1
// returns it, ISIN tail stripped) wins whenever the row has one —
// unchanged, because gold's name lookups key on it. Otherwise the
// narrative is composed from what the row does carry, in a fixed
// order: the booking type first, then the narrative lines — the PDF
// backfill's statement continuation lines less the statement's
// turnover-total line (bookingLines), or the CSV feed's Description3
// — joined with "; " (silver.JoinText). A row with a bare booking
// code and nothing else therefore reaches gold as that code; a row
// with no text at all stays NULL. Nothing is inferred or paraphrased.
// A payload that does not decode contributes only the booking type.
func webDescription(captionDesc *string, bookingType string, p webTxPayload) *string {
	if captionDesc != nil {
		// A deposit product is the one caption that is not a security.
		// Description1 names the PRODUCT ("UBS Call Deposit; Serial
		// no. …") and the booking type carries the only fact that
		// separates one of its rows from another — a principal
		// movement from the interest it pays. Dropping the type here
		// left every row of a deposit reading identically, which is
		// what made the export era unreadable to any tier that works
		// from the narrative: not ambiguous, IDENTICAL. Prepending it
		// gives the row the same shape the statement era already
		// composes, so one spelling reaches both eras.
		if isDepositProductBooking(bookingType) {
			return silver.StrPtrIfNonEmpty(silver.JoinText(bookingType, *captionDesc))
		}
		return captionDesc
	}
	own, _ := bookingLines(p.Continuation)
	parts := make([]string, 0, 2+len(own))
	parts = append(parts, bookingType)
	parts = append(parts, own...)
	parts = append(parts, p.Description3)
	return silver.StrPtrIfNonEmpty(silver.JoinText(parts...))
}

// depositProductBookings are the booking types that move PRINCIPAL in
// or out of one of the bank's own cash-parking products — a call
// deposit, a fixed-term deposit, a notice account. The bank books
// every one of them on the account that funds the product and never
// lists the product as an account of its own, so these rows are the
// only trace of it there is.
//
// The interest payment is deliberately absent. It is the one movement
// of a deposit that is not a transfer: the money is new, it is income,
// and a list that swept it in with the rest would take a year of
// interest out of the income statement.
var depositProductBookings = map[string]bool{
	"CALL DEPOSIT NEW INVESTMENT":       true,
	"CALL DEPOSIT INCREASE":             true,
	"CALL DEPOSIT DECREASE":             true,
	"CALL DEPOSIT REPAYMENT":            true,
	"FIXED TERM DEPOSIT NEW INVESTMENT": true,
	"FIXED TERM DEPOSIT INCREASE":       true,
	"FIXED TERM DEPOSIT DECREASE":       true,
	"FIXED TERM DEPOSIT REPAYMENT":      true,
}

// isDepositProductBooking reports whether a booking type moves a
// deposit product's principal. Case-folded because the two feeds spell
// the same type differently — the statement era shouts it, the CSV
// export title-cases it — which is the same fold webKind applies for
// the same reason.
func isDepositProductBooking(bookingType string) bool {
	return depositProductBookings[strings.ToUpper(strings.TrimSpace(bookingType))]
}

// isBookingType reports whether a string is nothing but the bank's own
// classification of the entry — "Third-Party Charges", "Dividend",
// "Custody Price".
//
// webKind already carries that vocabulary, and reading it there keeps
// one list rather than two that drift: with neither direction set, a
// booking type it names resolves to a kind, while anything else falls
// past the switch to the direction fallback and, with no direction, to
// TxKindOther. So "resolves to something" is exactly "is a booking
// type", and a type added to the classifier is recognised here for
// free.
//
// ONE BLIND SPOT, and it is structural rather than an oversight: a
// booking type whose kind comes from the DIRECTION — every payment
// order, "ORDER" among them — resolves to TxKindOther when asked with
// no direction, so this returns false for it. Such a type is a booking
// type by any other measure, and the two callers are written to survive
// the answer: webKindHint returns the booking type unchanged when
// neither text is recognised, and the payee refusal in text.go only
// declines to blank a payee it would have blanked. Widening the probe
// is not the fix — asked WITH a direction the fallback answers for
// every string, and then nothing is not a booking type.
func isBookingType(s string) bool {
	return strings.TrimSpace(s) != "" &&
		webKind(s, false, false) != canonical.TxKindOther
}

// turnoverTotalPrefix is the upper-cased head of the line an Account
// Statement prints before its closing balance: "Turnover total <debits>
// <credits>", the period's two totals.
const turnoverTotalPrefix = "TURNOVER TOTAL"

// isTurnoverTotalLine reports whether a statement continuation line is the
// period's turnover-total line: the two words, any case, followed by figures
// and nothing else — no letter after the prefix, so a line that merely
// begins with the words is not it. The line carries no date, so the
// collector's parser attaches it to whichever booking precedes it; it names
// the period's totals, never the booking's own text.
func isTurnoverTotalLine(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	if !strings.HasPrefix(upper, turnoverTotalPrefix) {
		return false
	}
	rest := upper[len(turnoverTotalPrefix):]
	if rest != "" && rest[0] != ' ' {
		return false
	}
	for _, r := range rest {
		if unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// bookingLines splits a PDF-backfill row's continuation lines into the
// booking's own text — every non-blank line that is not a turnover-total
// line, in order — and whether a turnover-total line was among them.
func bookingLines(continuation []string) (own []string, turnover bool) {
	for _, line := range continuation {
		switch {
		case strings.TrimSpace(line) == "":
		case isTurnoverTotalLine(line):
			turnover = true
		default:
			own = append(own, line)
		}
	}
	return own, turnover
}

// isStatementSummary reports whether a PDF-backfill row's narrative is
// nothing but the statement's turnover-total line — the shape of the
// period summary the parser emits as a booking at each period close. The
// caller pairs it with a zero amount: the same narrative under a real fee
// or interest amount is a booking, and stays.
func isStatementSummary(p webTxPayload) bool {
	own, turnover := bookingLines(p.Continuation)
	return turnover && len(own) == 0
}

// tickerFromDescription pulls the trailing `(TICKER)` segment
// out of a UBS web caption like "Reg.shs Example AG (XMPL)" or
// "Sponsored American Deposit Receipt Example Co Ltd
// (Repr. 5 shs)     (XMPL)". Returns nil
// when:
//
//   - The string has no trailing `(...)`.
//   - The bracketed content isn't 1-10 chars of upper-case
//     ASCII letters / digits / hyphens (filters out things
//     like "(IE)" / "(Lux)" — those appear mid-string in ETF
//     issuer suffixes, never at the very end).
//
// Strict enough to avoid false positives on long parenthetical
// phrases that happen to come last (e.g. "(Repr. 5 shs)").
func tickerFromDescription(desc string) *string {
	desc = strings.TrimRight(desc, " \t")
	if !strings.HasSuffix(desc, ")") {
		return nil
	}
	open := strings.LastIndex(desc, "(")
	if open < 0 {
		return nil
	}
	t := desc[open+1 : len(desc)-1]
	if len(t) < 1 || len(t) > 10 {
		return nil
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' {
			return nil
		}
	}
	return &t
}

// appendWebMortgages projects ubs-web silver `mortgages` rows
// into three canonical changes per row:
//
//   - AccountChange{Kind: mortgage}           — one liability account
//   - InstrumentChange{AssetClass: mortgage}  — synthetic instrument
//     keyed by the same external ID (UBS doesn't expose a separate
//     instrument-level identity for mortgages)
//   - PositionChange{AssetClass: mortgage,
//     MarketValue: outstanding_balance}       — already negative in
//     the silver row; flows through to net-worth roll-ups as a
//     negative contribution
//
// No PSN cutoff: PSN doesn't surface mortgages at all, so every web
// snapshot's mortgage rows pass through. Migration 0004 of the
// ubs-web silver created the table; if it's absent on an older
// silver, the COUNT-zero path skips cleanly.
func (r *webReader) appendWebMortgages(ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch) error {
	ok, err := r.hasTable(ctx, "mortgages")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, banking_relationship_id,
       portfolio_external_id, currency_iso, description, payload
  FROM mortgages
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendWebMortgages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                      int64
			extID, currency, payload  string
			relID, portfolioID, descr sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &relID, &portfolioID,
			&currency, &descr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		// Dimensions only: the balance and terms stay inside payload, and
		// mortgage Positions are injected by the psn-web fold stream, NOT
		// emitted here. Emitting a Position at the web dump's snapshot_at
		// would create a "mortgage-only" gold snapshot at that time — and
		// gold's "latest snapshot per silver source" query (MAX over
		// positions.snapshot_at) would then land on it and hide every other
		// UBS position from the "today" view. The fold stream injects
		// mortgages only into PSN batches that already carry Positions,
		// keeping snapshot times aligned.
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         canonical.AccountKindMortgage,
			TaxWrapper:          relationshipTaxWrapper(),
			DisplayName:         silver.StrPtrIfNonEmpty(descr.String),
			BaseCurrency:        silver.StrPtrIfNonEmpty(currency),
			RelationshipID:      silver.StrPtrIfNonEmpty(relID.String),
			PortfolioExternalID: silver.StrPtrIfNonEmpty(portfolioID.String),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Name:                 silver.StrPtrIfNonEmpty(descr.String),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})
	}
	return rows.Err()
}

// latestMortgagePositions returns one PositionChange per mortgage
// account in web silver, populated from the row with the largest
// snapshot_at ≤ asOf for that account. Used by the fold stream to
// carry web-only mortgage data forward into each PSN snapshot:
// web dumps fire on login, PSN snapshots fire
// nightly, so without carry-forward the gold "latest snapshot per
// source" query falls onto a PSN-only time where the mortgage
// isn't refreshed and disappears from the position table.
//
// The returned templates have SnapshotAt = 0; the caller stamps
// the right time per batch.
func (r *webReader) latestMortgagePositions(ctx context.Context, asOf int64) ([]canonical.PositionChange, error) {
	ok, err := r.hasTable(ctx, "mortgages")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	const q = `
SELECT m.account_external_id, m.currency_iso, m.outstanding_balance, m.payload
  FROM mortgages m
  JOIN (
        SELECT account_external_id, MAX(snapshot_at) AS s
          FROM mortgages
         WHERE snapshot_at <= ?
         GROUP BY account_external_id
       ) latest
    ON latest.account_external_id = m.account_external_id
   AND latest.s                    = m.snapshot_at`
	rows, err := r.db.QueryContext(ctx, q, asOf)
	if err != nil {
		return nil, fmt.Errorf("latestMortgagePositions: %w", err)
	}
	defer rows.Close()
	var out []canonical.PositionChange
	for rows.Next() {
		var (
			extID, currency, payload string
			outstanding              sql.NullFloat64
		)
		if err := rows.Scan(&extID, &currency, &outstanding, &payload); err != nil {
			return nil, err
		}
		idCopy := extID
		var mv *canonical.Decimal
		if outstanding.Valid {
			d := canonical.NewDecimalFromFloat(outstanding.Float64)
			mv = &d
		}
		out = append(out, canonical.PositionChange{
			AccountExternalID:    extID,
			PositionKey:          extID,
			InstrumentExternalID: &idCopy,
			AssetClass:           canonical.AssetClassRealEstate,
			Vehicle:              canonical.VehicleMortgage,
			Currency:             currency,
			MarketValue:          mv,
			Payload:              json.RawMessage(payload),
		})
	}
	return out, rows.Err()
}

// hasTable reports whether the silver database carries a table. Older silvers
// predate some of them, and a reader that serves every vintage asks rather
// than assumes.
func (r *webReader) hasTable(ctx context.Context, name string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
        SELECT COUNT(*)
          FROM sqlite_master
         WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasTable %s: %w", name, err)
	}
	return n > 0, nil
}

// buildMortgageAccountIndex maps a folded mortgage reference onto the account
// id gold holds the mortgage under.
//
// It reads the ids rather than composing them. The branch a stamp omits is
// knowable only from the account itself, and the two places silver records one
// — the live positions export and the maturity-notice PDFs — already agree on
// the spelling, deliberately (the collector re-pads the PDF form so the two
// are byte-equal). Looking the whole id up therefore guesses nothing: a stamp
// naming a mortgage silver has never seen resolves to nothing, which is the
// right answer and leaves the row exactly as it was.
func (r *webReader) buildMortgageAccountIndex(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, table := range []string{"mortgages", "historical_mortgages"} {
		ok, err := r.hasTable(ctx, table)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rows, err := r.db.QueryContext(ctx,
			`SELECT DISTINCT account_external_id FROM `+table)
		if err != nil {
			return nil, fmt.Errorf("buildMortgageAccountIndex %s: %w", table, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if k := mortgageRefKey(id, true); k != "" {
				out[k] = id
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// looksLikeISIN: 12 chars, first two ASCII letters (country
// code), remaining 10 ASCII alphanumerics. Strict enough to
// reject "(SCMN)"-style ticker fragments while accepting every
// real ISIN.
func looksLikeISIN(s string) bool {
	if len(s) != 12 {
		return false
	}
	if !isASCIIAlpha(s[0]) || !isASCIIAlpha(s[1]) {
		return false
	}
	for i := 2; i < 12; i++ {
		c := s[i]
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// stripReversalSuffix peels a `;Reversal` (case-insensitive)
// suffix off a description_kind. Returns (base, true) when a
// suffix was present, (descKind, false) otherwise.
func stripReversalSuffix(descKind string) (string, bool) {
	const suffix = ";Reversal"
	if len(descKind) > len(suffix) &&
		strings.EqualFold(descKind[len(descKind)-len(suffix):], suffix) {
		return descKind[:len(descKind)-len(suffix)], true
	}
	return descKind, false
}
