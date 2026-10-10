package gold

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// The lot engine's feed: gold rows into lots.Events (docs/LOTS.md §3).
// One generic reader serves every source; the source kind's registered
// lots.Policy says how to read its transactions.

// lotSource is one source a pass replays.
type lotSource struct {
	id, kind string
	pol      lots.Policy
}

type lotKeyRef struct{ src, acct, inst string }

// lotKeyInfo is what the writer needs to know of a key.
type lotKeyInfo struct {
	src *lotSource
	// acct is the key's account: the account itself, or its portfolio
	// when the source pools at the portfolio grain.
	acct string
	inst string
	// hint marks an instrument only a hint names.
	hint     bool
	currency string
	// posCurrency marks a currency a position row set, which no trade
	// overrides.
	posCurrency bool
}

// lotRow is a position row an observation writes to.
type lotRow struct {
	snapshot     int64
	acct, posKey string
	qty          float64
	stated       bool
}

// lotObs is one observation's position rows, their total, and the row
// that holds most: the account a pooled key's observation names, and
// the row its lot count goes on.
type lotObs struct {
	rows  []lotRow
	total float64
	top   int
}

// lotWallet is what one account of a pooled key holds: what it held
// when it last appeared in a snapshot, and its trades since, up to
// next (readPositions).
type lotWallet struct {
	acct string
	qty  float64
	next int
}

// lotWalletRef names one account of a pooled key.
type lotWalletRef struct {
	key  int32
	acct string
}

// lotWalletTrade is a trade's signed quantity on one account of a
// pooled key.
type lotWalletTrade struct {
	at  int64
	qty float64
}

// lotTrade is one transaction time of a key, for the zero observations
// (readPositions).
type lotTrade struct {
	at  int64
	key int32
}

// lotLeg is one leg of a transfer, before pairing.
type lotLeg struct {
	src  *lotSource
	key  int32
	inst string
	txn  string
	acct string
	at   int64
	qty  float64
	in   bool
	// cost is the cost basis a hand-kept equity-transfer ledger row
	// states for what it moved (loader.TransferEntry), 0 when none.
	cost float64
}

// lotCorpLeg is one leg of a corporate action, before grouping.
type lotCorpLeg struct {
	src  *lotSource
	key  int32
	acct string
	txn  string
	at   int64
	qty  float64 // signed: negative leaves, positive arrives
	amt  float64 // in ccy
	ccy  string
	has  bool
	rule lots.CorporateRule
}

// lotAux is what an event's value still needs once every key's
// currency is known.
type lotAux struct {
	ccy    string  // the currency Value is in
	fmv    bool    // Value is to be the market value of Qty
	fee    float64 // a third-currency fee to value
	feeCcy string
}

type lotFeed struct {
	cfg     lots.Config
	sources map[string]*lotSource
	ids     []string
	pid     map[[2]string]string
	never   map[[2]string]bool
	rates   *lotRates

	keys  []lots.KeySpec
	info  []lotKeyInfo
	keyOf map[lotKeyRef]int32
	// trades lists each key account's transactions, in time order.
	trades map[[2]string][]lotTrade
	// walletTrades lists each pooled key's trades per account, in time
	// order.
	walletTrades map[lotWalletRef][]lotWalletTrade

	events []lots.Event
	aux    []lotAux
	// transits numbers the paired transfers (lots.Event.Transit).
	transits int32
	legs     []lotLeg
	corp     []lotCorpLeg

	// targets are the rows each observation writes to.
	targets []lotObs
	// documented marks a transaction in a year its account's documents
	// cover: the engine's realized lots for it are not primary.
	documented map[[2]string]bool
	// findings are the feed's own, made outside the replay.
	findings []lots.Finding
	skipped  map[string]int
}

// newLotFeed reads which sources a pass replays: every source whose
// policy is not off. A move links sources, so a pass replays them all
// together.
func newLotFeed(ctx context.Context, db *sql.DB, cfg lots.Config) (*lotFeed, error) {
	f := &lotFeed{
		cfg:          cfg,
		sources:      map[string]*lotSource{},
		pid:          map[[2]string]string{},
		never:        map[[2]string]bool{},
		keyOf:        map[lotKeyRef]int32{},
		trades:       map[[2]string][]lotTrade{},
		walletTrades: map[lotWalletRef][]lotWalletTrade{},
		documented:   map[[2]string]bool{},
		skipped:      map[string]int{},
	}
	return f, f.readSources(ctx, db)
}

// read reads the replayed sources' rows into events.
func (f *lotFeed) read(ctx context.Context, db *sql.DB) error {
	if len(f.ids) == 0 {
		return nil
	}
	for _, step := range []func(context.Context, *sql.DB) error{
		f.readPortfolios, f.readClasses, f.readRates, f.readTransactions,
		f.readRealized, f.readDocumented, f.readPositions, f.readAnchors,
	} {
		if err := step(ctx, db); err != nil {
			return err
		}
	}
	f.pairTransfers()
	f.corporateActions()
	f.resolveValues()
	return nil
}

func (f *lotFeed) readSources(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id, silver_kind FROM silver_sources ORDER BY silver_source_id`)
	if err != nil {
		return fmt.Errorf("lots: read sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return err
		}
		p := f.cfg.PolicyFor(id, kind)
		if p.Mode == lots.Off {
			continue
		}
		f.sources[id] = &lotSource{id: id, kind: kind, pol: p}
		f.ids = append(f.ids, id)
	}
	return rows.Err()
}

// inLotSources restricts col to the replayed sources; it binds f.ids.
func inLotSources(col string) string { return col + " IN (SELECT unnest(?::VARCHAR[]))" }

func (f *lotFeed) readPortfolios(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT src, acct, pid FROM portfolio_acct_map()`)
	if err != nil {
		return fmt.Errorf("lots: read portfolios: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, acct, pid string
		if err := rows.Scan(&src, &acct, &pid); err != nil {
			return err
		}
		f.pid[[2]string{src, acct}] = pid
	}
	return rows.Err()
}

// readClasses marks the instruments that are never a key: a holding no
// cost basis describes (cash, a mortgage, an FX forward; GAINS.md §1),
// and one the source ever holds short, which a long-only book cannot
// carry.
func (f *lotFeed) readClasses(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, holding_key(instrument_external_id, position_key),
       COALESCE(bool_or(quantity < 0), FALSE) AS short,
       bool_or(NOT basis_applies(asset_class, vehicle)) AS no_basis
  FROM positions WHERE `+inLotSources("silver_source_id")+` GROUP BY ALL`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read classes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, inst string
		var short, noBasis bool
		if err := rows.Scan(&src, &inst, &short, &noBasis); err != nil {
			return err
		}
		if short || noBasis {
			f.never[[2]string{src, inst}] = true
		}
		if short && !noBasis {
			f.skipped[src]++
		}
	}
	return rows.Err()
}

func (f *lotFeed) readRates(ctx context.Context, db *sql.DB) error {
	r, err := loadLotRates(ctx, db)
	f.rates = r
	return err
}

// keyAccount is the account part of a key: the account, or its
// portfolio when the source pools.
func (f *lotFeed) keyAccount(s *lotSource, acct string) string {
	if s.pol.Grain == lots.GrainPortfolio {
		if pid := f.pid[[2]string{s.id, acct}]; pid != "" {
			return pid
		}
	}
	return acct
}

// key returns the key of an instrument in an account, opening it on
// first sight; -1 when the instrument is never a key.
func (f *lotFeed) key(s *lotSource, acct, inst string, hint bool) int32 {
	if inst == "" || f.never[[2]string{s.id, inst}] || (s.pol.Skip != nil && s.pol.Skip(inst)) {
		return -1
	}
	ka := f.keyAccount(s, acct)
	ref := lotKeyRef{s.id, ka, inst}
	if k, ok := f.keyOf[ref]; ok {
		return k
	}
	portfolio := f.pid[[2]string{s.id, acct}]
	account := acct
	if ka != acct {
		account = ""
	}
	k := int32(len(f.keys))
	f.keys = append(f.keys, lots.KeySpec{
		ID:     s.id + "\x00" + ka + "\x00" + inst,
		Method: f.cfg.MethodFor(s.id, portfolio, account, s.pol.Method),
		Fees:   s.pol.Fees,
	})
	f.info = append(f.info, lotKeyInfo{src: s, acct: ka, inst: inst, hint: hint})
	f.keyOf[ref] = k
	return k
}

// setCurrency records the currency a key's amounts are in: the first
// position row's, else the first trade's. resolveValues converts every
// amount into it.
func (f *lotFeed) setCurrency(k int32, ccy string, fromPosition bool) {
	info := &f.info[k]
	if ccy == "" || info.posCurrency || (info.currency != "" && !fromPosition) {
		return
	}
	info.currency, info.posCurrency = ccy, fromPosition
}

func (f *lotFeed) add(ev lots.Event, aux lotAux) {
	f.events = append(f.events, ev)
	f.aux = append(f.aux, aux)
}

func (f *lotFeed) readTransactions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, transaction_external_id, occurred_at, account_external_id,
       instrument_external_id, instrument_hint, kind, currency,
       CAST(quantity AS DOUBLE), CAST(net_amount AS DOUBLE), COALESCE(description, ''),
       COALESCE(json_extract_string(payload, '$.type'), ''),
       COALESCE(json_extract_string(payload, '$.comment'), ''),
       COALESCE(json_extract_string(payload, '$.Action'), json_extract_string(payload, '$.action'), ''),
       COALESCE(json_extract_string(payload, '$.buy_currency'), ''),
       COALESCE(json_extract_string(payload, '$.sell_currency'), ''),
       COALESCE(TRY_CAST(json_extract_string(payload, '$.fee_amount') AS DOUBLE), 0),
       COALESCE(json_extract_string(payload, '$.fee_currency'), ''),
       CASE WHEN json_extract_string(payload, '$.equity_transfer_ledger') = 'true'
            THEN COALESCE(TRY_CAST(json_extract_string(payload, '$.cost_basis') AS DOUBLE), 0) ELSE 0 END
  FROM transactions
 WHERE `+inLotSources("silver_source_id")+`
   AND (instrument_external_id IS NOT NULL OR instrument_hint IS NOT NULL)
 ORDER BY occurred_at, silver_source_id, transaction_external_id`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read transactions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			src, txn, acct, kind, ccy string
			inst, hint                sql.NullString
			at                        int64
			qty, amt                  sql.NullFloat64
			t                         lots.Txn
			ledgerCost                float64
		)
		if err := rows.Scan(&src, &txn, &at, &acct, &inst, &hint, &kind, &ccy, &qty, &amt,
			&t.Description, &t.Type, &t.Comment, &t.Action, &t.BuyCurrency, &t.SellCurrency,
			&t.FeeAmount, &t.FeeCurrency, &ledgerCost); err != nil {
			return fmt.Errorf("lots: read transactions: %w", err)
		}
		s := f.sources[src]
		name, isHint := inst.String, false
		if !inst.Valid {
			name, isHint = hint.String, true
		}
		t.ID, t.Kind, t.Quantity = txn, kind, qty.Float64
		classify := s.pol.Classify
		if classify == nil {
			classify = lots.DefaultClassify
		}
		c := classify(t)
		if c.Action == lots.Ignore {
			continue
		}
		k := f.key(s, acct, name, isHint)
		if k < 0 {
			continue
		}
		ka := [2]string{src, f.info[k].acct}
		f.trades[ka] = append(f.trades[ka], lotTrade{at: at, key: k})
		if ka[1] != acct {
			w := lotWalletRef{k, acct}
			f.walletTrades[w] = append(f.walletTrades[w], lotWalletTrade{at, qty.Float64})
		}
		q := math.Abs(qty.Float64)
		ev := lots.Event{At: at, Key: k, Txn: txn, Account: acct, Qty: q, AcqDay: lots.DayOf(at)}
		aux := lotAux{ccy: ccy}
		if c.Fee && t.FeeAmount != 0 {
			aux.fee, aux.feeCcy = math.Abs(t.FeeAmount), t.FeeCurrency
		}
		switch c.Action {
		case lots.Buy:
			if ccy != name {
				f.setCurrency(k, ccy, false)
			}
			ev.Kind, ev.Origin, ev.CostOrigin = lots.Acquire, lots.OriginBuy, lots.CostTrade
			ev.Value, ev.HasValue = math.Abs(amt.Float64), amt.Valid
		case lots.Sell:
			if ccy != name {
				f.setCurrency(k, ccy, false)
			}
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
			ev.Value, ev.HasValue = math.Abs(amt.Float64), amt.Valid
		case lots.Income, lots.Acquired:
			ev.Kind, ev.Origin, ev.CostOrigin, aux.fmv = lots.Acquire, lots.OriginIncome, lots.CostFMV, true
			if c.Action == lots.Acquired {
				ev.Origin = lots.OriginBuy
			}
		case lots.Exchange:
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
			aux.fmv = true
		case lots.Gone:
			ev.Kind, ev.Disposal = lots.Dispose, c.Disposal
		case lots.In, lots.Out:
			f.legs = append(f.legs, lotLeg{src: s, key: k, inst: name, txn: txn, acct: acct, at: at, qty: q,
				in: c.Action == lots.In, cost: ledgerCost})
			continue
		case lots.Corporate:
			f.corp = append(f.corp, lotCorpLeg{src: s, key: k, acct: acct, txn: txn, at: at,
				qty: qty.Float64, amt: amt.Float64, ccy: ccy, has: amt.Valid && amt.Float64 != 0, rule: c.Rule})
			continue
		}
		if !(q > 0) {
			continue
		}
		f.add(ev, aux)
	}
	return rows.Err()
}

// readDocumented marks the transactions the sources document
// (documented_txns, the test the gains reports apply): the engine's
// realized lots for them are not primary, so no sale counts twice.
func (f *lotFeed) readDocumented(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT src, txn FROM documented_txns() WHERE `+inLotSources("src"), f.ids)
	if err != nil {
		return fmt.Errorf("lots: read documented sales: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			return err
		}
		f.documented[k] = true
	}
	return rows.Err()
}

// readRealized gives each sale the realized lots its source states for
// it (docs/LOTS.md §5.3), where a seed it relieves takes their cost and
// date (§4): the lots that name its instrument and fall within two days
// of it, up to its quantity, on the sale's own account first, then on
// the other accounts of its portfolio. A document's CUSIP counts as the
// ticker the trades use where the source's instruments link the two and
// no key uses the CUSIP itself (lotAliases).
func (f *lotFeed) readRealized(ctx context.Context, db *sql.DB) error {
	stated, err := f.readStatedLots(ctx, db)
	if err != nil {
		return err
	}
	// The sales come in time order, so a group's lots claimed in full
	// before the current sale's window stay behind a cursor.
	claimed := map[lotStatedGroup]int{}
	for i := range f.events {
		ev := &f.events[i]
		if ev.Kind != lots.Dispose || ev.Disposal != lots.DisposeSell {
			continue
		}
		info := f.info[ev.Key]
		port := f.portOf(info.src.id, ev.Account)
		// The group is in date order: start at the first lot two days
		// before the sale, stop past two days after.
		day := lots.DayOf(ev.At)
		g := lotStatedGroup{info.src.id, port, info.inst}
		group := stated[g]
		c := claimed[g]
		for c < len(group) && group[c].left <= 0 {
			c++
		}
		claimed[g] = c
		j, _ := slices.BinarySearchFunc(group[c:], day-2, func(s *lotStated, d int32) int { return cmp.Compare(s.day, d) })
		j += c
		need := ev.Qty
		for _, own := range []bool{true, false} {
			for _, s := range group[j:] {
				if s.day > day+2 || need <= lots.Tolerance(ev.Qty) {
					break
				}
				if s.left <= 0 || (s.acct == ev.Account) != own {
					continue
				}
				q := math.Min(need, s.left)
				part := s.lot
				part.Qty, part.Cost = q, s.lot.Cost*q/s.lot.Qty
				ev.Lots = append(ev.Lots, part)
				s.left -= q
				need -= q
			}
		}
	}
	return nil
}

// lotStated is a stated primary realized lot, and what of it the sales
// have not yet claimed.
type lotStated struct {
	acct string
	day  int32
	lot  lots.StatedLot
	left float64
}

type lotStatedGroup struct{ src, port, inst string }

// readStatedLots reads the stated primary realized lots of the
// replayed sources, grouped by source, port and instrument, each group
// in date order.
func (f *lotFeed) readStatedLots(ctx context.Context, db *sql.DB) (map[lotStatedGroup][]*lotStated, error) {
	alias, err := f.lotAliases(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, account_external_id, lot_key(instrument_external_id, instrument_hint, description),
       CAST(epoch(realized_effective_date(disposal_date, settlement_date, tax_year)) AS BIGINT),
       CAST(quantity AS DOUBLE), CAST(book_value AS DOUBLE), CAST(epoch(acquisition_date) AS BIGINT)
  FROM realized_lots
 WHERE is_primary AND quantity <> 0 AND `+inLotSources("silver_source_id")+`
 ORDER BY 1, 2, 3, 4, acquisition_date, realized_lot_external_id`, f.ids)
	if err != nil {
		return nil, fmt.Errorf("lots: read realized lots: %w", err)
	}
	defer rows.Close()
	// The ids the keys use: a stated id that is one stays as it is.
	keyed := make(map[[2]string]bool, len(f.info))
	for _, info := range f.info {
		keyed[[2]string{info.src.id, info.inst}] = true
	}
	out := map[lotStatedGroup][]*lotStated{}
	for rows.Next() {
		var (
			src, acct, inst string
			at              int64
			qty             float64
			cost            sql.NullFloat64
			acq             sql.NullInt64
		)
		if err := rows.Scan(&src, &acct, &inst, &at, &qty, &cost, &acq); err != nil {
			return nil, err
		}
		if t, ok := alias[[2]string{src, inst}]; ok && !keyed[[2]string{src, inst}] {
			inst = t
		}
		sl := statedLot(qty, cost.Float64, cost.Valid, acq)
		g := lotStatedGroup{src, f.portOf(src, acct), inst}
		out[g] = append(out[g], &lotStated{acct: acct, day: lots.DayOf(at), lot: sl, left: sl.Qty})
	}
	for _, g := range out {
		slices.SortStableFunc(g, func(a, b *lotStated) int { return cmp.Compare(a.day, b.day) })
	}
	return out, rows.Err()
}

// statedLot is a lot a source states, from its quantity, cost and
// acquisition time; an absent cost or date is unknown.
func statedLot(qty, cost float64, costKnown bool, acq sql.NullInt64) lots.StatedLot {
	sl := lots.StatedLot{Qty: math.Abs(qty), Cost: math.Abs(cost), CostKnown: costKnown, AcqDay: lots.NoDay}
	if acq.Valid {
		sl.AcqDay = lots.DayOf(acq.Int64)
	}
	return sl
}

// lotAliases maps an instrument a source keys by its CUSIP to the ticker
// the source also keys it by, where its instruments carry both rows with
// one symbol: a statement names a security by CUSIP and the trades by
// ticker.
func (f *lotFeed) lotAliases(ctx context.Context, db *sql.DB) (map[[2]string]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT c.silver_source_id, c.instrument_external_id, c.symbol
  FROM instruments c
  JOIN instruments t ON t.silver_source_id = c.silver_source_id AND t.instrument_external_id = c.symbol
 WHERE c.cusip = c.instrument_external_id AND c.symbol <> c.instrument_external_id
   AND `+inLotSources("c.silver_source_id"), f.ids)
	if err != nil {
		return nil, fmt.Errorf("lots: read instrument aliases: %w", err)
	}
	defer rows.Close()
	out := map[[2]string]string{}
	for rows.Next() {
		var src, id, ticker string
		if err := rows.Scan(&src, &id, &ticker); err != nil {
			return nil, err
		}
		out[[2]string{src, id}] = ticker
	}
	return out, rows.Err()
}

// portOf is the account's portfolio, or the account itself outside
// any: what a stated lot and a sale must share.
func (f *lotFeed) portOf(src, acct string) string {
	if p := f.pid[[2]string{src, acct}]; p != "" {
		return "p:" + p
	}
	return "a:" + acct
}

// readPositions turns the snapshots into observations. Each key held
// at a snapshot observes the quantity its rows sum to. A key the
// account held at its previous snapshot, or traded since, observes
// zero when the account appears without it. An account absent from a
// snapshot observes nothing: sources leave accounts out of snapshots.
// So a pooled key's wallet absent from a snapshot holds what it held
// when it last appeared, moved by its trades since, and the key
// observes that too. A wallet its trades emptied holds nothing: a
// source drops an empty wallet from its snapshots.
func (f *lotFeed) readPositions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT silver_source_id, snapshot_at, lot_obs_at(snapshot_at), account_external_id, position_key,
       holding_key(instrument_external_id, position_key), instrument_external_id IS NULL, currency,
       COALESCE(CAST(quantity AS DOUBLE), 0), stated_basis(book_value, basis_origin)
  FROM positions WHERE `+inLotSources("silver_source_id")+`
 ORDER BY silver_source_id, snapshot_at, account_external_id, position_key`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read positions: %w", err)
	}
	defer rows.Close()

	type acctRef = [2]string // source, key account
	var (
		curSrc         string
		curSnap, curAt int64 = math.MinInt64, 0
		held                 = map[int32][]lotRow{}
		// heldBy lists the keys each present account holds at the
		// snapshot; lastHeld at its previous one.
		heldBy   = map[acctRef][]int32{}
		lastHeld = map[acctRef][]int32{}
		// cursor is how far into f.trades each account's previous
		// snapshots have read.
		cursor = map[acctRef]int{}
		// holder is the account that last held each key: the one a
		// pooled key's zero observation names.
		holder = map[int32]string{}
		gone   = map[int32]bool{}
		// wallets is what each account of a key held when it last
		// appeared; present marks the accounts of this snapshot.
		wallets = map[int32][]lotWallet{}
		present = map[string]bool{}
	)
	// carried is what k's accounts absent from this snapshot hold at
	// now; the present ones' entries go, for this snapshot's rows to
	// replace, and so do the ones their trades emptied.
	carried := func(k int32, now int64) float64 {
		sum, keep := 0.0, wallets[k][:0]
		for _, w := range wallets[k] {
			if present[w.acct] {
				continue
			}
			ts := f.walletTrades[lotWalletRef{k, w.acct}]
			for ; w.next < len(ts) && ts[w.next].at <= now; w.next++ {
				w.qty += ts[w.next].qty
			}
			if w.qty > lots.Tolerance(w.qty) {
				sum += w.qty
				keep = append(keep, w)
			}
		}
		wallets[k] = keep
		return sum
	}
	// seen records what each of k's accounts holds at now.
	seen := func(k int32, now int64, rs []lotRow) {
		for _, r := range rs {
			ts := f.walletTrades[lotWalletRef{k, r.acct}]
			next, _ := slices.BinarySearchFunc(ts, now+1, func(t lotWalletTrade, at int64) int { return cmp.Compare(t.at, at) })
			if i := len(wallets[k]) - 1; i >= 0 && wallets[k][i].acct == r.acct {
				wallets[k][i].qty += r.qty
				continue
			}
			wallets[k] = append(wallets[k], lotWallet{r.acct, r.qty, next})
		}
	}
	flush := func() {
		if curSrc == "" {
			return
		}
		at := curAt
		txn := "snapshot:" + strconv.FormatInt(at, 10)
		ks := make([]int32, 0, len(held))
		for k := range held {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		for _, k := range ks {
			rs := held[k]
			o := lotObs{rows: rs}
			for i, r := range rs {
				o.total += r.qty
				if r.qty > rs[o.top].qty {
					o.top = i
				}
			}
			qty := o.total + carried(k, at)
			seen(k, at, rs)
			f.observe(k, at, txn, qty, rs[o.top].acct, o)
			holder[k] = rs[o.top].acct
		}
		accts := make([]acctRef, 0, len(heldBy))
		for a := range heldBy {
			accts = append(accts, a)
		}
		slices.SortFunc(accts, func(a, b acctRef) int { return cmp.Compare(a[1], b[1]) })
		for _, a := range accts {
			// What the account held last time, and what it traded
			// since, and does not hold now, observes zero.
			clear(gone)
			for _, k := range lastHeld[a] {
				gone[k] = true
			}
			ts, i := f.trades[a], cursor[a]
			for ; i < len(ts) && ts[i].at <= at; i++ {
				gone[ts[i].key] = true
			}
			cursor[a] = i
			for _, k := range heldBy[a] {
				delete(gone, k)
			}
			zs := make([]int32, 0, len(gone))
			for k := range gone {
				zs = append(zs, k)
			}
			slices.Sort(zs)
			for _, k := range zs {
				acct := holder[k]
				if acct == "" {
					acct = a[1]
				}
				f.observe(k, at, txn, carried(k, at), acct, lotObs{})
			}
			lastHeld[a] = append(lastHeld[a][:0], heldBy[a]...)
		}
		clear(held)
		clear(heldBy)
		clear(present)
	}
	for rows.Next() {
		var (
			src, acct, posKey, inst, ccy string
			snap, obsAt                  int64
			isHint, stated               bool
			qty                          float64
		)
		if err := rows.Scan(&src, &snap, &obsAt, &acct, &posKey, &inst, &isHint, &ccy, &qty, &stated); err != nil {
			return fmt.Errorf("lots: read positions: %w", err)
		}
		if src != curSrc || snap != curSnap {
			flush()
			curSrc, curSnap, curAt = src, snap, obsAt
		}
		s := f.sources[src]
		a := acctRef{src, f.keyAccount(s, acct)}
		if _, ok := heldBy[a]; !ok {
			heldBy[a] = nil
		}
		present[acct] = true
		k := f.key(s, acct, inst, isHint)
		if k < 0 {
			continue
		}
		if _, ok := held[k]; !ok {
			heldBy[a] = append(heldBy[a], k)
		}
		f.setCurrency(k, ccy, true)
		held[k] = append(held[k], lotRow{snapshot: snap, acct: acct, posKey: posKey, qty: qty, stated: stated})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

func (f *lotFeed) observe(k int32, at int64, txn string, qty float64, acct string, o lotObs) {
	id := int32(len(f.targets))
	f.targets = append(f.targets, o)
	f.add(lots.Event{At: at, Kind: lots.Observe, Key: k, Txn: txn, Account: acct, Qty: qty, Obs: id}, lotAux{})
}

// readAnchors turns the lots a source states at a snapshot into
// anchors, one per position with lots. A pooled key has no anchor: a
// wallet's lots do not describe the pool.
func (f *lotFeed) readAnchors(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
SELECT l.silver_source_id, l.snapshot_at, lot_obs_at(l.snapshot_at), l.account_external_id,
       holding_key(p.instrument_external_id, l.position_key), p.instrument_external_id IS NULL,
       CAST(l.quantity AS DOUBLE), CAST(l.book_value AS DOUBLE), CAST(epoch(l.acquisition_date) AS BIGINT)
  FROM position_lots l
  JOIN positions p ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snapshot_at
   AND p.account_external_id = l.account_external_id AND p.position_key = l.position_key
 WHERE `+inLotSources("l.silver_source_id")+`
 ORDER BY 1, 2, 4, 5, l.acquisition_date, l.lot_key`, f.ids)
	if err != nil {
		return fmt.Errorf("lots: read anchors: %w", err)
	}
	defer rows.Close()
	var (
		cur  lots.Event
		have bool
		bad  bool
	)
	emit := func() {
		if have && !bad && len(cur.Lots) > 0 {
			f.add(cur, lotAux{})
		}
		have, bad = false, false
	}
	for rows.Next() {
		var (
			src, acct, inst string
			snap, obsAt     int64
			isHint          bool
			qty, cost       sql.NullFloat64
			acq             sql.NullInt64
		)
		if err := rows.Scan(&src, &snap, &obsAt, &acct, &inst, &isHint, &qty, &cost, &acq); err != nil {
			return err
		}
		s := f.sources[src]
		if s.pol.Grain == lots.GrainPortfolio {
			continue
		}
		k := f.key(s, acct, inst, isHint)
		if k < 0 {
			continue
		}
		if !have || cur.Key != k || cur.At != obsAt {
			emit()
			cur = lots.Event{At: obsAt, Kind: lots.Anchor, Key: k, Txn: fmt.Sprintf("anchor:%d", snap), Account: acct}
			have = true
		}
		if !qty.Valid || qty.Float64 <= 0 {
			// A lot without a positive quantity cannot stand for the
			// position: the anchor is incomplete, and is not adopted.
			bad = true
			continue
		}
		cur.Lots = append(cur.Lots, statedLot(qty.Float64, cost.Float64, cost.Valid, acq))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	emit()
	return nil
}

// pairTransfers pairs each transfer in with a transfer out of the same
// instrument within lots.PairWindow and lots.PairTolerance. Inside one
// key (a pooled portfolio) a pair cancels: nothing moves. Across keys a
// pair is a move; across sources only between sources of one kind,
// whose instrument ids mean the same thing. Either way, what the
// receipt falls short by leaves as a fee in kind, and what it exceeds
// the send by opens a lot of unknown cost. What pairs with nothing is a
// receipt of unknown cost, or a departure with no proceeds.
func (f *lotFeed) pairTransfers() {
	legs := f.legs
	slices.SortStableFunc(legs, func(a, b lotLeg) int {
		if c := cmp.Compare(a.inst, b.inst); c != 0 {
			return c
		}
		if c := cmp.Compare(a.at, b.at); c != 0 {
			return c
		}
		return cmp.Compare(a.txn, b.txn)
	})
	mate := make([]int, len(legs))
	for i := range mate {
		mate[i] = -1
	}
	// open lists the earlier legs of the instrument still unpaired, per
	// direction (outs, ins), oldest first: a leg looks only at the other
	// direction's, within the window, latest first.
	var open [2][]int
	dir := func(l *lotLeg) int {
		if l.in {
			return 1
		}
		return 0
	}
	for i := range legs {
		a := &legs[i]
		if i == 0 || legs[i-1].inst != a.inst {
			open[0], open[1] = open[0][:0], open[1][:0]
		}
		other := 1 - dir(a)
		q := open[other][:0]
		for _, j := range open[other] {
			if mate[j] < 0 && a.at-legs[j].at <= lots.PairWindow {
				q = append(q, j)
			}
		}
		open[other] = q
		best, bestDiff := -1, math.Inf(1)
		for x := len(q) - 1; x >= 0; x-- {
			b := &legs[q[x]]
			out, recv := b, a
			if b.in {
				out, recv = a, b
			}
			if out.src != recv.src && out.src.kind != recv.src.kind {
				continue
			}
			diff := math.Abs(out.qty - recv.qty)
			if diff > lots.PairTolerance*math.Max(out.qty, recv.qty) {
				continue
			}
			if diff < bestDiff {
				best, bestDiff = q[x], diff
			}
		}
		if best >= 0 {
			mate[i], mate[best] = best, i
		} else {
			open[dir(a)] = append(open[dir(a)], i)
		}
	}
	for i := range legs {
		a := legs[i]
		j := mate[i]
		if j < 0 {
			f.unpaired(a)
			continue
		}
		if a.in {
			// Each pair is emitted once, from its outgoing leg.
			continue
		}
		f.pair(a, legs[j])
	}
	f.legs = nil
}

func (f *lotFeed) unpaired(l lotLeg) {
	ev := lots.Event{At: l.at, Key: l.key, Txn: l.txn, Account: l.acct, Qty: l.qty, AcqDay: lots.DayOf(l.at)}
	if l.in {
		ev.Kind, ev.Origin = lots.Acquire, lots.OriginTransferIn
		if l.cost > 0 {
			// A hand-kept ledger row states the basis it arrived with.
			ev.CostOrigin, ev.Value, ev.HasValue = lots.CostStated, l.cost, true
		}
		// The cost and holding period of a receipt nothing explains are
		// unknown.
		ev.AcqDay = lots.NoDay
	} else {
		ev.Kind, ev.Disposal = lots.Dispose, lots.DisposeTransferOut
	}
	f.add(ev, lotAux{})
	f.findings = append(f.findings, lots.Finding{Kind: lots.FindUnpairedTransfer, Key: l.key, Account: l.acct, At: l.at, Qty: l.qty, Txn: l.txn})
}

// pair moves a paired transfer's lots: they leave at the departure and
// arrive at the receipt, so a sale on either side between the two
// relieves what each side held. A receipt recorded before its
// departure moves the lots at the receipt; the sender's snapshots until
// the departure still show them, and are not reconciled (lots.Flight).
func (f *lotFeed) pair(out, in lotLeg) {
	moved := math.Min(out.qty, in.qty)
	if out.key != in.key {
		f.transits++
		departAt := out.at
		if in.at < out.at {
			departAt = in.at
			f.add(lots.Event{At: in.at, Kind: lots.Flight, Key: out.key, Txn: in.txn, Account: out.acct}, lotAux{})
			f.add(lots.Event{At: out.at, Kind: lots.Land, Key: out.key, Txn: in.txn, Account: out.acct}, lotAux{})
		}
		f.add(lots.Event{At: departAt, Kind: lots.Depart, Key: out.key, Transit: f.transits, Txn: in.txn,
			Account: out.acct, Qty: moved}, lotAux{})
		f.add(lots.Event{At: in.at, Kind: lots.Arrive, Key: in.key, Transit: f.transits, Txn: in.txn,
			Account: in.acct}, lotAux{})
	}
	switch {
	case out.qty-moved > lots.Tolerance(out.qty):
		// The receipt is short by a network fee: that quantity left as
		// a fee, at its market value.
		f.add(lots.Event{At: out.at, Kind: lots.Dispose, Key: out.key, Txn: out.txn, Account: out.acct,
			Qty: out.qty - moved, Disposal: lots.DisposeFee}, lotAux{fmv: true})
	case in.qty-moved > lots.Tolerance(in.qty):
		ev := lots.Event{At: in.at, Kind: lots.Acquire, Key: in.key, Txn: in.txn, Account: in.acct,
			Qty: in.qty - moved, AcqDay: lots.NoDay, Origin: lots.OriginTransferIn}
		f.add(ev, lotAux{})
	}
}

// corporateActions turns the corporate action legs into events. Legs
// group by account and day. A rule other than CorpReorg acts on its
// leg alone. CorpReorg legs pair within their group: each outgoing
// instrument hands its lots to the incoming ones (docs/LOTS.md §3.5).
func (f *lotFeed) corporateActions() {
	type gk struct {
		src  string
		acct string
		day  int32
	}
	groups := map[gk][]lotCorpLeg{}
	var order []gk
	for _, c := range f.corp {
		switch c.rule {
		case lots.CorpIgnore:
			continue
		case lots.CorpReturnOfCapital:
			if c.has {
				f.add(lots.Event{At: c.at, Kind: lots.Adjust, Key: c.key, Txn: c.txn, Account: c.acct, Value: math.Abs(c.amt), HasValue: true},
					lotAux{ccy: c.ccy})
			}
			continue
		case lots.CorpCashInLieu, lots.CorpExpiry:
			if c.qty >= 0 {
				// Cash in lieu names no quantity: the fraction leaves at
				// the next snapshot.
				continue
			}
			ev := lots.Event{At: c.at, Kind: lots.Dispose, Key: c.key, Txn: c.txn, Account: c.acct, Qty: -c.qty,
				Disposal: lots.DisposeCashInLieu}
			switch {
			case c.rule == lots.CorpExpiry:
				ev.Disposal, ev.HasValue = lots.DisposeExpiry, true
			case c.has:
				ev.Value, ev.HasValue = math.Abs(c.amt), true
			}
			f.add(ev, lotAux{ccy: c.ccy})
			continue
		}
		g := gk{c.src.id, f.keyAccount(c.src, c.acct), lots.DayOf(c.at)}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	for _, g := range order {
		f.reorgGroup(groups[g])
	}
	f.corp = nil
}

// reorgGroup pairs one account's corporate action legs of one day. The
// same leg stated twice (a source with two feeds) counts once.
func (f *lotFeed) reorgGroup(legs []lotCorpLeg) {
	var outs, ins []lotCorpLeg
	type legID struct {
		out bool
		key int32
		qty float64
	}
	seen := map[legID]int{}
	for _, l := range legs {
		if l.qty == 0 {
			continue
		}
		side := &ins
		if l.qty < 0 {
			side = &outs
		}
		id := legID{l.qty < 0, l.key, math.Abs(l.qty)}
		if i, dup := seen[id]; dup {
			if !(*side)[i].has && l.has {
				(*side)[i] = l
			}
			continue
		}
		seen[id] = len(*side)
		*side = append(*side, l)
	}
	switch {
	case len(outs) == 0:
		for _, l := range ins {
			f.add(lots.Event{At: l.at, Kind: lots.Split, Key: l.key, Txn: l.txn, Account: l.acct, Qty: l.qty}, lotAux{})
		}
	case len(ins) == 0:
		for _, l := range outs {
			ev := lots.Event{At: l.at, Kind: lots.Dispose, Key: l.key, Txn: l.txn, Account: l.acct, Qty: -l.qty, Disposal: lots.DisposeReorg}
			if l.has {
				// Shares that leave for cash alone: a cash merger.
				ev.Disposal, ev.Value, ev.HasValue = lots.DisposeTender, math.Abs(l.amt), true
			}
			f.add(ev, lotAux{ccy: l.ccy})
		}
	case len(outs) == 1:
		// One instrument becomes one or several: its lots are shared
		// among them by their stated values.
		o, w := outs[0], legWeights(ins)
		ev := lots.Event{At: o.at, Kind: lots.Reorg, Key: o.key, Txn: o.txn, Account: o.acct, Qty: -o.qty}
		for j, n := range ins {
			ev.At = max(ev.At, n.at)
			ev.Into = append(ev.Into, lots.Into{Key: n.key, Qty: n.qty, Weight: w[j]})
		}
		if len(ins) == 1 {
			ev.Txn, ev.Account = ins[0].txn, ins[0].acct
		}
		f.add(ev, lotAux{})
	case len(ins) == 1:
		// Several instruments become one: each hands over its lots and
		// its share of the new quantity, by the stated values.
		n, w := ins[0], legWeights(outs)
		for i, o := range outs {
			f.add(lots.Event{At: max(o.at, n.at), Kind: lots.Reorg, Key: o.key, Txn: n.txn, Account: n.acct,
				Qty: -o.qty, Into: []lots.Into{{Key: n.key, Qty: n.qty * w[i], Weight: 1}}}, lotAux{})
		}
	default:
		// Several on each side: pair by the closest stated value, else
		// in order; what is left acts alone.
		used := make([]bool, len(ins))
		var restOut []lotCorpLeg
		for _, o := range outs {
			best := -1
			for j, n := range ins {
				if used[j] {
					continue
				}
				if best < 0 || (o.has && n.has && math.Abs(math.Abs(o.amt)-math.Abs(n.amt)) < math.Abs(math.Abs(o.amt)-math.Abs(ins[best].amt))) {
					best = j
				}
			}
			if best < 0 {
				restOut = append(restOut, o)
				continue
			}
			used[best] = true
			n := ins[best]
			f.add(lots.Event{At: max(o.at, n.at), Kind: lots.Reorg, Key: o.key, Txn: n.txn, Account: n.acct,
				Qty: -o.qty, Into: []lots.Into{{Key: n.key, Qty: n.qty, Weight: 1}}}, lotAux{})
		}
		var restIn []lotCorpLeg
		for j, n := range ins {
			if !used[j] {
				restIn = append(restIn, n)
			}
		}
		if len(restOut) > 0 || len(restIn) > 0 {
			f.reorgGroup(append(restOut, restIn...))
		}
	}
}

// legWeights shares one side of a reorg by the legs' stated values,
// else equally.
func legWeights(legs []lotCorpLeg) []float64 {
	w := make([]float64, len(legs))
	total := 0.0
	for _, l := range legs {
		if !l.has {
			total = 0
			break
		}
		total += math.Abs(l.amt)
	}
	for i, l := range legs {
		if total > 0 {
			w[i] = math.Abs(l.amt) / total
		} else {
			w[i] = 1 / float64(len(legs))
		}
	}
	return w
}

// resolveValues settles every value that needed the keys' currencies:
// a market value, a third-currency fee, an amount in another currency.
// A value that has no rate is unknown, and a fee without one is a
// finding.
func (f *lotFeed) resolveValues() {
	for i := range f.events {
		ev, aux := &f.events[i], f.aux[i]
		info := f.info[ev.Key]
		ccy := info.currency
		day := int64(lots.DayOf(ev.At))
		if aux.fmv {
			if r, ok := f.rates.rate(info.inst, ccy, day); ok && ccy != "" {
				ev.Value, ev.HasValue = ev.Qty*r, true
			} else {
				ev.HasValue = false
				if ev.Kind == lots.Acquire {
					ev.CostOrigin = lots.CostNone
				}
			}
		} else if ev.HasValue && aux.ccy != "" && ccy != "" && aux.ccy != ccy {
			if r, ok := f.rates.rate(aux.ccy, ccy, day); ok {
				ev.Value *= r
			} else {
				ev.HasValue = false
			}
		}
		if aux.fee > 0 {
			r, ok := f.rates.rate(aux.feeCcy, ccy, day)
			switch {
			case !ok || ccy == "":
				f.findings = append(f.findings, lots.Finding{Kind: lots.FindFeeUnvalued, Key: ev.Key, Account: ev.Account, At: ev.At, Qty: aux.fee, Txn: ev.Txn})
			case ev.Kind == lots.Acquire && ev.HasValue:
				ev.Value += aux.fee * r
			case ev.Kind == lots.Dispose && ev.HasValue:
				ev.Value = math.Max(ev.Value-aux.fee*r, 0)
			}
		}
	}
	f.aux = nil
}
