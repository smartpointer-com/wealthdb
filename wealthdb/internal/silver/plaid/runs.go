package plaid

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log"
	"maps"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

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
//     only, with the tax lots Plaid states for it, and a CURRENT balance
//     per currency for the cash among its holdings. Plaid's balance of
//     such an account is its total value, so it is never read as cash.
//     An account with no holding and a balance carries that balance as
//     one unitemized position (appendUnitemized).
//
// Gold ends a balance a run leaves out once that run restates the
// balances it was seen with (DESIGN.md §10.7), so every run restates
// every account. Where Plaid states no balance for an account in one run,
// the last figure it stated is carried into the run, marked "carried":
// unknown is not zero.
//
// A run whose holdings were not read in full (failed, not ready) adds
// nothing at its own instant. It cannot tell a holding sold from one it
// did not read, so it closes nothing. Its balances still count as the
// last figures stated. When an account holds no position after a run
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
	secs, err := c.securities(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := c.accountRuns(ctx, w)
	if err != nil {
		return nil, err
	}

	byInstant := silver.Batches{}

	var (
		lastCash    []cashKey
		lastHeld    map[string]canonical.SnapshotBatch
		newest      int64
		snapshotted bool
		unsummed    int
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
		b := byInstant.At(r.at)
		cash, err := c.appendRun(ctx, b, r.at, secs, stated, &unsummed)
		if err != nil {
			return nil, err
		}
		b.CashBalances = append(b.CashBalances, closedCash(lastCash, cash, r.at)...)
		lastCash = cash
		held := heldByAccount(*b)
		closePositions(b, lastHeld, held, r.at)
		lastHeld = held
	}
	if unsummed > 0 {
		log.Printf("plaid adapter: %d position snapshot(s) carry no lots: their tax lots "+
			"do not add up to the holding's quantity and cost", unsummed)
	}

	latest, err := c.accounts(ctx)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		if err := c.appendStatementCloses(ctx, w, newest, latest, stated, byInstant.At); err != nil {
			return nil, err
		}
	}

	batches := byInstant.Sorted()

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
	return silver.Query(ctx, c.db, "plaid accountRuns", `
SELECT d.snapshot_at, h.status IN ('fetched', 'absent', 'not_linked')
  FROM dump_runs d
  JOIN run_products a ON a.run_at = d.snapshot_at AND a.product = 'accounts'
  JOIN run_products h ON h.run_at = d.snapshot_at AND h.product = 'holdings'
 WHERE d.snapshot_at BETWEEN ? AND ?
   AND a.status = 'fetched'
 ORDER BY d.snapshot_at`, func(rows *sql.Rows) (accountRun, error) {
		var r accountRun
		err := rows.Scan(&r.at, &r.snapshots)
		return r, err
	}, w.Start, w.End)
}

// heldByAccount is each account's positions in b, with the instruments
// they name.
func heldByAccount(b canonical.SnapshotBatch) map[string]canonical.SnapshotBatch {
	instruments := make(map[string]canonical.InstrumentChange, len(b.Instruments))
	for _, i := range b.Instruments {
		if _, seen := instruments[i.InstrumentExternalID]; !seen {
			instruments[i.InstrumentExternalID] = i
		}
	}
	out := map[string]canonical.SnapshotBatch{}
	for _, p := range b.Positions {
		h := out[p.AccountExternalID]
		h.Positions = append(h.Positions, p)
		if p.InstrumentExternalID != nil {
			if i, ok := instruments[*p.InstrumentExternalID]; ok {
				h.Instruments = append(h.Instruments, i)
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
			out = append(out, silver.CashBalance(t, k.account, k.currency, canonical.BalanceKindCurrent,
				canonical.NewDecimalFromInt(0), string(silver.ClosureMarkerPayload)))
		}
	}
	return out
}

// appendRun adds one run's accounts, balances, mortgages and holdings to b,
// and records in `stated` the figure each account stated. b covers at t
// each account the run states in full: an investment account by its
// holdings, any other by its figure. An account listed with no figure
// stated so far says nothing of its cash, and is not covered. An account the
// run lists without a figure restates the last one it stated, if any. It
// returns the cash keys the run stated, and counts in unsummed the
// positions whose tax lots do not add up to them (appendHoldings).
func (c *Connection) appendRun(ctx context.Context, b *canonical.SnapshotBatch, t int64,
	secs map[string]security, stated map[string]statedBalance, unsummed *int) ([]cashKey, error) {
	accounts, err := c.accountsAt(ctx, t)
	if err != nil {
		return nil, err
	}
	var keys []cashKey
	byID := map[string]account{}
	for _, a := range accounts {
		byID[a.id] = a
		b.Accounts = append(b.Accounts, accountChange(a, t, t))
		cover := canonical.CoverageChange{SnapshotAt: t, AccountExternalID: a.id}
		if investmentLedgerKind(a.kind) {
			b.Covers = append(b.Covers, cover)
			continue
		}
		s, ok := statedOf(a, t)
		if ok {
			stated[a.id] = s
		} else if s, ok = stated[a.id]; !ok {
			continue
		}
		b.Covers = append(b.Covers, cover)
		if a.kind == canonical.AccountKindMortgage {
			appendMortgage(b, t, a, s)
			continue
		}
		payload := `{"basis":"roster"}`
		if s.at != t {
			payload = fmt.Sprintf(`{"basis":"carried","stated_at":%d}`, s.at)
		}
		b.CashBalances = append(b.CashBalances, silver.CashBalance(t, a.id, s.currency, s.kind, s.amount, payload))
		keys = append(keys, cashKey{a.id, s.currency})
	}
	holdings, err := c.holdingsAt(ctx, t)
	if err != nil {
		return nil, err
	}
	keys = append(keys, appendHoldings(b, t, byID, holdings, secs, unsummed)...)
	appendUnitemized(b, t, accounts, holdings, stated)
	return keys, nil
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

	if err := silver.ScanRows(ctx, c.db, "plaid transactionDimensions", `
SELECT account_id, posted_at FROM transactions WHERE posted_at BETWEEN ? AND ?`, func(bank *sql.Rows) error {
		var id string
		var t int64
		if err := bank.Scan(&id, &t); err != nil {
			return err
		}
		if a, ok := latest[id]; ok && bankLedgerKind(a.kind) {
			note(accounts, id, t)
		}
		return nil
	}, w.Start, w.End); err != nil {
		return tail, err
	}

	if err := silver.ScanRows(ctx, c.db, "plaid transactionDimensions", `
SELECT account_id, COALESCE(security_id, ''), COALESCE(currency, ''), t
  FROM (SELECT *, `+investmentDate+` AS t FROM investment_transactions)
 WHERE t BETWEEN ? AND ?`, func(inv *sql.Rows) error {
		var id, secID, currency string
		var t int64
		if err := inv.Scan(&id, &secID, &currency, &t); err != nil {
			return err
		}
		a, ok := latest[id]
		if !ok || !investmentLedgerKind(a.kind) {
			return nil
		}
		note(accounts, id, t)
		if sec, ok := secs[secID]; ok && !isCash(sec, cmp.Or(currency, a.currency)) {
			key := instrumentKey(sec)
			note(instruments, key, t)
			instrumentOf[key] = sec
		}
		return nil
	}, w.Start, w.End); err != nil {
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
