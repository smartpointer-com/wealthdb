package angellist

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Snapshots forward-fills the per-day portfolio from the silver's
// event-sourced position_snapshots. The collector already replays each
// position's timeline and computes its mark (silver migration 0005); this
// adapter does no valuation logic — for every event date it emits a COMPLETE
// snapshot (each position's latest snapshot on/before that date, keeping only
// the is_open ones), which is what gold's as-of query reads (the latest
// snapshot_at per source, all its positions). An exited position drops out
// exactly at its exit date.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	account, err := c.accountSlug(ctx)
	if err != nil {
		return nil, err
	}
	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batch, err := c.buildBatch(ctx, t, account)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	// The funding account's current uninvested cash (so account value =
	// positions + cash). Emitted as one CashBalanceChange dated at the last
	// funding movement.
	if cb, ok := c.fundingCashBalance(ctx, account); ok {
		batches = append(batches, canonical.SnapshotBatch{
			CashBalances: []canonical.CashBalanceChange{cb},
		})
	}
	// Instruments for EXITED investments — offerings derived from the funding
	// ledger that hold no current position (so buildBatch never emits them),
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
// transaction date span.
func (c *Connection) exitedInstruments(ctx context.Context) ([]canonical.InstrumentChange, error) {
	const q = `
SELECT o.position_external_id, COALESCE(o.company_name, ''), COALESCE(o.kind, ''),
       MIN(ft.occurred_at), MAX(ft.occurred_at)
  FROM offerings o
  JOIN funding_transactions ft ON ft.position_external_id = o.position_external_id
 WHERE o.position_external_id NOT IN (SELECT position_external_id FROM position_snapshots)
 GROUP BY o.position_external_id, o.company_name, o.kind
 ORDER BY o.position_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("exitedInstruments: %w", err)
	}
	defer rows.Close()
	var out []canonical.InstrumentChange
	for rows.Next() {
		var (
			pid, company, kind string
			first, last        sql.NullInt64
		)
		if err := rows.Scan(&pid, &company, &kind, &first, &last); err != nil {
			return nil, err
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
		out = append(out, inst)
	}
	return out, rows.Err()
}

// fundingCashBalance is the funding account's current uninvested cash as a
// CashBalanceChange, dated at the latest funding movement (the balance is the
// running total after it). ok=false when there is no funding ledger (older
// silver) or no account.
func (c *Connection) fundingCashBalance(ctx context.Context, account string) (canonical.CashBalanceChange, bool) {
	if account == "" {
		return canonical.CashBalanceChange{}, false
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
	if err := c.db.QueryRowContext(ctx, q).Scan(&balMinor, &ccy, &asOf); err != nil {
		return canonical.CashBalanceChange{}, false
	}
	if !balMinor.Valid || !asOf.Valid {
		return canonical.CashBalanceChange{}, false
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
		Amount:            canonical.Decimal(decimal.New(balMinor.Int64, -2)),
	}, true
}

// snapshotTimesInWindow are the position event dates in the window — the
// distinct as_of_date of position_snapshots (the download time in dump_runs
// is provenance, not a holding event, so it is excluded).
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT as_of_date FROM position_snapshots
 WHERE as_of_date BETWEEN ? AND ?
 ORDER BY as_of_date`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
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

// buildBatch materialises the full portfolio as of event date t: each
// position's latest snapshot on/before t, keeping only is_open=1 (an exited
// position's latest row is is_open=0, so it is dropped), plus the single
// invest account and one instrument per held position (the SPV stake — gold's
// position FK is satisfied and the account/instrument seen-range merges
// across batches).
func (c *Connection) buildBatch(ctx context.Context, t int64, account string) (canonical.SnapshotBatch, error) {
	var batch canonical.SnapshotBatch
	if account == "" {
		return batch, nil
	}
	const q = `
SELECT ps.position_external_id,
       COALESCE(ps.currency, o.currency, 'USD'),
       ps.market_value_minor, ps.contributed_minor,
       COALESCE(o.kind, ''), COALESCE(o.company_name, ''),
       o.investment_date, COALESCE(o.payload, '')
  FROM position_snapshots ps
  JOIN offerings o ON o.position_external_id = ps.position_external_id
 WHERE ps.is_open = 1
   AND ps.as_of_date = (SELECT MAX(as_of_date) FROM position_snapshots s2
                         WHERE s2.position_external_id = ps.position_external_id
                           AND s2.as_of_date <= ?)
 ORDER BY ps.position_external_id`
	rows, err := c.db.QueryContext(ctx, q, t)
	if err != nil {
		return batch, fmt.Errorf("buildBatch: %w", err)
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var (
			pid, currency, kind, company, payl string
			marketMinor, contribMinor, invDate sql.NullInt64
		)
		if err := rows.Scan(&pid, &currency, &marketMinor, &contribMinor,
			&kind, &company, &invDate, &payl); err != nil {
			return batch, err
		}
		any = true
		instKey := pid
		acNew, vehicle := taxonomyForKind(kind)
		change := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    account,
			PositionKey:          pid,
			InstrumentExternalID: &instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             currency,
			MarketValue:          minorPtr(marketMinor),
			BookValue:            minorPtr(contribMinor),
			AcquisitionDate:      acqDate(invDate),
		}
		batch.Positions = append(batch.Positions, change)

		inst := canonical.InstrumentChange{
			InstrumentExternalID: instKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			FirstSeenAt:          t,
			LastSeenAt:           t,
		}
		if company != "" {
			inst.Name = &company
		}
		if payl != "" {
			inst.Payload = json.RawMessage(payl)
		}
		batch.Instruments = append(batch.Instruments, inst)
	}
	if err := rows.Err(); err != nil {
		return batch, err
	}
	if !any {
		return batch, nil // nothing held at t
	}

	// One account for the whole AngelList LP book. It custodies the
	// holder's LP interests (not a brokerage), and the holder self-directs
	// which SPVs / funds to back — we do NOT model the GP's management
	// inside each vehicle. Mirrors the carta / equityzen private-market
	// accounts; config overrides win on overlap.
	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	cat := "AngelList LP"
	usd := "USD"
	batch.Accounts = append(batch.Accounts, canonical.AccountChange{
		AccountExternalID: account,
		AccountKind:       canonical.AccountKindCustody,
		BaseCurrency:      &usd,
		AccountCategory:   &cat,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       t,
		LastSeenAt:        t,
	})
	return batch, nil
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

// acqDate converts a nullable unix-seconds timestamp to a UTC-midnight
// calendar date (gold stores AcquisitionDate as DATE).
func acqDate(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}
