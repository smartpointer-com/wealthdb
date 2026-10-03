package plaid

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// account is one row of silver's `accounts` table, with its kind.
type account struct {
	id, name, officialName, mask, typ, subtype, currency string
	balance, available                                   sql.NullString
	payload                                              string
	kind                                                 canonical.AccountKind
}

// holding is one row of silver's `holdings` table.
type holding struct {
	accountID, securityID       string
	quantity, value, costBasis  sql.NullString
	price                       sql.NullString
	vestedQuantity, vestedValue sql.NullString
	currency, payload           string
}

// owned is the part of a holding its holder owns, and the quantity not yet
// vested. Plaid states a vested quantity for equity compensation. Where it
// is below the whole, the position is the vested quantity at the vested
// value. A vested value Plaid leaves empty is the price times the vested
// quantity, else the whole value pro rata.
func (h holding) owned() (quantity, value, unvested *canonical.Decimal) {
	quantity, value = silver.DecimalPtrOrNil(h.quantity), silver.DecimalPtrOrNil(h.value)
	vested := silver.DecimalPtrOrNil(h.vestedQuantity)
	if quantity == nil || vested == nil || vested.IsNegative() || !vested.LessThan(*quantity) {
		return quantity, value, nil
	}
	rest := quantity.Sub(*vested)
	vestedValue := silver.DecimalPtrOrNil(h.vestedValue)
	if price := silver.DecimalPtrOrNil(h.price); vestedValue == nil && price != nil {
		v := price.Mul(*vested)
		vestedValue = &v
	}
	if vestedValue == nil && value != nil {
		v := value.Mul(*vested).Div(*quantity)
		vestedValue = &v
	}
	return vested, vestedValue, &rest
}

// Snapshots emits one batch per run in the window, at the run's start, and
// one per card statement closed in it, at the statement's issue date. A
// batch carries the accounts, instruments and facts of its instant:
//
//   - a cash account: a CURRENT balance, Plaid's balance as it stands, or
//     an AVAILABLE one where the institution states only that;
//   - a card: a CURRENT balance, negated (gold carries what is owed as
//     negative cash), and a CLOSING balance at each statement's issue date;
//   - a mortgage: a negative (real estate, mortgage) position, as the
//     outstanding principal;
//   - an investment account: a position per instrument held, vested shares
//     only, and a CURRENT balance per currency for the cash among its
//     holdings. Plaid's balance of such an account is its total value, so
//     it is never read as cash.
//
// Gold reads a source's current state from its latest instant, so every
// run restates every account. Where Plaid states no balance for an account
// in one run, the last figure it stated is carried into the run, marked
// "carried": unknown is not zero.
//
// A run whose holdings were not read in full (failed, not ready) adds
// nothing: its balances alone at a later instant than the last holdings
// would hide every position and zero nothing. Its balances still count as
// the last figures stated. When an account holds no position after a run
// in which it did, the run replays its last positions at zero
// (silver.ClosureMarkerBatch), so gold does not carry them forward. Cash
// closes the same way, key by key: an account Plaid no longer lists, and
// cash an investment account no longer holds, read zero once (closedCash).
//
// Dimensions travel only on the snapshot stream, so the accounts and
// instruments the window's transactions name are emitted too, on the last
// batch, seen over the span of those transactions.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	secs, err := c.readSecurities(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := c.accountRuns(ctx, w)
	if err != nil {
		return nil, err
	}

	byInstant := map[int64]*canonical.SnapshotBatch{}
	at := func(t int64) *canonical.SnapshotBatch {
		b, ok := byInstant[t]
		if !ok {
			b = &canonical.SnapshotBatch{}
			byInstant[t] = b
		}
		return b
	}

	var (
		lastCash    []cashKey
		lastHeld    map[string]canonical.SnapshotBatch
		newest      int64
		snapshotted bool
	)
	stated := map[string]statedBalance{}
	for _, r := range runs {
		if !r.snapshots {
			if err := c.noteBalances(ctx, r.at, stated); err != nil {
				return nil, err
			}
			continue
		}
		newest, snapshotted = r.at, true
		b := at(r.at)
		cash, err := c.appendRun(ctx, b, r.at, secs, stated)
		if err != nil {
			return nil, err
		}
		b.CashBalances = append(b.CashBalances, closedCash(lastCash, cash, r.at)...)
		lastCash = cash
		held := heldByAccount(*b)
		closePositions(b, lastHeld, held, r.at)
		lastHeld = held
	}

	latest, err := c.latestAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		if err := c.appendStatementCloses(ctx, w, newest, latest, stated, at); err != nil {
			return nil, err
		}
	}

	batches := make([]canonical.SnapshotBatch, 0, len(byInstant))
	for _, t := range slices.Sorted(maps.Keys(byInstant)) {
		batches = append(batches, *byInstant[t])
	}

	tail, err := c.transactionDimensions(ctx, w, latest, secs)
	if err != nil {
		return nil, err
	}
	switch {
	case len(tail.Accounts)+len(tail.Instruments) == 0:
	case len(batches) == 0:
		batches = append(batches, tail)
	default:
		last := &batches[len(batches)-1]
		last.Accounts = append(last.Accounts, tail.Accounts...)
		last.Instruments = append(last.Instruments, tail.Instruments...)
	}
	return silver.NewSnapshotStream(batches), nil
}

// accountRun is a run that read the accounts, and whether it carries
// snapshots: whether it settled the holdings too, read in full or found
// nothing to read.
type accountRun struct {
	at        int64
	snapshots bool
}

// accountRuns are the runs in the window that read the accounts, oldest
// first.
func (c *Connection) accountRuns(ctx context.Context, w canonical.Window) ([]accountRun, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT d.snapshot_at, h.status IN ('fetched', 'absent', 'not_linked')
  FROM dump_runs d
  JOIN run_products a ON a.run_at = d.snapshot_at AND a.product = 'accounts'
  JOIN run_products h ON h.run_at = d.snapshot_at AND h.product = 'holdings'
 WHERE d.snapshot_at BETWEEN ? AND ?
   AND a.status = 'fetched'
 ORDER BY d.snapshot_at`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("plaid accountRuns: %w", err)
	}
	defer rows.Close()
	var out []accountRun
	for rows.Next() {
		var r accountRun
		if err := rows.Scan(&r.at, &r.snapshots); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// heldByAccount is each account's positions in b, with the instruments
// they name.
func heldByAccount(b canonical.SnapshotBatch) map[string]canonical.SnapshotBatch {
	out := map[string]canonical.SnapshotBatch{}
	for _, p := range b.Positions {
		h := out[p.AccountExternalID]
		h.Positions = append(h.Positions, p)
		for _, i := range b.Instruments {
			if p.InstrumentExternalID != nil && i.InstrumentExternalID == *p.InstrumentExternalID {
				h.Instruments = append(h.Instruments, i)
				break
			}
		}
		out[p.AccountExternalID] = h
	}
	return out
}

// closePositions replays at zero, at t, the positions of each account that
// held some at the last run and holds none now: it left the Item, or sold
// out. Gold's history carries an account's last positions until the source
// shows them gone.
func closePositions(b *canonical.SnapshotBatch, last, now map[string]canonical.SnapshotBatch, t int64) {
	for _, id := range slices.Sorted(maps.Keys(last)) {
		if _, holds := now[id]; holds {
			continue
		}
		marker := silver.ClosureMarkerBatch(last[id], t, canonical.AccountChange{})
		b.Positions = append(b.Positions, marker.Positions...)
		for _, i := range marker.Instruments {
			if !slices.ContainsFunc(b.Instruments, func(o canonical.InstrumentChange) bool {
				return o.InstrumentExternalID == i.InstrumentExternalID
			}) {
				b.Instruments = append(b.Instruments, i)
			}
		}
	}
}

// cashKey is the cash of one account in one currency.
type cashKey struct{ account, currency string }

// closedCash is a zero CURRENT balance at t, marked as a closure, for each
// cash key the last run stated and this run does not. Such a key is an
// account Plaid no longer lists, or cash an investment account invested or
// moved out. Plaid lists no line for either. Gold keeps a key's last figure
// until the source restates it.
func closedCash(last, now []cashKey, t int64) []canonical.CashBalanceChange {
	var out []canonical.CashBalanceChange
	for _, k := range last {
		if !slices.Contains(now, k) {
			out = append(out, balanceOf(t, k.account, k.currency, canonical.BalanceKindCurrent,
				canonical.NewDecimalFromInt(0), silver.ClosureMarkerPayload))
		}
	}
	return out
}

// statedBalance is a figure a cash, card or mortgage account stated: a
// cash or card balance of `kind`, or a mortgage's principal, in gold's
// sign.
type statedBalance struct {
	at       int64
	currency string
	kind     canonical.BalanceKind
	amount   canonical.Decimal
}

// statedOf is the figure a cash, card or mortgage account's listing in the
// run at t states, if it states one.
func statedOf(a account, t int64) (statedBalance, bool) {
	amount := silver.DecimalPtrOrNil(a.balance)
	available := silver.DecimalPtrOrNil(a.available)
	switch {
	case a.currency == "":
	case amount != nil && (a.kind == canonical.AccountKindCard || a.kind == canonical.AccountKindMortgage):
		return statedBalance{t, a.currency, canonical.BalanceKindCurrent, amount.Neg()}, true
	case amount != nil:
		return statedBalance{t, a.currency, canonical.BalanceKindCurrent, *amount}, true
	case a.kind == canonical.AccountKindCash && available != nil:
		// Plaid leaves the current balance empty at some institutions; the
		// available balance is then all there is.
		return statedBalance{t, a.currency, canonical.BalanceKindAvailable, *available}, true
	}
	return statedBalance{}, false
}

// noteBalances records in `stated` the figures a run that carries no
// snapshots states, so a later carry restates the newest of them.
func (c *Connection) noteBalances(ctx context.Context, t int64, stated map[string]statedBalance) error {
	accounts, err := c.accountsAt(ctx, t)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if investmentLedgerKind(a.kind) {
			continue
		}
		if s, ok := statedOf(a, t); ok {
			stated[a.id] = s
		}
	}
	return nil
}

// appendRun adds one run's accounts, balances, mortgages and holdings to b,
// and records in `stated` the figure each account stated. An account the
// run lists without a figure restates the last one it stated, if any. It
// returns the cash keys the run stated.
func (c *Connection) appendRun(ctx context.Context, b *canonical.SnapshotBatch, t int64,
	secs map[string]security, stated map[string]statedBalance) ([]cashKey, error) {
	accounts, err := c.accountsAt(ctx, t)
	if err != nil {
		return nil, err
	}
	var keys []cashKey
	byID := map[string]account{}
	for _, a := range accounts {
		byID[a.id] = a
		b.Accounts = append(b.Accounts, accountChange(a, t, t))
		if investmentLedgerKind(a.kind) {
			continue
		}
		s, ok := statedOf(a, t)
		if ok {
			stated[a.id] = s
		} else if s, ok = stated[a.id]; !ok {
			continue
		}
		if a.kind == canonical.AccountKindMortgage {
			appendMortgage(b, t, a, s)
			continue
		}
		payload := json.RawMessage(`{"basis":"roster"}`)
		if s.at != t {
			payload = json.RawMessage(fmt.Sprintf(`{"basis":"carried","stated_at":%d}`, s.at))
		}
		b.CashBalances = append(b.CashBalances, balanceOf(t, a.id, s.currency, s.kind, s.amount, payload))
		keys = append(keys, cashKey{a.id, s.currency})
	}
	holdings, err := c.holdingsAt(ctx, t)
	if err != nil {
		return nil, err
	}
	return append(keys, appendHoldings(b, t, byID, holdings, secs)...), nil
}

// balanceOf is one account's cash in one currency at t. Its payload says
// what the figure was read from.
func balanceOf(t int64, accountID, currency string, kind canonical.BalanceKind,
	amount canonical.Decimal, payload json.RawMessage) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{
		SnapshotAt:        t,
		AccountExternalID: accountID,
		Currency:          currency,
		BalanceKind:       kind,
		Amount:            amount,
		Payload:           payload,
	}
}

// appendMortgage adds a mortgage's outstanding principal as a negative
// (real estate, mortgage) position, keyed by the account: one loan, one
// position, one instrument.
func appendMortgage(b *canonical.SnapshotBatch, t int64, a account, s statedBalance) {
	id := a.id
	value := s.amount
	currency := s.currency
	payload := json.RawMessage(a.payload)
	if s.at != t {
		payload = silver.PayloadWith(a.payload, map[string]any{"basis": "carried", "stated_at": s.at})
	}
	b.Instruments = append(b.Instruments, canonical.InstrumentChange{
		InstrumentExternalID: id,
		AssetClass:           canonical.AssetClassRealEstate,
		Vehicle:              canonical.VehicleMortgage,
		Name:                 silver.StrPtrIfNonEmpty(displayName(a)),
		Currency:             &currency,
		FirstSeenAt:          t,
		LastSeenAt:           t,
	})
	b.Positions = append(b.Positions, canonical.PositionChange{
		SnapshotAt:           t,
		AccountExternalID:    id,
		PositionKey:          id,
		InstrumentExternalID: &id,
		AssetClass:           canonical.AssetClassRealEstate,
		Vehicle:              canonical.VehicleMortgage,
		Currency:             currency,
		MarketValue:          &value,
		Payload:              payload,
	})
}

// position is the running sum of the holdings of one instrument in one
// account. Two holdings land on one instrument when Plaid lists a security
// twice, or two securities share an identifier.
type position struct {
	accountID, key, currency             string
	sec                                  security
	quantity, value, costBasis, unvested *canonical.Decimal
	costKnown                            bool
	payloads                             []json.RawMessage
}

// appendHoldings adds the positions of the run's investment accounts, and
// one CURRENT balance per account and currency for the cash among them. A
// position holds the vested part of its holdings (holding.owned). It
// returns the cash keys it stated.
func appendHoldings(b *canonical.SnapshotBatch, t int64, accounts map[string]account,
	holdings []holding, secs map[string]security) []cashKey {
	positions := map[[2]string]*position{}
	var order [][2]string
	cash := map[cashKey]canonical.Decimal{}
	var cashOrder []cashKey
	for _, h := range holdings {
		a, ok := accounts[h.accountID]
		if !ok || !investmentLedgerKind(a.kind) {
			continue
		}
		sec := secs[h.securityID]
		if sec.id == "" {
			sec = security{id: h.securityID}
		}
		currency := cmp.Or(h.currency, sec.currency, a.currency)
		if currency == "" {
			continue
		}
		if isCash(sec, currency) {
			value := silver.DecimalPtrOrNil(h.value)
			if value == nil {
				value = silver.DecimalPtrOrNil(h.quantity)
			}
			if value == nil {
				continue
			}
			k := cashKey{a.id, currency}
			if _, seen := cash[k]; !seen {
				cashOrder = append(cashOrder, k)
			}
			cash[k] = cash[k].Add(*value)
			continue
		}
		k := [2]string{a.id, instrumentKey(sec)}
		p, ok := positions[k]
		if !ok {
			p = &position{accountID: a.id, key: k[1], currency: currency, sec: sec, costKnown: true}
			positions[k] = p
			order = append(order, k)
		}
		quantity, value, unvested := h.owned()
		p.quantity = addPtr(p.quantity, quantity)
		p.value = addPtr(p.value, value)
		p.unvested = addPtr(p.unvested, unvested)
		cost := silver.DecimalPtrOrNil(h.costBasis)
		p.costKnown = p.costKnown && cost != nil
		p.costBasis = addPtr(p.costBasis, cost)
		p.payloads = append(p.payloads, json.RawMessage(h.payload))
	}

	seenInstrument := map[string]bool{}
	for _, k := range order {
		p := positions[k]
		ac, veh, known := pairFor(p.sec)
		extra := map[string]any{}
		if !known {
			extra["source_type"] = p.sec.typ
		}
		if p.unvested != nil {
			extra["unvested_quantity"] = p.unvested.String()
		}
		payload := p.payloads[0]
		if len(p.payloads) > 1 {
			extra["holdings"] = p.payloads
			payload = json.RawMessage(`{}`)
		}
		var book *canonical.Decimal
		if p.costKnown {
			book = p.costBasis
		}
		key := p.key
		b.Positions = append(b.Positions, canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    p.accountID,
			PositionKey:          key,
			InstrumentExternalID: &key,
			AssetClass:           ac,
			Vehicle:              veh,
			Currency:             p.currency,
			Quantity:             p.quantity,
			MarketValue:          p.value,
			BookValue:            book,
			Payload:              silver.PayloadWith(string(payload), extra),
		})
		if !seenInstrument[key] {
			seenInstrument[key] = true
			b.Instruments = append(b.Instruments, instrumentChange(p.sec, key, t, t))
		}
	}
	for _, k := range cashOrder {
		b.CashBalances = append(b.CashBalances, balanceOf(t, k.account, k.currency,
			canonical.BalanceKindCurrent, cash[k], json.RawMessage(`{"basis":"holdings"}`)))
	}
	return cashOrder
}

// instrumentChange is the gold instrument of a security, seen over
// [first, last].
func instrumentChange(s security, key string, first, last int64) canonical.InstrumentChange {
	ac, veh, known := pairFor(s)
	extra := map[string]any{}
	if !known {
		extra["source_type"] = s.typ
	}
	payload := s.payload
	if payload == "" {
		payload = "{}"
	}
	return canonical.InstrumentChange{
		InstrumentExternalID: key,
		AssetClass:           ac,
		Vehicle:              veh,
		ISIN:                 silver.StrPtrIfNonEmpty(s.isin),
		CUSIP:                silver.StrPtrIfNonEmpty(s.cusip),
		Symbol:               silver.StrPtrIfNonEmpty(s.ticker),
		Name:                 silver.StrPtrIfNonEmpty(s.name),
		Currency:             silver.StrPtrIfNonEmpty(s.currency),
		FirstSeenAt:          first,
		LastSeenAt:           last,
		Payload:              silver.PayloadWith(payload, extra),
	}
}

// appendStatementCloses adds a card's CLOSING balance at the issue date of
// each statement in the window: the balance the statement stated, negated.
// Every run restates the last statement, so the newest run that states a
// figure for a statement stands.
//
// Only a statement issued before the newest run that carries snapshots
// counts. A later one, read by a run whose holdings failed, would be the
// source's latest instant on its own and hide every other balance. It
// counts once a later run carries snapshots. A card whose newest listing
// names no currency closes in the currency of its last stated balance.
func (c *Connection) appendStatementCloses(ctx context.Context, w canonical.Window, newest int64,
	latest map[string]account, stated map[string]statedBalance,
	at func(int64) *canonical.SnapshotBatch) error {
	rows, err := c.db.QueryContext(ctx, `
SELECT account_id, last_statement_issue_date, last_statement_balance FROM (
    SELECT account_id, last_statement_issue_date, last_statement_balance,
           ROW_NUMBER() OVER (PARTITION BY account_id, last_statement_issue_date
                              ORDER BY snapshot_at DESC) AS rn
      FROM liabilities
     WHERE kind = 'credit'
       AND last_statement_balance IS NOT NULL
       AND last_statement_issue_date BETWEEN ? AND ?
       AND last_statement_issue_date < ?
) WHERE rn = 1
 ORDER BY account_id, last_statement_issue_date`, w.Start, w.End, newest)
	if err != nil {
		return fmt.Errorf("plaid appendStatementCloses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			issued  int64
			balance sql.NullString
		)
		if err := rows.Scan(&id, &issued, &balance); err != nil {
			return err
		}
		a, ok := latest[id]
		amount := silver.DecimalPtrOrNil(balance)
		currency := cmp.Or(a.currency, stated[id].currency)
		if !ok || a.kind != canonical.AccountKindCard || amount == nil || currency == "" {
			continue
		}
		b := at(issued)
		b.Accounts = append(b.Accounts, accountChange(a, issued, issued))
		b.CashBalances = append(b.CashBalances, balanceOf(issued, id, currency,
			canonical.BalanceKindClosing, amount.Neg(), json.RawMessage(`{"basis":"statement_closing"}`)))
	}
	return rows.Err()
}

// seenSpan is the first and last date the transactions that name one id
// book at.
type seenSpan struct{ first, last int64 }

func (s seenSpan) widen(t int64) seenSpan {
	return seenSpan{min(s.first, t), max(s.last, t)}
}

// transactionDimensions is the batch of accounts and instruments the
// window's transactions name, each seen over the span of those
// transactions, in its latest description.
func (c *Connection) transactionDimensions(ctx context.Context, w canonical.Window,
	latest map[string]account, secs map[string]security) (canonical.SnapshotBatch, error) {
	var tail canonical.SnapshotBatch
	accounts := map[string]seenSpan{}
	instruments := map[string]seenSpan{}
	instrumentOf := map[string]security{}
	note := func(m map[string]seenSpan, id string, t int64) {
		if s, ok := m[id]; ok {
			m[id] = s.widen(t)
		} else {
			m[id] = seenSpan{t, t}
		}
	}

	bank, err := c.db.QueryContext(ctx, `
SELECT account_id, posted_at FROM transactions WHERE posted_at BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		return tail, fmt.Errorf("plaid transactionDimensions: %w", err)
	}
	for bank.Next() {
		var id string
		var t int64
		if err := bank.Scan(&id, &t); err != nil {
			bank.Close()
			return tail, err
		}
		if a, ok := latest[id]; ok && bankLedgerKind(a.kind) {
			note(accounts, id, t)
		}
	}
	bank.Close()
	if err := bank.Err(); err != nil {
		return tail, err
	}

	inv, err := c.db.QueryContext(ctx, `
SELECT account_id, COALESCE(security_id, ''), COALESCE(currency, ''), t
  FROM (SELECT *, `+investmentDate+` AS t FROM investment_transactions)
 WHERE t BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		return tail, fmt.Errorf("plaid transactionDimensions: %w", err)
	}
	for inv.Next() {
		var id, secID, currency string
		var t int64
		if err := inv.Scan(&id, &secID, &currency, &t); err != nil {
			inv.Close()
			return tail, err
		}
		a, ok := latest[id]
		if !ok || !investmentLedgerKind(a.kind) {
			continue
		}
		note(accounts, id, t)
		if sec, ok := secs[secID]; ok && !isCash(sec, cmp.Or(currency, a.currency)) {
			key := instrumentKey(sec)
			note(instruments, key, t)
			instrumentOf[key] = sec
		}
	}
	inv.Close()
	if err := inv.Err(); err != nil {
		return tail, err
	}

	for _, id := range slices.Sorted(maps.Keys(accounts)) {
		s := accounts[id]
		tail.Accounts = append(tail.Accounts, accountChange(latest[id], s.first, s.last))
	}
	for _, key := range slices.Sorted(maps.Keys(instruments)) {
		s := instruments[key]
		tail.Instruments = append(tail.Instruments,
			instrumentChange(instrumentOf[key], key, s.first, s.last))
	}
	return tail, nil
}

// accountChange is the gold account of a silver account row, seen over
// [first, last]. Its category is Plaid's type and subtype, followed by the
// institution's official name for the account, which can say more than
// the subtype.
func accountChange(a account, first, last int64) canonical.AccountChange {
	category := a.typ
	if a.subtype != "" {
		category += " / " + a.subtype
	}
	if a.officialName != "" {
		category += " · " + a.officialName
	}
	return canonical.AccountChange{
		AccountExternalID: a.id,
		AccountKind:       a.kind,
		DisplayName:       silver.StrPtrIfNonEmpty(displayName(a)),
		Nickname:          nickname(a),
		BaseCurrency:      silver.StrPtrIfNonEmpty(a.currency),
		AccountCategory:   silver.StrPtrIfNonEmpty(category),
		TaxWrapper:        wrapperFor(a.kind, a.subtype),
		ManagementStyle:   styleFor(a.kind),
		FirstSeenAt:       first,
		LastSeenAt:        last,
		Payload:           json.RawMessage(a.payload),
	}
}

// displayName is the institution's official name for the account, else
// the account's own name, followed by its mask.
func displayName(a account) string {
	name := cmp.Or(a.officialName, a.name)
	switch {
	case a.mask == "":
		return name
	case name == "":
		return a.mask
	}
	return name + " …" + a.mask
}

// nickname is the account's own name, set by the holder or the institution,
// where the institution states an official name besides it.
func nickname(a account) *string {
	if a.officialName == "" || a.name == a.officialName {
		return nil
	}
	return silver.StrPtrIfNonEmpty(a.name)
}

// bankLedgerKind reports whether the bank and card ledger of an account of
// this kind is projected. A loan's own ledger is not: its payment is booked
// once, on the account it was paid from.
func bankLedgerKind(k canonical.AccountKind) bool {
	return k == canonical.AccountKindCash || k == canonical.AccountKindCard
}

func investmentLedgerKind(k canonical.AccountKind) bool {
	return k == canonical.AccountKindBrokerage || k == canonical.AccountKindCrypto
}

// ---- reading silver ----------------------------------------------------------

const accountColumns = `account_id, COALESCE(name, ''), COALESCE(official_name, ''),
       COALESCE(mask, ''), COALESCE(type, ''), COALESCE(subtype, ''),
       COALESCE(currency, ''), balance_current, balance_available, payload`

func scanAccount(rows *sql.Rows) (account, bool, error) {
	var a account
	if err := rows.Scan(&a.id, &a.name, &a.officialName, &a.mask, &a.typ, &a.subtype,
		&a.currency, &a.balance, &a.available, &a.payload); err != nil {
		return a, false, err
	}
	kind, projected := accountKindFor(a.typ, a.subtype)
	a.kind = kind
	return a, projected, nil
}

// accountsAt are the projected accounts of the run at t.
func (c *Connection) accountsAt(ctx context.Context, t int64) ([]account, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+accountColumns+`
  FROM accounts WHERE snapshot_at = ? ORDER BY account_id`, t)
	if err != nil {
		return nil, fmt.Errorf("plaid accountsAt: %w", err)
	}
	defer rows.Close()
	var out []account
	for rows.Next() {
		a, projected, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		if projected {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// latestAccounts is every projected account in the description of the
// latest run that listed it.
func (c *Connection) latestAccounts(ctx context.Context) (map[string]account, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+accountColumns+`
  FROM accounts
 WHERE (snapshot_at, account_id) IN (SELECT MAX(snapshot_at), account_id
                                       FROM accounts GROUP BY account_id)`)
	if err != nil {
		return nil, fmt.Errorf("plaid latestAccounts: %w", err)
	}
	defer rows.Close()
	out := map[string]account{}
	for rows.Next() {
		a, projected, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		if projected {
			out[a.id] = a
		}
	}
	return out, rows.Err()
}

func (c *Connection) holdingsAt(ctx context.Context, t int64) ([]holding, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT account_id, security_id, quantity, institution_value, cost_basis,
       institution_price, vested_quantity, vested_value, COALESCE(currency, ''), payload
  FROM holdings WHERE snapshot_at = ? ORDER BY account_id, security_id, seq`, t)
	if err != nil {
		return nil, fmt.Errorf("plaid holdingsAt: %w", err)
	}
	defer rows.Close()
	var out []holding
	for rows.Next() {
		var h holding
		if err := rows.Scan(&h.accountID, &h.securityID, &h.quantity, &h.value,
			&h.costBasis, &h.price, &h.vestedQuantity, &h.vestedValue, &h.currency,
			&h.payload); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (c *Connection) readSecurities(ctx context.Context) (map[string]security, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT s.security_id, COALESCE(s.name, ''), COALESCE(s.ticker_symbol, ''),
       COALESCE(s.type, ''), COALESCE(s.currency, ''), COALESCE(s.cusip, ''),
       COALESCE(s.isin, ''), COALESCE(s.cfi_code, ''), s.payload,
       s.security_id IN (
           SELECT security_id FROM holdings
            WHERE CAST(institution_price AS REAL) NOT IN (0, 1)
           UNION ALL
           SELECT security_id FROM investment_transactions
            WHERE security_id IS NOT NULL
              AND LOWER(TRIM(type)) IN ('buy', 'sell')
              AND CAST(price AS REAL) NOT IN (0, 1))
  FROM securities s`)
	if err != nil {
		return nil, fmt.Errorf("plaid readSecurities: %w", err)
	}
	defer rows.Close()
	out := map[string]security{}
	for rows.Next() {
		var s security
		if err := rows.Scan(&s.id, &s.name, &s.ticker, &s.typ, &s.currency,
			&s.cusip, &s.isin, &s.cfi, &s.payload, &s.priced); err != nil {
			return nil, err
		}
		out[s.id] = s
	}
	return out, rows.Err()
}

// ---- small helpers -----------------------------------------------------------

// addPtr adds two optional values; the sum is absent only when both are.
func addPtr(a, b *canonical.Decimal) *canonical.Decimal {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	sum := a.Add(*b)
	return &sum
}
