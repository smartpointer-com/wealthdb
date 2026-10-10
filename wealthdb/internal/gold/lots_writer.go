package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// lotWriter stores one pass's result: the ledger, the rebuilt basis on
// positions and the engine's realized lots (docs/LOTS.md §5). Every
// table goes in column by column (colInsert): a trading history makes
// hundreds of thousands of rows.
type lotWriter struct {
	tx    *sql.Tx
	f     *lotFeed
	res   *lots.Result
	build int64
	sums  map[string]*LotsSourceSummary
	// realized names the realized lot each disposal produced, by
	// disposal; "" for none.
	realized []string
}

// LotLedgerTables are the tables of the engine's ledger that every pass
// rewrites whole; loader.Reset drops a source's rows.
var LotLedgerTables = []string{"lots", "lot_disposals", "lot_realized", "lot_findings", "lot_anchors"}

func (w *lotWriter) write(ctx context.Context, prints map[string]string) error {
	var clearSQL []string
	for _, t := range LotLedgerTables {
		clearSQL = append(clearSQL, `DELETE FROM `+t)
	}
	if len(w.f.ids) == 0 {
		// A pass that replays no source leaves no run to read: the last
		// runs would otherwise still name sources it no longer replays.
		clearSQL = append(clearSQL, `DELETE FROM lot_runs`)
	}
	for _, q := range append(clearSQL,
		`UPDATE positions SET book_value = NULL, basis_origin = NULL, basis_method = NULL, basis_fees = NULL,
		        book_value_known = NULL, quantity_without_basis = NULL, open_lots = NULL
		  WHERE basis_origin = 'rebuilt' OR book_value_known IS NOT NULL OR open_lots IS NOT NULL`,
	) {
		if _, err := w.tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("lots: clear: %w", err)
		}
	}
	for _, step := range []func(context.Context) error{
		w.writeRealized, w.writeLots, w.writeDisposals, w.writeFindings, w.writeAnchors, w.writePositions,
	} {
		if err := step(ctx); err != nil {
			return fmt.Errorf("lots: %w", err)
		}
	}
	return w.writeRuns(ctx, prints)
}

func (w *lotWriter) sum(k int32) *LotsSourceSummary { return w.sums[w.f.info[k].src.id] }

func (w *lotWriter) fill(k int32) bool { return w.f.info[k].src.pol.Mode == lots.Fill }

// dateOf is the UTC date of a Unix day, nil for NoDay.
func dateOf(day int32) *time.Time {
	if day == lots.NoDay {
		return nil
	}
	t := returns.DayToTimeUTC(int64(day))
	return &t
}

// lotVal is v, or NaN (NULL) when not known (colInsert.floats).
func lotVal(v float64, known bool) float64 {
	if !known {
		return math.NaN()
	}
	return v
}

func currencyOr(c string) string {
	if c == "" {
		return "USD"
	}
	return c
}

// writeRealized writes a realized lot for every disposal that realized
// on a source the engine fills: one per sale and lot relieved, into
// lot_realized. It is primary unless its transaction is documented
// (documented_txns).
func (w *lotWriter) writeRealized(ctx context.Context) error {
	w.realized = make([]string, len(w.res.Disposals))
	var (
		src, id, acct, inst, hint, acq, disposed, ccy, term, origin, method, fees, disposal []string
		qty, proceeds, cost                                                                 []float64
		years                                                                               []int64
		various, primary                                                                    []bool
	)
	for i, d := range w.res.Disposals {
		l := w.res.Lots[d.Lot]
		if !d.Realized() || !w.fill(l.Key) || !(d.Qty > 0) {
			continue
		}
		info := w.f.info[l.Key]
		w.realized[i] = d.Txn + "#" + l.ID
		w.sum(l.Key).RealizedLots++
		day := lots.DayOf(d.At)
		on := *dateOf(day)
		src = append(src, info.src.id)
		id = append(id, w.realized[i])
		acct = append(acct, d.Account)
		if info.hint {
			inst, hint = append(inst, ""), append(hint, info.inst)
		} else {
			inst, hint = append(inst, info.inst), append(hint, "")
		}
		years = append(years, int64(on.Year()))
		acq = append(acq, dateText(dateOf(l.AcqDay)))
		various = append(various, l.Various)
		disposed = append(disposed, dateText(&on))
		ccy = append(ccy, currencyOr(info.currency))
		qty = append(qty, lotVal(d.Qty, true))
		proceeds = append(proceeds, lotVal(d.Proceeds, true))
		cost = append(cost, lotVal(d.Cost, d.CostKnown))
		t := ""
		if l.AcqDay != lots.NoDay {
			t = "short"
			if on.After(dateOf(l.AcqDay).AddDate(1, 0, 0)) {
				t = "long"
			}
		}
		term = append(term, t)
		if d.CostKnown {
			origin = append(origin, string(canonical.BasisRebuilt))
			method = append(method, l.Method.String())
			fees = append(fees, l.Fees.String())
		} else {
			origin, method, fees = append(origin, ""), append(method, ""), append(fees, "")
		}
		primary = append(primary, !w.f.documented[[2]string{info.src.id, d.Txn}])
		k := ""
		if d.Kind != lots.DisposeSell {
			k = d.Kind.String()
		}
		disposal = append(disposal, k)
	}
	var c colInsert
	c.text("silver_source_id", src)
	c.text("realized_lot_external_id", id)
	c.text("account_external_id", acct)
	c.text("instrument_external_id", inst)
	c.text("instrument_hint", hint)
	c.int64s("tax_year", years)
	c.typed("acquisition_date", "DATE", acq)
	c.bools("acquired_various", various)
	c.typed("disposal_date", "DATE", disposed)
	c.text("currency", ccy)
	c.floats("quantity", "DECIMAL(28, 8)", qty)
	c.floats("proceeds", "DECIMAL(28, 4)", proceeds)
	c.floats("book_value", "DECIMAL(28, 4)", cost)
	c.text("term", term)
	c.text("basis_origin", origin)
	c.text("basis_method", method)
	c.text("basis_fees", fees)
	c.bools("is_primary", primary)
	c.text("disposal", disposal)
	return c.exec(ctx, w.tx, "lot_realized")
}

func (w *lotWriter) writeLots(ctx context.Context) error {
	var (
		id, src, acct, inst, ccy, acq, origin, txn, costOrigin, fees, method, closed []string
		qty, cost                                                                    []float64
		opened                                                                       []int64
		various                                                                      []bool
	)
	for _, l := range w.res.Lots {
		if !(l.Qty > 0) {
			continue
		}
		info := w.f.info[l.Key]
		w.sum(l.Key).Lots++
		id = append(id, l.ID)
		src = append(src, info.src.id)
		acct = append(acct, info.acct)
		inst = append(inst, info.inst)
		ccy = append(ccy, currencyOr(info.currency))
		opened = append(opened, l.OpenedAt)
		acq = append(acq, dateText(dateOf(l.AcqDay)))
		various = append(various, l.Various)
		origin = append(origin, l.Origin.String())
		txn = append(txn, l.Txn)
		qty = append(qty, lotVal(l.Qty, true))
		cost = append(cost, lotVal(l.Cost, l.CostKnown))
		costOrigin = append(costOrigin, l.CostOrigin.String())
		fees = append(fees, l.Fees.String())
		method = append(method, l.Method.String())
		c := ""
		if l.ClosedAt != 0 {
			c = strconv.FormatInt(l.ClosedAt, 10)
		}
		closed = append(closed, c)
	}
	var c colInsert
	c.text("lot_id", id)
	c.text("silver_source_id", src)
	c.text("account_external_id", acct)
	c.text("instrument_external_id", inst)
	c.text("currency", ccy)
	c.int64s("opened_at", opened)
	c.typed("acquisition_date", "DATE", acq)
	c.bools("acquired_various", various)
	c.text("origin", origin)
	c.text("origin_transaction_external_id", txn)
	c.floats("quantity", "DECIMAL(28, 8)", qty)
	c.floats("cost", "DECIMAL(28, 4)", cost)
	c.text("cost_origin", costOrigin)
	c.text("fees", fees)
	c.text("method", method)
	c.typed("closed_at", "BIGINT", closed)
	c.constant("build_id", w.build)
	return c.exec(ctx, w.tx, "lots")
}

func (w *lotWriter) writeDisposals(ctx context.Context) error {
	var (
		lot, src, acct, txn, kind, realized []string
		qty, proceeds, cost                 []float64
		at                                  []int64
	)
	for i, d := range w.res.Disposals {
		l := w.res.Lots[d.Lot]
		if !(d.Qty > 0) || !(l.Qty > 0) {
			continue
		}
		w.sum(l.Key).Disposals++
		lot = append(lot, l.ID)
		src = append(src, w.f.info[l.Key].src.id)
		acct = append(acct, d.Account)
		txn = append(txn, d.Txn)
		at = append(at, d.At)
		kind = append(kind, d.Kind.String())
		qty = append(qty, lotVal(d.Qty, true))
		proceeds = append(proceeds, lotVal(d.Proceeds, d.ProceedsKnown))
		cost = append(cost, lotVal(d.Cost, d.CostKnown))
		realized = append(realized, w.realized[i])
	}
	var c colInsert
	c.text("lot_id", lot)
	c.text("silver_source_id", src)
	c.text("account_external_id", acct)
	c.text("disposal_transaction_external_id", txn)
	c.int64s("disposed_at", at)
	c.text("kind", kind)
	c.floats("quantity", "DECIMAL(28, 8)", qty)
	c.floats("proceeds", "DECIMAL(28, 4)", proceeds)
	c.floats("cost", "DECIMAL(28, 4)", cost)
	c.text("realized_lot_external_id", realized)
	c.constant("build_id", w.build)
	return c.exec(ctx, w.tx, "lot_disposals")
}

// writeFindings writes the replay's findings, the feed's, and one
// wash_sale_window per loss the engine realized where the key bought
// within thirty days either side: the engine applies no wash sale rule.
func (w *lotWriter) writeFindings(ctx context.Context) error {
	var (
		src, acct, inst, kind, txn []string
		qty                        []float64
		at                         []int64
	)
	add := func(k int32, account string, when int64, finding lots.FindingKind, q float64, t string) {
		info := w.f.info[k]
		if account == "" {
			account = info.acct
		}
		src = append(src, info.src.id)
		acct = append(acct, account)
		inst = append(inst, info.inst)
		at = append(at, when)
		kind = append(kind, finding.String())
		qty = append(qty, lotVal(q, true))
		txn = append(txn, t)
	}
	count := func(k int32, kind lots.FindingKind) {
		s := w.sum(k)
		switch kind {
		case lots.FindSeed:
			s.Seeds++
		case lots.FindImplied:
			s.ImpliedDisposals++
		case lots.FindBlip:
			s.Blips++
		case lots.FindResolved:
			s.SeedsResolved++
		case lots.FindFeeUnvalued:
			s.FeeUnvalued++
		}
	}
	for _, x := range slices.Concat(w.res.Findings, w.f.findings) {
		add(x.Key, x.Account, x.At, x.Kind, x.Qty, x.Txn)
		count(x.Key, x.Kind)
	}
	buys := map[int32][]int64{}
	for _, l := range w.res.Lots {
		if l.Origin == lots.OriginBuy {
			buys[l.Key] = append(buys[l.Key], l.OpenedAt)
		}
	}
	for i, d := range w.res.Disposals {
		if w.realized[i] == "" || !d.CostKnown || d.Proceeds >= d.Cost {
			continue
		}
		l := w.res.Lots[d.Lot]
		ts := buys[l.Key]
		j, _ := slices.BinarySearch(ts, d.At-30*86400)
		for ; j < len(ts) && ts[j] <= d.At+30*86400; j++ {
			if ts[j] != l.OpenedAt {
				add(l.Key, d.Account, d.At, lots.FindWashSaleWindow, d.Qty, d.Txn)
				break
			}
		}
	}
	var c colInsert
	c.text("silver_source_id", src)
	c.text("account_external_id", acct)
	c.text("instrument_external_id", inst)
	c.int64s("found_at", at)
	c.text("finding", kind)
	c.floats("quantity", "DECIMAL(28, 8)", qty)
	c.text("transaction_external_id", txn)
	c.constant("build_id", w.build)
	return c.exec(ctx, w.tx, "lot_findings")
}

func (w *lotWriter) writeAnchors(ctx context.Context) error {
	var (
		src, acct, inst, ccy                                              []string
		bookQty, statedQty, matchedQty, bookCost, statedCost, resolvedQty []float64
		at                                                                []int64
		kept                                                              []bool
	)
	for _, a := range w.res.Anchors {
		info := w.f.info[a.Key]
		s := w.sum(a.Key)
		s.Anchors++
		if a.Kept {
			s.AnchorsKept++
		}
		src = append(src, info.src.id)
		acct = append(acct, info.acct)
		inst = append(inst, info.inst)
		ccy = append(ccy, currencyOr(info.currency))
		at = append(at, a.At)
		bookQty = append(bookQty, lotVal(a.BookQty, true))
		statedQty = append(statedQty, lotVal(a.StatedQty, true))
		matchedQty = append(matchedQty, lotVal(a.MatchedQty, true))
		bookCost = append(bookCost, lotVal(a.BookCost, true))
		statedCost = append(statedCost, lotVal(a.StatedCost, true))
		resolvedQty = append(resolvedQty, lotVal(a.Resolved, true))
		kept = append(kept, a.Kept)
	}
	var c colInsert
	c.text("silver_source_id", src)
	c.text("account_external_id", acct)
	c.text("instrument_external_id", inst)
	c.text("currency", ccy)
	c.int64s("anchored_at", at)
	c.floats("book_quantity", "DECIMAL(28, 8)", bookQty)
	c.floats("stated_quantity", "DECIMAL(28, 8)", statedQty)
	c.floats("matched_quantity", "DECIMAL(28, 8)", matchedQty)
	c.floats("book_cost", "DECIMAL(28, 4)", bookCost)
	c.floats("stated_cost", "DECIMAL(28, 4)", statedCost)
	c.floats("resolved_quantity", "DECIMAL(28, 8)", resolvedQty)
	c.bools("kept", kept)
	c.constant("build_id", w.build)
	return c.exec(ctx, w.tx, "lot_anchors")
}

// writePositions writes each observation's book onto the rows of its
// snapshot that state no basis of their own, on a source the engine
// fills. Several rows share a key's book pro rata by quantity (a pooled
// portfolio's wallets); the row holding most carries the lot count, so
// the counts add up.
//
// A snapshot taken while a move is in flight is not reconciled, so the
// book can hold less or more than the rows. The rows then take what the
// book holds of them: a shortfall is quantity without a basis, and an
// excess leaves pro rata.
func (w *lotWriter) writePositions(ctx context.Context) error {
	var (
		src, acct, posKey, method, fees []string
		book, known, unknown            []float64
		snap, open                      []int64
	)
	for _, o := range w.res.Observations {
		t := w.f.targets[o.Obs]
		if !w.fill(o.Key) || len(t.rows) == 0 {
			continue
		}
		info := w.f.info[o.Key]
		bookKnown, bookUnknown := o.Known, o.Unknown
		switch diff := t.total - o.Qty; {
		case diff > lots.Tolerance(t.total):
			bookUnknown += diff
		case -diff > lots.Tolerance(o.Qty):
			bookKnown *= t.total / o.Qty
			bookUnknown *= t.total / o.Qty
		}
		complete := bookUnknown <= lots.Tolerance(t.total)
		for i, r := range t.rows {
			if r.stated {
				continue
			}
			share := 1 / float64(len(t.rows))
			if t.total > 0 {
				share = r.qty / t.total
			}
			n := int64(0)
			if i == t.top {
				n = int64(o.Lots)
			}
			src = append(src, info.src.id)
			snap = append(snap, r.snapshot)
			acct = append(acct, r.acct)
			posKey = append(posKey, r.posKey)
			book = append(book, lotVal(bookKnown*share, complete))
			known = append(known, lotVal(bookKnown*share, true))
			unknown = append(unknown, lotVal(bookUnknown*share, true))
			open = append(open, n)
			method = append(method, w.f.keys[o.Key].Method.String())
			fees = append(fees, info.src.pol.Fees.String())
			w.sums[info.src.id].PositionsFilled++
		}
	}
	if _, err := w.tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE lot_fill (
    silver_source_id TEXT, snapshot_at BIGINT, account_external_id TEXT, position_key TEXT,
    book_value DECIMAL(28, 4), book_value_known DECIMAL(28, 4), quantity_without_basis DECIMAL(28, 8),
    open_lots INTEGER, basis_method TEXT, basis_fees TEXT)`); err != nil {
		return fmt.Errorf("fill table: %w", err)
	}
	var c colInsert
	c.text("silver_source_id", src)
	c.int64s("snapshot_at", snap)
	c.text("account_external_id", acct)
	c.text("position_key", posKey)
	c.floats("book_value", "DECIMAL(28, 4)", book)
	c.floats("book_value_known", "DECIMAL(28, 4)", known)
	c.floats("quantity_without_basis", "DECIMAL(28, 8)", unknown)
	c.int64s("open_lots", open)
	c.text("basis_method", method)
	c.text("basis_fees", fees)
	if err := c.exec(ctx, w.tx, "lot_fill"); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
UPDATE positions p
   SET book_value = f.book_value, book_value_known = f.book_value_known,
       quantity_without_basis = f.quantity_without_basis, open_lots = f.open_lots,
       basis_origin = CASE WHEN f.book_value IS NOT NULL THEN 'rebuilt' END,
       basis_method = CASE WHEN f.book_value IS NOT NULL THEN f.basis_method END,
       basis_fees   = CASE WHEN f.book_value IS NOT NULL THEN f.basis_fees END
  FROM lot_fill f
 WHERE p.silver_source_id = f.silver_source_id AND p.snapshot_at = f.snapshot_at
   AND p.account_external_id = f.account_external_id AND p.position_key = f.position_key
   AND p.book_value IS NULL`); err != nil {
		return fmt.Errorf("fill positions: %w", err)
	}
	_, err := w.tx.ExecContext(ctx, `DROP TABLE lot_fill`)
	return err
}

func (w *lotWriter) writeRuns(ctx context.Context, prints map[string]string) error {
	methods := map[string]map[string]bool{}
	for i, k := range w.f.keys {
		id := w.f.info[i].src.id
		if methods[id] == nil {
			methods[id] = map[string]bool{}
		}
		methods[id][k.Method.String()] = true
	}
	for _, e := range w.f.events {
		w.sums[w.f.info[e.Key].src.id].Events++
	}
	now := time.Now().Unix()
	for _, id := range w.f.ids {
		s := w.sums[id]
		ms := make([]string, 0, len(methods[id]))
		for m := range methods[id] {
			ms = append(ms, m)
		}
		slices.Sort(ms)
		s.Methods = strings.Join(ms, ",")
		s.DatedBySettlement = w.f.sources[id].pol.DatedBySettlement
		if _, err := w.tx.ExecContext(ctx, `
INSERT INTO lot_runs (build_id, silver_source_id, built_at, mode, grain, methods, fees, events, lots, disposals,
    realized_lots, positions_filled, seeds, implied_disposals, blips, anchors, anchors_kept, seeds_resolved,
    fee_unvalued, keys_skipped, dated_by_settlement, input_fingerprint)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.build, id, now, s.Mode, s.Grain, s.Methods, s.Fees, s.Events, s.Lots, s.Disposals,
			s.RealizedLots, s.PositionsFilled, s.Seeds, s.ImpliedDisposals, s.Blips, s.Anchors, s.AnchorsKept,
			s.SeedsResolved, s.FeeUnvalued, s.KeysSkipped, s.DatedBySettlement, prints[id]); err != nil {
			return fmt.Errorf("lots: run: %w", err)
		}
	}
	return nil
}
