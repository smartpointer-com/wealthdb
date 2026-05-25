package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}
	times, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendPortfolios(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositionsAndCash(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(times))}
	for _, t := range times {
		out.batches = append(out.batches, *byTime[t])
	}
	return out, nil
}

func (s *snapshotStream) Next(context.Context) (canonical.SnapshotBatch, bool, error) {
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *snapshotStream) Close() error { return nil }

// snapshotTimesInWindow returns the union of distinct snapshot_at
// values across dump_runs and the snapshot-typed content tables.
// All four are typically the same set (Fidelity dumps create a
// dump_runs row and same-timestamp positions/accounts/portfolios
// rows), but the union is defensive against partial dumps.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM dump_runs  WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM positions  WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM accounts   WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM portfolios WHERE snapshot_at BETWEEN ? AND ?
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

func (c *Connection) appendPortfolios(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, portfolio_external_id, kind, payload
  FROM portfolios
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPortfolios: %w", err)
	}
	defer rows.Close()
	usd := "USD"
	for rows.Next() {
		var (
			snap                  int64
			extID, kind, payload  string
		)
		if err := rows.Scan(&snap, &extID, &kind, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		// Promote the silver `kind` ('529' / 'trust_managed' /
		// 'other') as the portfolio's DisplayName so the
		// `wealthdb portfolios` output shows something more
		// meaningful than the bare label. Real label remains the
		// PortfolioExternalID for joins.
		display := extID
		if kind != "" && kind != "other" {
			display = fmt.Sprintf("%s (%s)", extID, kind)
		}
		batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
			PortfolioExternalID: extID,
			DisplayName:         &display,
			BaseCurrency:        &usd,
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// appendAccounts joins each silver account against its portfolio
// to lift the silver-side `portfolios.kind` discriminator
// (529 / trust_managed / other) into the canonical taxonomy:
//
//   - kind=529           → TaxWrapper=529 (US education-savings).
//   - kind=trust_managed → TaxWrapper=trust_non_grantor +
//                          ManagementStyle=discretionary, since
//                          "trust_managed" by definition implies
//                          a third-party investment manager
//                          holding limited POA.
//   - kind=other         → leave TaxWrapper/ManagementStyle nil
//                          so a config-side override can pin
//                          per-account values (e.g. an IRA
//                          nickname that silver can't classify).
//
// Both columns stay nil for accounts whose portfolio has no
// classification or no portfolio at all; the gold COALESCE
// upsert preserves whatever a later writer / override supplies.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT a.snapshot_at, a.account_external_id, a.portfolio_external_id,
       a.nickname, a.payload, p.kind
  FROM accounts a
  LEFT JOIN portfolios p
    ON p.snapshot_at = a.snapshot_at
   AND p.portfolio_external_id = a.portfolio_external_id
 WHERE a.snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	usd := "USD"
	for rows.Next() {
		var (
			snap                                          int64
			extID, payload                                string
			portfolioID, nickname, portfolioKind          sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &portfolioID, &nickname, &payload, &portfolioKind); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		change := canonical.AccountChange{
			AccountExternalID:   extID,
			AccountKind:         canonical.AccountKindBrokerage,
			BaseCurrency:        &usd,
			Nickname:            nullStringPtr(nickname),
			PortfolioExternalID: nullStringPtr(portfolioID),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		}
		applyPortfolioKindTaxonomy(portfolioKind.String, &change)
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// applyPortfolioKindTaxonomy stamps TaxWrapper / ManagementStyle
// on an AccountChange based on the joined silver portfolios.kind.
// See appendAccounts for the mapping rationale. No-op when kind
// is empty or 'other'.
func applyPortfolioKindTaxonomy(kind string, change *canonical.AccountChange) {
	switch kind {
	case "529":
		w := canonical.TaxWrapper529
		change.TaxWrapper = &w
	case "trust_managed":
		w := canonical.TaxWrapperTrustNonGrantor
		s := canonical.ManagementStyleDiscretionary
		change.TaxWrapper = &w
		change.ManagementStyle = &s
	}
}

// appendPositionsAndCash walks `positions` once and splits each
// row down one of two paths based on the silver-side
// `is_core_position` flag:
//
//   - is_core_position=1 → CashBalanceChange with
//     BalanceKindCurrent. Fidelity surfaces money-market core
//     positions (FDRXX / SPAXX / ...) as ordinary positions
//     rows; the gold convention is to route them into
//     cash_balances so `wealthdb positions --with-cash` and the
//     cash_balance aggregate column populate uniformly across
//     sources.
//   - everything else → InstrumentChange (registers identity +
//     name + asset class) plus PositionChange (the holding line).
//
// "Pending activity" rows (no instrument_key, description
// "Pending activity") are skipped — Fidelity hasn't booked them
// yet, so they have no resolvable instrument identity. They
// reappear on the next dump as proper rows.
//
// `asset_class`, `currency`, and `is_core_position` are all
// promoted columns on silver (added by the maintainer after the
// first adapter review); the adapter relies on them directly
// rather than re-deriving from instrument_key + description.
func (c *Connection) appendPositionsAndCash(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// CAST decimals to VARCHAR so SQLite's REAL → float64 round-
	// trip doesn't bleed precision before we parse into the
	// shopspring/decimal-backed canonical.Decimal.
	const q = `
SELECT snapshot_at, account_external_id, instrument_key,
       COALESCE(description, ''),
       COALESCE(asset_class, ''),
       currency,
       is_core_position,
       CAST(quantity      AS VARCHAR),
       CAST(current_value AS VARCHAR),
       payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositionsAndCash: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap                                          int64
			acct, key, desc, silverClass, currency        string
			isCore                                        int
			qtyStr, valueStr                              sql.NullString
			payload                                       string
		)
		if err := rows.Scan(&snap, &acct, &key, &desc, &silverClass, &currency,
			&isCore, &qtyStr, &valueStr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		if key == "" || desc == "Pending activity" {
			// Pending-activity rows have no resolvable identity;
			// emit nothing. The next dump should land them as
			// real positions or transactions.
			continue
		}

		if isCore != 0 {
			amt, err := decimalOrZero(valueStr)
			if err != nil {
				return fmt.Errorf("money-market amount parse (acct=%s instr=%s): %w", acct, key, err)
			}
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: acct,
				Currency:          currency,
				BalanceKind:       canonical.BalanceKindCurrent,
				Amount:            amt,
				Payload:           json.RawMessage(payload),
			})
			continue
		}

		assetClass := assetClassFor(silverClass)
		symbol := key
		ccy := currency
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: key,
			AssetClass:           assetClass,
			Symbol:               &symbol,
			Name:                 strPtrIfNonEmpty(desc),
			Currency:             &ccy,
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              json.RawMessage(payload),
		})

		instrumentKey := key
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          key,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClass,
			Currency:             currency,
			Quantity:             decimalPtrOrNil(qtyStr),
			MarketValue:          decimalPtrOrNil(valueStr),
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// ---- helpers -------------------------------------------------------------

func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func decimalPtrOrNil(s sql.NullString) *canonical.Decimal {
	if !s.Valid || s.String == "" {
		return nil
	}
	d, err := canonical.NewDecimalFromString(s.String)
	if err != nil {
		return nil
	}
	return &d
}

func decimalOrZero(s sql.NullString) (canonical.Decimal, error) {
	if !s.Valid || s.String == "" {
		return canonical.Decimal{}, nil
	}
	return canonical.NewDecimalFromString(s.String)
}
