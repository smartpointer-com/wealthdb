package relevate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
	"github.com/shopspring/decimal"
)

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	histTimes, err := c.historicalSnapshotTimes(ctx, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	// Union live + historical; the same snapshot_at may appear in
	// both (a Quartalsbericht date that also has a live dump_run),
	// in which case both readers populate the SAME batch.
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times)+len(histTimes))
	all := make([]int64, 0, len(times)+len(histTimes))
	for _, t := range times {
		if _, ok := byTime[t]; !ok {
			byTime[t] = &canonical.SnapshotBatch{}
			all = append(all, t)
		}
	}
	for _, t := range histTimes {
		if _, ok := byTime[t]; !ok {
			byTime[t] = &canonical.SnapshotBatch{}
			all = append(all, t)
		}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendInstruments(ctx, w, byTime); err != nil {
		return nil, err
	}
	// Cash + positions share a SQL pass: the per-fund market
	// values are derived from each account's securities_balance,
	// so we pull both side-by-side per account.
	if err := c.appendPositionsAndCash(ctx, w, byTime); err != nil {
		return nil, err
	}
	// Historical projection from the Quartalsbericht PDFs (silver
	// migration 0002). Silently no-op on silvers that haven't been
	// migrated yet.
	if err := c.appendHistoricalPositions(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendHistoricalCash(ctx, w, byTime); err != nil {
		return nil, err
	}

	// Sort the combined snapshot list for deterministic output.
	sortInt64sAsc(all)
	out := make([]canonical.SnapshotBatch, 0, len(all))
	for _, t := range all {
		out = append(out, *byTime[t])
	}
	return silver.NewSnapshotStream(out), nil
}

// sortInt64sAsc — small in-place ascending sort, same shape as the
// UBS historical helper. Snapshot lists are tiny (low dozens), so
// the O(n²) constant is fine and keeps the function self-contained.
func sortInt64sAsc(xs []int64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// snapshotTimesInWindow unions dump_runs with the content
// tables. dump_runs is the live-time signal but the content
// tables drive the batch dispatch — if a future Relevate dump
// ever lands content rows whose snapshot_at differs from the
// dump_run's (it doesn't today, but defensive), this keeps the
// byTime dispatch aware of them.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM dump_runs     WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM accounts      WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM positions     WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM cash_balances WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q,
		w.Start, w.End, w.Start, w.End,
		w.Start, w.End, w.Start, w.End)
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

func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// Silver may or may not carry a promoted `management_style`
	// column (the relevate loader was updated to stamp it
	// per-account after the gold-side adapter shipped). Read
	// the silver value when present; fall back to the adapter
	// default ('automated') otherwise. Relevate's robo-style
	// strategy menu maps to canonical 'automated': the holder
	// picks a strategy from a fixed list, then an algorithm
	// allocates and rebalances with no human in the loop.
	hasMgmt, err := silver.HasColumn(ctx, c.db, "accounts", "management_style")
	if err != nil {
		return err
	}
	mgmtCol := "NULL"
	if hasMgmt {
		mgmtCol = "management_style"
	}
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, currency_code,
       COALESCE(name, ''),
       COALESCE(product_name, ''),
       COALESCE(%s, ''),
       payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, mgmtCol)
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	wrapper := canonical.TaxWrapperVestedBenefits
	for rows.Next() {
		var (
			snap                                       int64
			extID, currency, name, product, silverMgmt string
			payload                                    string
		)
		if err := rows.Scan(&snap, &extID, &currency, &name, &product, &silverMgmt, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		w := wrapper
		style := canonical.ManagementStyleAutomated
		if silverMgmt != "" {
			style = canonical.ManagementStyle(silverMgmt)
		}
		ccy := currency
		change := canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			BaseCurrency:      &ccy,
			TaxWrapper:        &w,
			ManagementStyle:   &style,
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		// Relevate's per-account `name` is the foundation-
		// assigned label (typically the strategy name or a
		// customer-set nickname); use it as DisplayName.
		// product_name ("PensFree" / "Independent") goes to
		// AccountCategory so the strategy family is queryable
		// without parsing the payload.
		if name != "" {
			change.DisplayName = &name
		}
		if product != "" {
			change.AccountCategory = &product
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

func (c *Connection) appendInstruments(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// Relevate's instruments table has no snapshot_at; the most-
	// recent observation is what we have. Emit each instrument
	// against every snapshot in the window so the gold per-column
	// upsert sees consistent data — the loader-side LEAST/GREATEST
	// on first_seen_at / last_seen_at handles dedup correctly.
	const q = `
SELECT instrument_external_id,
       COALESCE(isin, ''),
       COALESCE(name, ''),
       COALESCE(asset_class, ''),
       first_seen_at, last_seen_at, payload
  FROM instruments`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendInstruments: %w", err)
	}
	defer rows.Close()
	ccy := "CHF"
	var instruments []canonical.InstrumentChange
	for rows.Next() {
		var (
			extID, isin, name, rawClass string
			firstSeen, lastSeen         int64
			payload                     string
		)
		if err := rows.Scan(&extID, &isin, &name, &rawClass, &firstSeen, &lastSeen, &payload); err != nil {
			return err
		}
		change := canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           assetClassFor(rawClass),
			Currency:             &ccy,
			FirstSeenAt:          firstSeen,
			LastSeenAt:           lastSeen,
			Payload:              json.RawMessage(payload),
		}
		if isin != "" {
			change.ISIN = &isin
			// Relevate's `instrument_external_id` is a small
			// internal numeric ID; we surface the ISIN as the
			// Symbol too so resolve-symbols and the
			// instruments display column have something
			// recognisable to key on.
			change.Symbol = &isin
		}
		if name != "" {
			change.Name = &name
		}
		instruments = append(instruments, change)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Emit once per snapshot batch.
	for _, batch := range byTime {
		batch.Instruments = append(batch.Instruments, instruments...)
	}
	return nil
}

// appendPositionsAndCash emits the per-account cash_balance row
// (liquid cash awaiting investment, from silver's `cash` kind)
// and derives per-fund positions by multiplying each fund's
// allocation by the account's securities_balance.
//
// See package doc for why derivation: silver carries target
// allocations, not held quantities. The aggregate
// `securities_balance` from silver equals the sum of the
// derived per-fund market values exactly.
func (c *Connection) appendPositionsAndCash(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// Pull each account's `cash` (liquid) and `securities`
	// (aggregate fund market value) balances in one pivot.
	const balQ = `
SELECT snapshot_at, account_external_id, currency,
       MAX(CASE WHEN balance_kind = 'cash'       THEN amount END) AS cash_amount,
       MAX(CASE WHEN balance_kind = 'securities' THEN amount END) AS securities_amount
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
 GROUP BY snapshot_at, account_external_id, currency`
	rows, err := c.db.QueryContext(ctx, balQ, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositionsAndCash (balances): %w", err)
	}
	defer rows.Close()
	type acctBal struct {
		currency   string
		securities decimal.Decimal
	}
	securitiesByAcct := make(map[[2]any]acctBal)
	for rows.Next() {
		var (
			snap                            int64
			acct, ccy                       string
			cashAmt, securitiesAmt          sql.NullFloat64
		)
		if err := rows.Scan(&snap, &acct, &ccy, &cashAmt, &securitiesAmt); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		if cashAmt.Valid {
			amt := decimal.NewFromFloat(cashAmt.Float64)
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: acct,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindCurrent,
				Amount:            canonical.Decimal(amt),
			})
		}
		if securitiesAmt.Valid {
			securitiesByAcct[[2]any{snap, acct}] = acctBal{
				currency:   ccy,
				securities: decimal.NewFromFloat(securitiesAmt.Float64),
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Now walk positions; per row, derive market value =
	// securities_balance * allocation. The instrument
	// identity uses ISIN when present (cross-bank join key),
	// falling back to Relevate's internal numeric id.
	const posQ = `
SELECT snapshot_at, account_external_id,
       instrument_external_id,
       COALESCE(isin, ''),
       COALESCE(asset_class, ''),
       allocation
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	prows, err := c.db.QueryContext(ctx, posQ, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositionsAndCash (positions): %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var (
			snap                            int64
			acct, extID, isin, rawClass     string
			allocation                      sql.NullFloat64
		)
		if err := prows.Scan(&snap, &acct, &extID, &isin, &rawClass, &allocation); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		key := extID
		if isin != "" {
			key = isin
		}
		instrumentKey := extID // join to instruments table on numeric id
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          key,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClassFor(rawClass),
		}
		if bal, ok := securitiesByAcct[[2]any{snap, acct}]; ok {
			change.Currency = bal.currency
			if allocation.Valid {
				alloc := decimal.NewFromFloat(allocation.Float64)
				mv := canonical.Decimal(bal.securities.Mul(alloc))
				change.MarketValue = &mv
			}
		}
		batch.Positions = append(batch.Positions, change)
	}
	return prows.Err()
}
