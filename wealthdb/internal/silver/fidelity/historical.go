package fidelity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// Historical-snapshot reader for the fidelity-web silver
// migration-0004 table `historical_position_snapshots`. Parsed
// from quarterly + year-end 529 statement PDFs by
// fidelity-web/pdf_parsers.py. Statement archives exist in this
// silver only for account groups that expose statements (see fidelity-web/DESIGN.md §4.5), so the historical path
// covers those alone.
//
// The table's primary key is (as_of_date, account_external_id,
// description). The cross-walked `instrument_key` column points
// at the same identity space as the live `positions` table — no
// bridge map needed (unlike schwab-web where the api hashValue
// has to translate to the web suffix). When the cross-walk on
// the silver side missed (instrument_key NULL), we synthesise a
// stable identity from the description so gold still has an
// instrument to hang the row on.

// hasHistoricalTable reports whether the fidelity-web silver
// carries the migration-0004 historical_position_snapshots
// table. Pre-v4 silvers won't have it; the adapter must still
// load, falling back to the live-only stream.
func (c *Connection) hasHistoricalTable(ctx context.Context) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
 WHERE type = 'table'
   AND name = 'historical_position_snapshots'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("fidelity hasHistoricalTable: %w", err)
	}
	return n == 1, nil
}

// historicalSnapshotTimes returns the distinct as_of_date values
// in `historical_position_snapshots` that fall inside the window.
// Called by snapshotTimesInWindow so the byTime dispatch in
// Snapshots() carries a batch slot per historical date too.
func (c *Connection) historicalSnapshotTimes(ctx context.Context, w canonical.Window) ([]int64, error) {
	ok, err := c.hasHistoricalTable(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	const q = `
SELECT DISTINCT as_of_date
  FROM historical_position_snapshots
 WHERE as_of_date BETWEEN ? AND ?
 ORDER BY as_of_date`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("historicalSnapshotTimes: %w", err)
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

// appendHistoricalAccounts projects the per-account master rows
// captured in the live `accounts` table backwards onto each
// historical as_of_date. Without this, `wealthdb accounts
// --as-of <historical-date>` would show no account for a date
// the historical positions cover.
//
// We use any one live `accounts` row per account_external_id
// (they're stable across snapshots for a given account) and copy
// nickname / portfolio / management_style verbatim. Gold's
// upsert pulls FirstSeenAt back to the historical date so the
// account's first-seen timestamp reflects the statement, not
// the toolkit-first-ran date.
func (c *Connection) appendHistoricalAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	hasMgmt, err := silver.HasColumn(ctx, c.db, "accounts", "management_style")
	if err != nil {
		return err
	}
	mgmtCol := "NULL"
	if hasMgmt {
		mgmtCol = "a.management_style"
	}
	// One live `accounts` row per distinct account_external_id
	// that appears in historical_position_snapshots (joined on
	// account, not date — the snapshot_at of the live row may be
	// well after the historical date).
	q := fmt.Sprintf(`
SELECT DISTINCT a.account_external_id, a.portfolio_external_id,
       a.nickname, a.payload, p.kind, COALESCE(%s, '')
  FROM historical_position_snapshots h
  JOIN accounts a
    ON a.account_external_id = h.account_external_id
  LEFT JOIN portfolios p
    ON p.snapshot_at = a.snapshot_at
   AND p.portfolio_external_id = a.portfolio_external_id
 WHERE h.as_of_date BETWEEN ? AND ?
   AND a.snapshot_at = (
       SELECT MAX(a2.snapshot_at) FROM accounts a2
        WHERE a2.account_external_id = a.account_external_id
   )`, mgmtCol)
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalAccounts: %w", err)
	}
	defer rows.Close()
	usd := "USD"
	type acctRow struct {
		extID, payload, silverMgmt           string
		portfolioID, nickname, portfolioKind sql.NullString
	}
	var seedRows []acctRow
	for rows.Next() {
		var r acctRow
		if err := rows.Scan(&r.extID, &r.portfolioID, &r.nickname, &r.payload,
			&r.portfolioKind, &r.silverMgmt); err != nil {
			return err
		}
		seedRows = append(seedRows, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// For each historical date in the window, emit one
	// AccountChange + one PortfolioChange per seed row. Without
	// the per-date emission, the byTime dispatch in Snapshots()
	// would have no master data for the historical timestamps.
	for snap, batch := range byTime {
		for _, r := range seedRows {
			change := canonical.AccountChange{
				AccountExternalID:   r.extID,
				AccountKind:         canonical.AccountKindBrokerage,
				BaseCurrency:        &usd,
				Nickname:            silver.NullStringPtr(r.nickname),
				PortfolioExternalID: silver.NullStringPtr(r.portfolioID),
				FirstSeenAt:         snap,
				LastSeenAt:          snap,
				Payload:             json.RawMessage(r.payload),
			}
			applyPortfolioKindTaxonomy(r.portfolioKind.String, &change)
			if r.silverMgmt != "" {
				s := canonical.ManagementStyle(r.silverMgmt)
				change.ManagementStyle = &s
			}
			batch.Accounts = append(batch.Accounts, change)
			if r.portfolioID.Valid && r.portfolioID.String != "" {
				display := r.portfolioID.String
				if r.portfolioKind.String != "" && r.portfolioKind.String != "other" {
					display = fmt.Sprintf("%s (%s)", r.portfolioID.String, r.portfolioKind.String)
				}
				batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
					PortfolioExternalID: r.portfolioID.String,
					DisplayName:         &display,
					BaseCurrency:        &usd,
					FirstSeenAt:         snap,
					LastSeenAt:          snap,
				})
			}
		}
	}
	return nil
}

// appendHistoricalPositions emits one InstrumentChange + one
// PositionChange per row in `historical_position_snapshots`. The
// silver schema is positions-only (no cash-balance counterpart
// for the 529 historical path), so we never produce
// CashBalanceChange rows from this source. The statement PDFs
// carry no structured type code, so the (asset_class, vehicle)
// pair comes from the shape heuristics in classifyHistoricalPair
// (instrument-key and description shapes); the live-positions
// path still overwrites
// the instrument dimension whenever the same instrument_key
// reappears with a source-classified value.
//
// When the silver cross-walk to `instrument_key` missed (column
// NULL), we synthesise a stable identity from the human-readable
// description so gold still has an instrument and position key.
// The fallback is "fidelity-hist:" + sha-prefix(description) and
// is deterministic per description, so re-loads converge.
func (c *Connection) appendHistoricalPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT as_of_date, account_external_id,
       COALESCE(instrument_key, ''),
       description,
       currency,
       CAST(quantity     AS VARCHAR),
       CAST(market_value AS VARCHAR),
       payload
  FROM historical_position_snapshots
 WHERE as_of_date BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalPositions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap                           int64
			acct, instrKey, desc, currency string
			qtyStr, valueStr               sql.NullString
			payload                        string
		)
		if err := rows.Scan(&snap, &acct, &instrKey, &desc, &currency,
			&qtyStr, &valueStr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		// Classified from the raw instrKey/desc (before the synthetic
		// key substitution below).
		assetClassNew, vehicle := classifyHistoricalPair(instrKey, desc)
		if instrKey == "" {
			instrKey = syntheticHistoricalInstrumentKey(desc)
		}
		symbol := instrKey
		name := desc
		ccy := currency
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrKey,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			Symbol:               &symbol,
			Name:                 &name,
			Currency:             &ccy,
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})

		instrumentKey := instrKey
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          instrKey,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			Currency:             currency,
			Quantity:             silver.DecimalPtrOrNil(qtyStr),
			MarketValue:          silver.DecimalPtrOrNil(valueStr),
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// syntheticHistoricalInstrumentKey deterministically derives a
// gold-side instrument identity from a fund description when
// the silver cross-walk to a real ticker missed. Stable across
// re-loads so the same statement → same gold rows.
func syntheticHistoricalInstrumentKey(description string) string {
	sum := sha256.Sum256([]byte(description))
	return "fidelity-hist:" + hex.EncodeToString(sum[:8])
}
