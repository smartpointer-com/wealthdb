package viac

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendInstruments(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendCashBalances(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		out = append(out, *byTime[t])
	}
	return silver.NewSnapshotStream(out), nil
}

// snapshotTimesInWindow unions dump_runs with the snapshot-typed
// content tables. Defensive against the (currently hypothetical)
// case where a content row has a snapshot_at that doesn't match
// any dump_run.
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
	// silver may or may not carry a promoted `management_style`
	// column (forward-compat with a future viac loader
	// update). When present, the silver value wins; otherwise
	// the default is `automated` — every observed VIAC product
	// is robo-managed.
	hasMgmt, err := silver.HasColumn(ctx, c.db, "accounts", "management_style")
	if err != nil {
		return err
	}
	mgmtCol := "NULL"
	if hasMgmt {
		mgmtCol = "management_style"
	}
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, product_code,
       COALESCE(name, ''), COALESCE(state, ''),
       COALESCE(currency_code, 'CHF'),
       COALESCE(%s, ''),
       payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, mgmtCol)
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                              int64
			extID, product, name, state, currency, silverMgmt string
			payload                                           string
		)
		if err := rows.Scan(&snap, &extID, &product, &name, &state, &currency, &silverMgmt, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		wrapper := taxWrapperFor(product)
		style := canonical.ManagementStyleAutomated
		if silverMgmt != "" {
			style = canonical.ManagementStyle(silverMgmt)
		}
		ccy := currency
		change := canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			BaseCurrency:      &ccy,
			TaxWrapper:        &wrapper,
			ManagementStyle:   &style,
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		if name != "" {
			change.DisplayName = &name
		}
		// product_code surfaced verbatim in account_category for
		// forensic queries (P3A / PVB / INV without needing to
		// re-derive from tax_wrapper).
		if cat := productCategory(product); cat != "" {
			change.AccountCategory = &cat
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// productCategory returns the short human label silver's
// product_code encodes (P3A / PVB / INV). Kept here rather than
// in classmap.go because it's purely a display string for
// AccountCategory, not part of the canonical taxonomy.
func productCategory(productCode string) string {
	switch productCode {
	case "3":
		return "P3A"
	case "2":
		return "PVB"
	case "1":
		return "INV"
	}
	return ""
}

func (c *Connection) appendInstruments(_ context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// silver.instruments has no snapshot_at; emit the latest-
	// known row per ISIN against every snapshot in the window.
	// gold's per-column upsert plus LEAST/GREATEST on
	// first_seen_at / last_seen_at handle dedup.
	const q = `
SELECT instrument_external_id, COALESCE(isin, ''),
       COALESCE(name, ''), COALESCE(currency_code, ''),
       COALESCE(asset_class, ''),
       first_seen_at, last_seen_at, payload
  FROM instruments`
	rows, err := c.db.QueryContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("appendInstruments: %w", err)
	}
	defer rows.Close()
	var instruments []canonical.InstrumentChange
	for rows.Next() {
		var (
			extID, isin, name, currency, rawClass string
			firstSeen, lastSeen                   int64
			payload                               string
		)
		if err := rows.Scan(&extID, &isin, &name, &currency, &rawClass, &firstSeen, &lastSeen, &payload); err != nil {
			return err
		}
		change := canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           assetClassFor(rawClass),
			FirstSeenAt:          firstSeen,
			LastSeenAt:           lastSeen,
			Payload:              json.RawMessage(payload),
		}
		if isin != "" {
			change.ISIN = &isin
			// VIAC's instrument_external_id is the ISIN; expose
			// it as the Symbol too so resolve-symbols / the
			// instruments display column have something
			// recognisable.
			change.Symbol = &isin
		}
		if name != "" {
			change.Name = &name
		}
		if currency != "" {
			change.Currency = &currency
		}
		instruments = append(instruments, change)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, batch := range byTime {
		batch.Instruments = append(batch.Instruments, instruments...)
	}
	return nil
}

func (c *Connection) appendPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// silver column semantics (viac schema v2):
	//   quantity           = number of fund units held
	//   market_value_chf   = CHF market value (= quantity * asset_price)
	//   acquisition_price  = per-unit cost basis in CHF
	//   asset_price        = per-unit current price in CHF
	//   ratio              = fraction of account NAV (0..1), unused
	// CAST decimals to VARCHAR to dodge SQLite REAL → float64
	// precision loss before parsing through shopspring/decimal.
	const q = `
SELECT snapshot_at, account_external_id, instrument_external_id,
       COALESCE(asset_class, ''),
       CAST(quantity          AS VARCHAR),
       CAST(market_value_chf  AS VARCHAR),
       CAST(acquisition_price AS VARCHAR),
       payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                                       int64
			acct, isin, rawClass                       string
			qtyStr, marketValueStr, acquisitionPxStr   sql.NullString
			payload                                    string
		)
		if err := rows.Scan(&snap, &acct, &isin, &rawClass,
			&qtyStr, &marketValueStr, &acquisitionPxStr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		instrumentKey := isin
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          isin,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClassFor(rawClass),
			Currency:             "CHF",
			Quantity:             silver.DecimalPtrOrNil(qtyStr),
			MarketValue:          silver.DecimalPtrOrNil(marketValueStr),
			Payload:              json.RawMessage(payload),
		}
		// BookValue (cost basis in CHF) = quantity * acquisition_price.
		if change.Quantity != nil {
			if acq := silver.DecimalPtrOrNil(acquisitionPxStr); acq != nil {
				bv := change.Quantity.Mul(*acq)
				change.BookValue = &bv
			}
		}
		batch.Positions = append(batch.Positions, change)
	}
	return rows.Err()
}

func (c *Connection) appendCashBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, currency,
       CAST(amount AS VARCHAR)
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
   AND balance_kind = 'cash'`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCashBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap          int64
			acct, ccy     string
			amountStr     sql.NullString
		)
		if err := rows.Scan(&snap, &acct, &ccy, &amountStr); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		amt, err := silver.DecimalOrZero(amountStr)
		if err != nil {
			return fmt.Errorf("appendCashBalances amount parse (acct=%s): %w", acct, err)
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: acct,
			Currency:          ccy,
			BalanceKind:       canonical.BalanceKindCurrent,
			Amount:            amt,
		})
	}
	return rows.Err()
}
