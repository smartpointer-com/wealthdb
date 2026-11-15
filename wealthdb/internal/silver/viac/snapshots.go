package viac

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := silver.NewBatches(times)

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendInstruments(ctx, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendCashBalances(ctx, w, byTime); err != nil {
		return nil, err
	}

	return byTime.Stream(), nil
}

// snapshotTimesInWindow unions dump_runs with the snapshot-typed
// content tables. A content row whose snapshot_at matches no
// dump_run still gets a batch.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	return silver.SnapshotTimesOf(ctx, c.db, w, nil,
		"dump_runs", "accounts", "positions", "cash_balances")
}

// appendAccounts emits one AccountChange per account snapshot, the rate
// its strategy states under the stated-rate keys (strategyRateKeys).
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// A silver may carry a promoted `management_style` column.
	// When present, the silver value wins; otherwise the default
	// is `automated` — every VIAC product is robo-managed.
	mgmtCol := silver.ColumnOr(c.schema.managementStyle, "management_style")
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, product_code,
       COALESCE(name, ''), COALESCE(state, ''),
       COALESCE(currency_code, 'CHF'),
       COALESCE(%s, ''),
       payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`, mgmtCol)
	if err := silver.ScanRows(ctx, c.db, "appendAccounts", q, func(rows *sql.Rows) error {
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
			return nil
		}
		wrapper := taxWrapperFor(product)
		style := canonical.ManagementStyleAutomated
		if silverMgmt != "" {
			style = canonical.ManagementStyle(silverMgmt)
		}
		ccy := currency
		accountPayload := json.RawMessage(payload)
		if keys := strategyRateKeys(accountPayload); keys != nil {
			stamped, err := silver.WithStatedRates(accountPayload, keys)
			if err != nil {
				return fmt.Errorf("viac account %s rate: %w", extID, err)
			}
			accountPayload = stamped
		}
		change := canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			BaseCurrency:      &ccy,
			TaxWrapper:        &wrapper,
			ManagementStyle:   &style,
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           accountPayload,
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
		return nil
	}, w.Start, w.End); err != nil {
		return err
	}
	return nil
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

func (c *Connection) appendInstruments(ctx context.Context, byTime map[int64]*canonical.SnapshotBatch) error {
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
	var instruments []canonical.InstrumentChange
	if err := silver.ScanRows(ctx, c.db, "appendInstruments", q, func(rows *sql.Rows) error {
		var (
			extID, isin, name, currency, rawClass string
			firstSeen, lastSeen                   int64
			payload                               string
		)
		if err := rows.Scan(&extID, &isin, &name, &currency, &rawClass, &firstSeen, &lastSeen, &payload); err != nil {
			return err
		}
		classNew, vehicle := taxonomyFor(rawClass, name)
		change := canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           classNew,
			Vehicle:              vehicle,
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
		return nil
	}); err != nil {
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
       COALESCE(asset_class, ''), COALESCE(name, ''),
       CAST(quantity          AS VARCHAR),
       CAST(market_value_chf  AS VARCHAR),
       CAST(acquisition_price AS VARCHAR),
       payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	if err := silver.ScanRows(ctx, c.db, "appendPositions", q, func(rows *sql.Rows) error {
		var (
			snap                                     int64
			acct, isin, rawClass, name               string
			qtyStr, marketValueStr, acquisitionPxStr *canonical.Decimal
			payload                                  string
		)
		if err := rows.Scan(&snap, &acct, &isin, &rawClass, &name,
			silver.Dec(&qtyStr, silver.DecText), silver.Dec(&marketValueStr, silver.DecText), silver.Dec(&acquisitionPxStr, silver.DecText), &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}
		instrumentKey := isin
		classNew, vehicle := taxonomyFor(rawClass, name)
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          isin,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           classNew,
			Vehicle:              vehicle,
			Currency:             "CHF",
			Quantity:             qtyStr,
			MarketValue:          marketValueStr,
			Payload:              json.RawMessage(payload),
		}
		// BookValue (cost basis in CHF) = quantity * acquisition_price.
		if change.Quantity != nil {
			if acq := acquisitionPxStr; acq != nil {
				bv := change.Quantity.Mul(*acq)
				change.SetBookValue(&bv, acquisitionBasis)
			}
		}
		batch.Positions = append(batch.Positions, change)
		return nil
	}, w.Start, w.End); err != nil {
		return err
	}
	return nil
}

func (c *Connection) appendCashBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, currency,
       CAST(amount AS VARCHAR)
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
   AND balance_kind = 'cash'`
	if err := silver.ScanRows(ctx, c.db, "appendCashBalances", q, func(rows *sql.Rows) error {
		var (
			snap      int64
			acct, ccy string
			amountStr sql.NullString
		)
		if err := rows.Scan(&snap, &acct, &ccy, &amountStr); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
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
		return nil
	}, w.Start, w.End); err != nil {
		return err
	}
	return nil
}
