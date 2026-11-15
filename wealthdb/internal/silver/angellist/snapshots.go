package angellist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots forward-fills the per-day portfolio from the silver's
// event-sourced position_snapshots (silver.Book). The collector already
// replays each position's timeline and computes its mark (silver
// migration 0005); this adapter does no valuation logic. Every event
// date gets a COMPLETE snapshot: each position's latest snapshot on or
// before that date, keeping only the is_open ones, which is what gold's
// as-of query reads (each account's latest snapshot, all its
// positions). An exited position drops out exactly at its exit date. A
// position the portal stopped listing drops out at the first dump that
// omitted it (removals): that dump time is a snapshot time too, where
// the roster is emitted without it and every other position keeps its
// own mark date (ValuedAt). When the LAST position leaves, that date
// gets the exit-day zero snapshot instead (silver.ClosureMarkerBatch).
// The K-1 period ends are snapshot times too: a book value steps down
// there (inKind).
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	k, dropped, err := c.readInKind(ctx)
	if err != nil {
		return nil, err
	}
	logDropped(dropped)
	removals, err := c.removals(ctx)
	if err != nil {
		return nil, err
	}
	account, err := c.accountSlug(ctx)
	if err != nil {
		return nil, err
	}
	states, err := c.states(ctx)
	if err != nil {
		return nil, err
	}
	batches, err := silver.Book{
		States:   states,
		Extra:    k.PeriodEnds(),
		Removals: removals,
		Account:  func(t int64) canonical.AccountChange { return custodyAccount(account, t) },
		Emit: func(t int64, held []silver.Holding) (canonical.SnapshotBatch, error) {
			return emit(t, account, k, held), nil
		},
		StampValuedAt: true,
		Closure:       true,
	}.Batches(w)
	if err != nil {
		return nil, err
	}
	k.LogClamped(kindName)
	// The funding account's current uninvested cash (so account value =
	// positions + cash). Emitted as one CashBalanceChange dated at the last
	// funding movement.
	cb, ok, err := c.fundingCashBalance(ctx, account)
	if err != nil {
		return nil, err
	}
	if ok {
		batches = append(batches, canonical.SnapshotBatch{
			CashBalances: []canonical.CashBalanceChange{cb},
		})
	}
	// Instruments for EXITED investments — offerings derived from the funding
	// ledger that hold no current position (so emit never emits them),
	// but whose contributions / distributions need an instrument to link to.
	insts, err := c.exitedInstruments(ctx)
	if err != nil {
		return nil, err
	}
	if len(insts) > 0 {
		batches = append(batches, canonical.SnapshotBatch{Instruments: insts})
	}
	return silver.NewSnapshotStream(batches), nil
}

// exitedInstruments emits an InstrumentChange for every offering with no
// position_snapshots — the exited / funding-only investments load.py derives
// from the funding ledger. They carry no current holding, but their
// contributions / distributions reference them; seen-range = the funding
// transaction date span. A silver whose ledger names no position has none.
func (c *Connection) exitedInstruments(ctx context.Context) ([]canonical.InstrumentChange, error) {
	if !c.schema.fundingLinks {
		return nil, nil
	}
	const q = `
SELECT o.position_external_id, COALESCE(o.company_name, ''), COALESCE(o.kind, ''),
       MIN(ft.occurred_at), MAX(ft.occurred_at)
  FROM offerings o
  JOIN funding_transactions ft ON ft.position_external_id = o.position_external_id
 WHERE o.position_external_id NOT IN (SELECT position_external_id FROM position_snapshots)
 GROUP BY o.position_external_id, o.company_name, o.kind
 ORDER BY o.position_external_id`
	return silver.Query(ctx, c.db, "exitedInstruments", q, func(rows *sql.Rows) (rec canonical.InstrumentChange, err error) {
		var (
			pid, company, kind string
			first, last        sql.NullInt64
		)
		if err := rows.Scan(&pid, &company, &kind, &first, &last); err != nil {
			return rec, err
		}
		acNew, vehicle := taxonomyForKind(kind)
		inst := canonical.InstrumentChange{
			InstrumentExternalID: pid,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			FirstSeenAt:          first.Int64,
			LastSeenAt:           last.Int64,
		}
		if company != "" {
			inst.Name = &company
		}
		return inst, nil
	})
}

// fundingCashBalance is the funding account's current uninvested cash as a
// CashBalanceChange, dated at the latest funding movement (the balance is the
// running total after it). ok=false when there is no funding ledger (a silver
// that predates it, or no row) or no account.
func (c *Connection) fundingCashBalance(ctx context.Context, account string) (canonical.CashBalanceChange, bool, error) {
	if account == "" {
		return canonical.CashBalanceChange{}, false, nil
	}
	if !c.schema.fundingAccounts || !c.schema.fundingTransactions {
		return canonical.CashBalanceChange{}, false, nil
	}
	var (
		balMinor sql.NullInt64
		ccy      sql.NullString
		asOf     sql.NullInt64
	)
	const q = `
SELECT fa.balance_minor, fa.currency,
       (SELECT MAX(occurred_at) FROM funding_transactions)
  FROM funding_accounts fa LIMIT 1`
	err := c.db.QueryRowContext(ctx, q).Scan(&balMinor, &ccy, &asOf)
	if errors.Is(err, sql.ErrNoRows) {
		return canonical.CashBalanceChange{}, false, nil
	}
	if err != nil {
		return canonical.CashBalanceChange{}, false, fmt.Errorf("angellist funding balance: %w", err)
	}
	if !balMinor.Valid || !asOf.Valid {
		return canonical.CashBalanceChange{}, false, nil
	}
	cur := "USD"
	if ccy.Valid && ccy.String != "" {
		cur = ccy.String
	}
	return canonical.CashBalanceChange{
		SnapshotAt:        asOf.Int64,
		AccountExternalID: account,
		Currency:          cur,
		BalanceKind:       canonical.BalanceKindCurrent,
		Amount:            *minorPtr(balMinor),
	}, true, nil
}

// removals are the open positions a complete dump no longer lists, each
// with the time of the first such dump that omitted it (silver.Removals).
// A dump stamps every position it lists (offerings.last_seen_at), and its
// portfolio summary declares how many investments the portfolio holds: a
// dump is complete when the positions known to be listed around its time
// (first_seen_at ≤ dump ≤ last_seen_at) number what its summary declares.
// A dump that captured no summary, or whose listing fell short of the
// count, says nothing about the roster. A position whose latest row
// already closes it (is_open = 0) leaves at that row's date and needs no
// removal.
func (c *Connection) removals(ctx context.Context) (silver.Removals, error) {
	const q = `
WITH complete AS (
    SELECT d.snapshot_at AS t
      FROM dump_runs d
      JOIN portfolio_summary s ON s.snapshot_at = d.snapshot_at
     WHERE s.total_investments_count IS NOT NULL
       AND s.total_investments_count = (
           SELECT COUNT(*) FROM offerings o
            WHERE o.first_seen_at <= d.snapshot_at AND d.snapshot_at <= o.last_seen_at)),
open AS (
    SELECT position_external_id AS key FROM position_snapshots ps
     WHERE is_open = 1
       AND as_of_date = (SELECT MAX(as_of_date) FROM position_snapshots s2
                          WHERE s2.position_external_id = ps.position_external_id))
SELECT o.position_external_id, (SELECT MIN(t) FROM complete WHERE t > o.last_seen_at)
  FROM offerings o
  JOIN open ON open.key = o.position_external_id
 WHERE o.last_seen_at IS NOT NULL
   AND EXISTS (SELECT 1 FROM complete WHERE t > o.last_seen_at)`
	return silver.QueryRemovals(ctx, c.db, "angellist", q)
}

// accountSlug is the single invest account's external id, from dump_runs
// (fall back to portfolio_summary).
func (c *Connection) accountSlug(ctx context.Context) (string, error) {
	const q = `
SELECT COALESCE(NULLIF(slug, ''), '') FROM (
    SELECT invest_account_slug AS slug FROM dump_runs
     WHERE invest_account_slug IS NOT NULL AND invest_account_slug <> ''
    UNION ALL
    SELECT invest_account_slug FROM portfolio_summary
     WHERE invest_account_slug IS NOT NULL AND invest_account_slug <> ''
) LIMIT 1`
	var slug sql.NullString
	err := c.db.QueryRowContext(ctx, q).Scan(&slug)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("accountSlug: %w", err)
	}
	return slug.String, nil
}

// state is one position_snapshots row with what the position's batch
// reads from it (silver.Holding.Row).
type state struct {
	currency, kind, company, payload string
	market, contributed, taxBasis    sql.NullInt64
	invested                         sql.NullInt64
}

// states are every position's snapshots, oldest first (silver.Book). A
// snapshot is held while it is is_open=1 and its offering is known (an
// exited position's latest row is is_open=0). The contributed capital
// on a snapshot is the latest the portal states on or before it, and so
// is the K-1's tax-basis figure: no other snapshot of the position lies
// between it and any snapshot time it stands for.
func (c *Connection) states(ctx context.Context) ([]silver.Holding, error) {
	const q = `
SELECT ps.position_external_id, ps.as_of_date,
       COALESCE(ps.is_open = 1 AND o.position_external_id IS NOT NULL, 0),
       COALESCE(ps.currency, o.currency, 'USD'),
       ps.market_value_minor,
       (SELECT contributed_minor FROM position_snapshots p
         WHERE p.position_external_id = ps.position_external_id
           AND p.event_type IN ('investment', 'valuation')
           AND p.as_of_date <= ps.as_of_date
         ORDER BY p.as_of_date DESC LIMIT 1),
       (SELECT contributed_minor FROM position_snapshots k
         WHERE k.position_external_id = ps.position_external_id
           AND k.event_type = 'statement' AND k.valuation_basis = 'tax_basis'
           AND k.as_of_date <= ps.as_of_date
         ORDER BY k.as_of_date DESC LIMIT 1),
       COALESCE(o.kind, ''), COALESCE(o.company_name, ''),
       o.investment_date, COALESCE(o.payload, '')
  FROM position_snapshots ps
  LEFT JOIN offerings o ON o.position_external_id = ps.position_external_id
 ORDER BY ps.position_external_id, ps.as_of_date`
	return silver.Query(ctx, c.db, "states", q, func(rows *sql.Rows) (silver.Holding, error) {
		var (
			h silver.Holding
			s state
		)
		err := rows.Scan(&h.Key, &h.At, &h.Open, &s.currency, &s.market, &s.contributed, &s.taxBasis,
			&s.kind, &s.company, &s.invested, &s.payload)
		h.Row = s
		return h, err
	})
}

// emit is the batch at t of the positions held then: one position and
// one instrument (the SPV stake) per held position, so gold's position
// FK is satisfied and the instrument's seen-range merges across batches.
// The book carries the single invest account.
//
// The value is the latest event's mark, whichever source states it, and
// the position is valued at that event's date. The book value is the
// capital contributed as the portal states it, on its latest event
// on/before t, less the basis K-1s say left in kind by then (inKind). A K-1
// states cumulative contributions too, but on the partnership's tax basis,
// which can differ from the portal's figure; it rides in the payload as
// tax_basis_contributed, so the book value does not move between the two
// as the latest event changes.
func emit(t int64, account string, k *silver.InKind, held []silver.Holding) canonical.SnapshotBatch {
	var batch canonical.SnapshotBatch
	if account == "" {
		return batch
	}
	for _, h := range held {
		s := h.Row.(state)
		instKey := h.Key
		acNew, vehicle := taxonomyForKind(s.kind)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    account,
			PositionKey:          h.Key,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             s.currency,
			MarketValue:          minorPtr(s.market),
			AcquisitionDate:      silver.DatePtrFromNullUnix(s.invested),
		}
		book, basis, extra := k.BookValue(h.Key, t, minorPtr(s.contributed), paidInBasis)
		change.SetBookValue(book, basis)
		if tax := minorPtr(s.taxBasis); tax != nil {
			if extra == nil {
				extra = map[string]any{}
			}
			extra["tax_basis_contributed"] = tax.String()
		}
		if extra != nil {
			change.Payload = silver.PayloadWith("{}", extra)
		}
		batch.Positions = append(batch.Positions, change)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			FirstSeenAt:          t,
			LastSeenAt:           t,
		}
		if s.company != "" {
			company := s.company
			inst.Name = &company
		}
		if s.payload != "" {
			inst.Payload = json.RawMessage(s.payload)
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	return batch
}

// custodyAccount is the one account for the whole AngelList LP book, as of
// t. It custodies the holder's LP interests (not a brokerage), and the
// holder self-directs which SPVs / funds to back — the GP's management
// inside each vehicle is not modelled. Mirrors the carta / equityzen
// private-market accounts; config overrides win on overlap.
func custodyAccount(account string, t int64) canonical.AccountChange {
	a := silver.CustodyAccount(account, t)
	cat := "AngelList LP"
	a.AccountCategory = &cat
	return a
}

// ---- helpers ------------------------------------------------------

// minorPtr scales a nullable minor-unit (cents) integer to a canonical
// Decimal pointer (×10^-2). Returns nil for SQL NULL.
func minorPtr(n sql.NullInt64) *canonical.Decimal {
	if !n.Valid {
		return nil
	}
	d := decimal.New(n.Int64, -2)
	return &d
}
