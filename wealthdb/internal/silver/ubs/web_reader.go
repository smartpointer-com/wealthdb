package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
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
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil {
		return canonical.Status{}, err
	}
	if ok {
		histLo, histHi, err := r.historicalRange(ctx)
		if err != nil {
			return canonical.Status{}, err
		}
		if histLo >= 0 {
			if out.OldestSnapshotAt == -1 || histLo < out.OldestSnapshotAt {
				out.OldestSnapshotAt = histLo
			}
		}
		if histHi >= 0 && histHi > out.LatestSnapshotAt {
			out.LatestSnapshotAt = histHi
		}
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
	var (
		snapMin, snapMax sql.NullInt64
		txMin, txMax     sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(snapshot_at), MAX(snapshot_at)
          FROM dump_runs WHERE snapshot_at > ?`, since).Scan(&snapMin, &snapMax); err != nil {
		return canonical.Window{}, fmt.Errorf("ubs-web ChangeWindow snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(value_date), MAX(value_date)
          FROM transactions WHERE value_date > ?`, since).Scan(&txMin, &txMax); err != nil {
		return canonical.Window{}, fmt.Errorf("ubs-web ChangeWindow transactions: %w", err)
	}
	w := canonical.Window{NewChangeNumber: since}
	merge := func(n sql.NullInt64) {
		if !n.Valid {
			return
		}
		w.HasChanges = true
		if w.NewChangeNumber < n.Int64 {
			w.NewChangeNumber = n.Int64
		}
	}
	mins, maxs := []sql.NullInt64{snapMin, txMin}, []sql.NullInt64{snapMax, txMax}
	startSet := false
	for _, m := range mins {
		if m.Valid && (!startSet || m.Int64 < w.Start) {
			w.Start = m.Int64
			startSet = true
		}
	}
	for _, m := range maxs {
		if m.Valid && m.Int64 > w.End {
			w.End = m.Int64
		}
	}
	merge(snapMax)
	merge(txMax)

	if w.HasChanges {
		ok, err := r.hasHistoricalTables(ctx)
		if err != nil {
			return canonical.Window{}, err
		}
		if ok {
			histLo, histHi, err := r.historicalRange(ctx)
			if err != nil {
				return canonical.Window{}, err
			}
			if histLo >= 0 && histLo < w.Start {
				w.Start = histLo
			}
			if histHi > w.End {
				w.End = histHi
			}
		}
		// Cards emit at their own dates — a billing period ends on a
		// date no dump run need share — so the window has to reach them
		// or gold's delete-then-reinsert would leave duplicates behind.
		cardLo, cardHi, err := r.cardRange(ctx)
		if err != nil {
			return canonical.Window{}, err
		}
		if cardLo >= 0 && cardLo < w.Start {
			w.Start = cardLo
		}
		if cardHi > w.End {
			w.End = cardHi
		}
	}
	return w, nil
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
// The era fold (buildEraFold) is the one place a row IS dropped for
// being a second record of a booking, and only inside the web silver's
// own two eras against the machine-readable records: a statement
// reconstruction whose account, value day, signed amount and currency
// match an export or MT940 row is not emitted, because the row that
// matched it already carries the booking. That is an exact signature on
// the booking's own facts rather than a heuristic on ids, and it never
// folds two rows of one era.
//
// The second return value is what this pass decided about rows the PSN
// stream will emit (psnHints): the event ids of PSN cash movements whose
// mirror leg pairs a web row, which the caller demotes — a pair must drop
// on BOTH sides or the surviving side books a one-sided phantom external
// flow — and the narrative of any statement row the era fold dropped in
// favour of a PSN event, for the caller to carry onto it.
func (r *webReader) transactionsBeforePSNStart(ctx context.Context, w canonical.Window, psn *psnReader, rels []silver.RelationshipPair) (silver.TransactionStream, psnHints, error) {
	if !w.HasChanges {
		return silver.NewTransactionStream(canonical.TransactionBatch{}), psnHints{}, nil
	}
	cutoff, err := buildPSNStartByWebRel(ctx, psn, rels)
	if err != nil {
		return nil, psnHints{}, err
	}
	accountToRel, err := r.buildAccountToRelMap(ctx)
	if err != nil {
		return nil, psnHints{}, err
	}
	ownIBANs, err := r.buildOwnIBANSet(ctx)
	if err != nil {
		return nil, psnHints{}, err
	}
	mt940Start, err := r.mt940FeedStart(ctx)
	if err != nil {
		return nil, psnHints{}, err
	}
	// The era fold runs first: a statement row it folds away is not in the
	// ledger, so it must not consume an offset-veto match either.
	fold, err := r.buildEraFold(ctx, psn, cutoff, accountToRel)
	if err != nil {
		return nil, psnHints{}, err
	}
	offsetVeto, psnVeto, err := r.buildSameDayOffsetVeto(ctx, psn, cutoff, accountToRel, fold.drop)
	if err != nil {
		return nil, psnHints{}, err
	}

	const q = `
SELECT transaction_external_id, value_date, account_external_id,
       currency_iso, amount_debit, amount_credit, counterparty, description_kind, payload
  FROM transactions
 WHERE value_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, psnHints{}, fmt.Errorf("ubs-web Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	summaries, folded := 0, 0
	for rows.Next() {
		var (
			txID, accountID, ccy, payload string
			valueDate                     int64
			debit, credit                 sql.NullFloat64
			counterparty, kindStr         sql.NullString
		)
		if err := rows.Scan(&txID, &valueDate, &accountID, &ccy, &debit, &credit, &counterparty, &kindStr, &payload); err != nil {
			return nil, psnHints{}, fmt.Errorf("ubs-web Transactions scan: %w", err)
		}
		// Hard cut at PSN_start per relationship.
		if rel, ok := accountToRel[accountID]; ok {
			if cut := cutoff[rel]; cut > 0 && valueDate >= cut {
				continue
			}
		}
		emittedKey := txID + "@" + accountID
		// The era fold (buildEraFold): this statement reconstruction
		// describes a booking the export or the MT940 feed also records, and
		// the machine-readable record keeps it. Counted so the drop is
		// visible on the load summary.
		if fold.drop[emittedKey] {
			folded++
			continue
		}

		kind, net, netAmount := webProjectedNet(kindStr.String, isStatementEraID(txID), debit, credit)
		netPtr := net

		// A statement's period summary is not a booking. The "Turnover
		// total" line a statement prints before its closing balance
		// carries no date, so the collector's parser attaches it to the
		// booking that precedes it — at a period close the zero-amount
		// service-price or interest line — and that row reaches silver
		// with the totals as its only narrative and an amount of zero.
		// Dropped here and counted so the drop is visible. The same line
		// under a real fee or interest amount is a booking and is kept;
		// its narrative is composed without the line (bookingLines) and
		// its counterparty left empty below (docs/adapters/ubs.md §7).
		// A payload that does not decode is treated as a PDF backfill
		// whatever its fields say: that routes the row through
		// pdfCashIsExternal, which on the zero value classifies it
		// INTERNAL, rather than letting an undecodable row skip the
		// gate and keep its deposit/withdrawal kind. The flag's other
		// uses stay on the same side: the summary drop needs a summary
		// line the zero payload does not carry, and the booking type
		// travels whole instead of being split into memo + type.
		p, decoded := decodeWebTxPayload(payload)
		pdfBackfill := !decoded || isPDFCashBackfill(p)
		if pdfBackfill && net.IsZero() && isStatementSummary(p) {
			summaries++
			continue
		}
		// Classify each deposit/withdrawal as EXTERNAL (boundary-crossing
		// owner capital) or INTERNAL (conduit churn). Two layers:
		//
		//  1. The same-day offset veto (buildSameDayOffsetVeto) catches
		//     any leg — either feed — whose mirror books on another own
		//     account the same value day.
		//  2. PDF-backfill rows additionally pass pdfCashIsExternal
		//     (default INTERNAL; counter-IBAN + era-gated rail bookings —
		//     its doc carries the conduit model and the engine-policy
		//     interplay).
		//
		// The verdict is carried as its OWN flag, not by rewriting the
		// kind. Demoting the row to TxKindOther conflates two questions
		// a single column cannot answer at once: "is this owner capital
		// crossing the boundary?" (returns) and "is this a spending
		// row?" (the spending population selects on kind). A card
		// purchase is not owner capital under any reading, and it is
		// still spending — so the flag lets returns skip the row while
		// it stays in the spending base.
		returnsInternal := false
		if kind == canonical.TxKindDeposit || kind == canonical.TxKindWithdrawal {
			railEra := mt940Start > 0 && valueDate >= mt940Start
			switch {
			case offsetVeto[txID+"@"+accountID]:
				returnsInternal = true
			case pdfBackfill && !pdfCashIsExternal(p, ownIBANs, kind == canonical.TxKindWithdrawal, railEra):
				returnsInternal = true
			}
		}

		// The verdict is stamped here, before the sign is pinned: a
		// payload that cannot carry it degrades to the older demotion,
		// and the sign must then be read off the kind the row ENDS with.
		rowPayload := json.RawMessage(payload)
		if returnsInternal {
			rowPayload, kind = markReturnsInternal(rowPayload, kind)
		}
		if !webReversal(kindStr.String, isStatementEraID(txID), debit, credit) {
			netAmount = canonical.ApplyCanonicalSign(kind, &netPtr)
		}

		// The three narrative columns, the instrument id and the
		// payer's message all fall out of one projection
		// (projectWebTxText, which carries the per-column contract):
		// the ISIN goes on instrument_external_id so the gold-side
		// instruments join works for dividend / coupon / fee rows tied
		// to a security, and the message travels as the row's memo
		// rather than as narrative. The era text fold (merge.go) reads
		// the same projection, so one booking's text is the same string
		// whichever side of the seam it reaches gold from. The kind is
		// already classified from the raw column above and no text read
		// here can move it.
		text, instrumentID, message := projectWebTxText(counterparty.String, kindStr.String, p, pdfBackfill)
		description := silver.StrPtrIfNonEmpty(text.description)
		payee := silver.StrPtrIfNonEmpty(text.counterparty)
		category := silver.StrPtrIfNonEmpty(text.providerCategory)
		// This export row kept a booking whose statement copy the era fold
		// dropped. Per column and only downward (richerText), the statement's
		// reading fills what this row left empty or as a bare code, so the
		// fold loses nothing the printed record said. Nothing else moves:
		// the amount, the value date, the kind and the id are this row's.
		if alt, ok := fold.web[emittedKey]; ok {
			description = richerText(description, alt.description)
			payee = richerText(payee, alt.counterparty)
			category = richerText(category, alt.providerCategory)
		}

		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			// Web silver's transactions PK is the compound
			// (transaction_external_id, account_external_id) so
			// that FX trades and other multi-leg events appear as
			// separate rows per leg. Gold's transactions PK is
			// (silver_source_id, transaction_external_id), so we
			// synthesize a per-leg ID here. The natural
			// "Transaction no." remains in the payload for
			// downstream queries that want to reassemble the trade.
			TransactionExternalID: emittedKey,
			OccurredAt:            valueDate,
			AccountExternalID:     accountID,
			InstrumentExternalID:  instrumentID,
			Kind:                  kind,
			Currency:              ccy,
			NetAmount:             netAmount,
			Description:           description,
			Memo:                  silver.StrPtrIfNonEmpty(message),
			Counterparty:          payee,
			// The bank's own booking type, verbatim (a `;Reversal`
			// suffix included) — the closest thing a bank statement has
			// to a provider category, and what the spending provider
			// tier translates. The payer's message never enters it.
			ProviderCategory: category,
			Payload:          rowPayload,
		})
	}
	if summaries > 0 {
		log.Printf("ubs adapter: dropped %d statement summary row(s) — a zero-amount period-close line carrying nothing but the turnover totals", summaries)
	}
	if folded > 0 {
		log.Printf("ubs adapter: folded %d statement row(s) into the export or feed record of the same booking — one booking, one row", folded)
	}
	return silver.NewTransactionStream(out), psnHints{veto: psnVeto, carry: fold.psn}, rows.Err()
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
// a later dump is picked up on the next `reload`. `folded` names the statement
// rows the era fold (buildEraFold) removed from that universe for the same
// reason the cut removes rows: their booking is already represented by the
// export or feed row that kept it.
//
// Matching is 1:1 greedy and deterministic, in two global phases: first every
// bank-linked twin (shared Transaction no. — UBS stamps both sides of an
// inter-account transfer with one number; FX legs share it too but differ in
// currency, so they never pair here), then loose same-day offsets among the
// remaining legs. The twin phase is global so a twin-less leg that merely
// sorts earlier can never steal another leg's bank-linked twin.
func (r *webReader) buildSameDayOffsetVeto(ctx context.Context, psn *psnReader, cutoff map[string]int64, accountToRel map[string]string, folded map[string]bool) (webVeto, psnVeto map[string]bool, err error) {
	type groupKey struct {
		day int64
		ccy string
	}
	groups := map[groupKey][]offsetLeg{}

	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, value_date, account_external_id,
       currency_iso, amount_debit, amount_credit, description_kind
  FROM transactions`)
	if err != nil {
		return nil, nil, fmt.Errorf("buildSameDayOffsetVeto (web): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			txID, acct, ccy string
			valueDate       int64
			debit, credit   sql.NullFloat64
			kindStr         sql.NullString
		)
		if err := rows.Scan(&txID, &valueDate, &acct, &ccy, &debit, &credit, &kindStr); err != nil {
			return nil, nil, fmt.Errorf("buildSameDayOffsetVeto scan (web): %w", err)
		}
		// Emitted-universe filter: mirror the transaction loop's hard cut at
		// the per-relationship PSN cutover.
		if rel, ok := accountToRel[acct]; ok {
			if cut := cutoff[rel]; cut > 0 && valueDate >= cut {
				continue
			}
		}
		// A statement row the era fold folded away is likewise not in the
		// ledger: its booking is represented by the export or feed row that
		// kept it, and letting the folded copy stand here would let one
		// booking consume two mirrors.
		if folded[txID+"@"+acct] {
			continue
		}
		kind := webKind(kindStr.String, debit.Valid, credit.Valid)
		if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
			continue
		}
		amt := credit.Float64 - debit.Float64
		k := groupKey{day: valueDate / 86400, ccy: ccy}
		groups[k] = append(groups[k], offsetLeg{
			vetoKey: txID + "@" + acct, txID: txID, acct: acct, amt: amt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if psn != nil {
		prows, err := psn.db.QueryContext(ctx, `
SELECT event_external_id, timestamp, account_external_id, currency_iso, payload
  FROM events
 WHERE kind = 'cash_movement'`)
		if err != nil {
			return nil, nil, fmt.Errorf("buildSameDayOffsetVeto (psn): %w", err)
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
				return nil, nil, fmt.Errorf("buildSameDayOffsetVeto scan (psn): %w", err)
			}
			var p cashMovementPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Amount == nil {
				continue
			}
			kind := cashMovementKind(p.Narrative, p.CreditDebit)
			if kind != canonical.TxKindDeposit && kind != canonical.TxKindWithdrawal {
				continue
			}
			amt, _ := p.Amount.Float64()
			if p.CreditDebit == "D" && amt > 0 {
				amt = -amt
			}
			if p.Account != "" {
				acct = p.Account
			}
			c := ccy.String
			if p.Funds != "" {
				c = p.Funds
			}
			k := groupKey{day: ts / 86400, ccy: c}
			groups[k] = append(groups[k], offsetLeg{
				vetoKey: eventID, psnLeg: true, txID: eventID, acct: acct, amt: amt,
			})
		}
		if err := prows.Err(); err != nil {
			return nil, nil, err
		}
	}

	webVeto, psnVeto = map[string]bool{}, map[string]bool{}
	record := func(l offsetLeg) {
		if l.psnLeg {
			psnVeto[l.vetoKey] = true
		} else {
			webVeto[l.vetoKey] = true
		}
	}
	for _, legs := range groups {
		var debits, credits []offsetLeg
		for _, l := range legs {
			if l.amt < 0 {
				debits = append(debits, l)
			} else if l.amt > 0 {
				credits = append(credits, l)
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
		sort.Slice(debits, less(debits))
		sort.Slice(credits, less(credits))
		matchOf := make([]int, len(debits))
		for i := range matchOf {
			matchOf[i] = -1
		}
		used := make([]bool, len(credits))
		for pass := 0; pass < 2; pass++ {
			for di, d := range debits {
				if matchOf[di] >= 0 {
					continue
				}
				for i, c := range credits {
					if used[i] || c.acct == d.acct || math.Abs(d.amt+c.amt) > offsetVetoEps {
						continue
					}
					if pass == 0 && c.txID != d.txID {
						continue // twin phase: shared Transaction no. only
					}
					matchOf[di], used[i] = i, true
					break
				}
			}
		}
		for di, ci := range matchOf {
			if ci < 0 {
				continue
			}
			record(debits[di])
			record(credits[ci])
		}
	}
	return webVeto, psnVeto, nil
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

// webKind maps the web silver's `description_kind` string plus
// debit/credit indicators to a canonical.TxKind. Conservative —
// unknown / ambiguous shapes route to TxKindOther so we never
// invent semantics that PSN's own MT940 events would contradict.
//
// UBS marks bank-side corrections with a `<base>;Reversal`
// suffix (e.g. `Dividend;Reversal`, a clawback of a dividend
// booking). Reversals carry a negative amount in the
// credit column; we map them to the same canonical kind as the
// underlying event so they net out when summed by kind, and
// rely on ApplyCanonicalSign preserving the source's negative
// sign rather than forcing it positive.
func webKind(descKind string, hasDebit, hasCredit bool) canonical.TxKind {
	// Strip a `;Reversal` suffix if present and recurse on the
	// base. Lets us pick up any future reversal flavour the bank
	// invents without enumerating each.
	if base, ok := stripReversalSuffix(descKind); ok {
		return webKind(base, hasDebit, hasCredit)
	}
	// Match case-insensitively on the whole (trimmed) string. The
	// MT940 CSV feed and the PDF Account-Statement backfill spell the
	// same concept differently ("Dividend" vs "DIVIDEND",
	// "e-banking payment order" vs "E-BANKING PAYMENT ORDER"), so an
	// upper-cased EXACT-string match classifies both. Exact (not
	// prefix/substring) matching keeps the two vocabularies from
	// colliding: MT940's distinctive multi-token forms ("UCCDD…;
	// order") never equal a bare PDF booking type ("ORDER"), so each
	// feed's rows resolve independently.
	switch strings.ToUpper(strings.TrimSpace(descKind)) {
	// ---- Income / cost: NOT capital flows; excluded from returns.
	case "DIVIDEND", "REVERSAL DIVIDEND":
		return canonical.TxKindDividend
	case "COUPON":
		return canonical.TxKindCoupon
	case "INTEREST",
		"INTEREST CALCULATION BALANCE",
		"CALL DEPOSIT INTEREST PAYMENT",
		"FIXED TERM DEPOSIT INTEREST PAYMENT":
		return canonical.TxKindInterest
	case "FEE", "FEES",
		"CUSTODY PRICE",
		"ADR/GDR HANDLING FEES",
		"THIRD-PARTY CHARGES",
		"RENTAL FEE SAFE BOX",
		"BALANCE CLOSING OF SERVICE PRICES",
		"ADVICE", "UBS ADVICE":
		return canonical.TxKindFee
	// ---- Currency conversion between the holder's own accounts —
	// an internal reshuffle, not a capital flow. Spot, forward and
	// swap legs all reallocate cash across the holder's single-
	// currency accounts; none is external capital. The MT940 feed
	// names the instrument ("Purchase/Sale FX Spot/Forward", "…from
	// FX Swap"), the older PDF backfill only says "FOREX". All map
	// to non-flow fx kinds so they never enter net_flow — without
	// the explicit enumeration the multi-token MT940 forms would
	// fall through to the direction switch below and be mis-booked
	// as deposits / withdrawals.
	case "FOREX PURCHASE", "FOREX SALE",
		"PURCHASE FX SPOT", "SALE FX SPOT":
		return canonical.TxKindFx
	case "PURCHASE FX FORWARD", "SALE FX FORWARD":
		return canonical.TxKindFxForward
	case "PURCHASE FROM FX SWAP", "SALE FROM FX SWAP":
		return canonical.TxKindFxSwap
	// ---- Securities settlements: reallocate between cash and
	// instruments; excluded from flows. Side by cash direction.
	case "BUY", "SECURITIES PURCHASE":
		return canonical.TxKindBuy
	case "SELL", "SECURITIES SALE":
		return canonical.TxKindSell
	case "SHARE", "MUTUAL FUNDS", "INVESTMENT FUNDS",
		"UBS INVESTMENT FUNDS", "STRUCTURED PRODUCTS",
		"ORDER", "PURCHASE", "SALE",
		"PRECIOUS METAL BUY", "PRECIOUS METAL SELL",
		"BUY PM SPOT W/O VAT", "SELL PM SPOT W/O VAT",
		"SUBSCRIPTION RIGHT",
		"UBS MANAGE", "REC UBS MANAGE", "CAN UBS MANAGE":
		return securitiesSide(hasDebit, hasCredit)
	// ---- Mobile payments: money moving across the relationship
	// boundary, like a card payment or a payment order. The statement
	// era books the outflows as PAYMENT / DEBIT UBS TWINT and the
	// inflows — a payment received, an outbound payment reversed — as
	// CREDIT / REVERSAL UBS TWINT; the CSV feed spells the same types
	// in mixed case, which the fold above covers. Named here rather
	// than left to the direction fallback so the kind follows the
	// booking type: the silver row carries an unsigned figure in a
	// debit or a credit column (a trailing-minus figure on the
	// statement stays negative), and ApplyCanonicalSign orients the
	// net amount by the kind, so a reversal keeps its inflow whichever
	// column printed it.
	case "PAYMENT UBS TWINT", "DEBIT UBS TWINT":
		return canonical.TxKindWithdrawal
	case "CREDIT UBS TWINT", "REVERSAL UBS TWINT":
		return canonical.TxKindDeposit
	}
	// No description_kind hint → use direction. Credit-only
	// without instrument context = deposit; debit-only =
	// withdrawal. Everything else stays "other".
	switch {
	case hasCredit && !hasDebit:
		return canonical.TxKindDeposit
	case hasDebit && !hasCredit:
		return canonical.TxKindWithdrawal
	}
	return canonical.TxKindOther
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
	Description1     string   `json:"Description1"`
	Description3     string   `json:"Description3"`
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
		return captionDesc
	}
	own, _ := bookingLines(p.Continuation)
	parts := make([]string, 0, 2+len(own))
	parts = append(parts, bookingType)
	parts = append(parts, own...)
	parts = append(parts, p.Description3)
	return silver.StrPtrIfNonEmpty(silver.JoinText(parts...))
}

// isBookingType reports whether a string is nothing but the bank's own
// classification of the entry — "Third-Party Charges", "Dividend",
// "e-banking payment order".
//
// webKind already carries that vocabulary, and reading it there keeps
// one list rather than two that drift: with neither direction set, a
// booking type it names resolves to a kind, while anything else falls
// past the switch to the direction fallback and, with no direction, to
// TxKindOther. So "resolves to something" is exactly "is a booking
// type", and a type added to the classifier is recognised here for
// free.
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
	ok, err := r.hasMortgagesTable(ctx)
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
	ok, err := r.hasMortgagesTable(ctx)
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

// hasMortgagesTable returns true if the connected silver carries
// the `mortgages` table (added by migration 0004). Older silvers
// loaded before that migration just return false and the caller
// skips the projection.
func (r *webReader) hasMortgagesTable(ctx context.Context) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
        SELECT COUNT(*)
          FROM sqlite_master
         WHERE type = 'table' AND name = 'mortgages'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasMortgagesTable: %w", err)
	}
	return n > 0, nil
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
