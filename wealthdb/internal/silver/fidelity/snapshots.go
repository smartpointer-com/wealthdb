package fidelity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	histTimes, err := c.historicalSnapshotTimes(ctx, w)
	if err != nil {
		return nil, err
	}
	times, err := c.snapshotTimesInWindow(ctx, w, histTimes)
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
	// Read once for both account paths: they emit the same account at
	// the same snapshot, and the fold keeps whichever comes last.
	held, err := c.heldAccountKinds(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.appendAccounts(ctx, w, byTime, held); err != nil {
		return nil, err
	}
	lots, err := c.readOpenLots(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.appendPositionsAndCash(ctx, w, byTime, lots); err != nil {
		return nil, err
	}
	lots.logSkipped()
	if len(histTimes) > 0 {
		// Project the master rows onto each historical date
		// before emitting the historical positions — gold
		// upserts run by time-batch and historical-only dates
		// would otherwise carry positions without accounts.
		if err := c.appendHistoricalAccounts(ctx, w, byTime, held); err != nil {
			return nil, err
		}
		if err := c.appendHistoricalPositions(ctx, w, byTime); err != nil {
			return nil, err
		}
	}

	// times is the sorted union of live + historical times, so batches
	// stream in chronological order (important for gold's earlier-wins
	// FirstSeenAt accounting on instruments + accounts).
	out := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		out = append(out, *byTime[t])
	}
	return silver.NewSnapshotStream(out), nil
}

// snapshotTimesInWindow returns the union of distinct snapshot_at
// values across dump_runs and the snapshot-typed content tables,
// plus the extra (historical) times. All four tables typically hold
// the same set (Fidelity dumps create a dump_runs row and
// same-timestamp positions/accounts/portfolios rows), but the union
// is defensive against partial dumps.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window, extra []int64) ([]int64, error) {
	return silver.SnapshotTimes(ctx, c.db, w, extra, `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM dump_runs  WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM positions  WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM accounts   WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM portfolios WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`,
		w.Start, w.End, w.Start, w.End, w.Start, w.End, w.Start, w.End)
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
			snap                 int64
			extID, kind, payload string
		)
		if err := rows.Scan(&snap, &extID, &kind, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		display := portfolioDisplay(extID, kind)
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

// portfolioDisplay is a portfolio's DisplayName: its label with the
// silver `kind` ('529' / 'trust_managed' / 'daf') promoted beside it,
// so the `wealthdb portfolios` output shows something more meaningful
// than the bare label. The label stays the PortfolioExternalID for
// joins.
func portfolioDisplay(id, kind string) string {
	if kind != "" && kind != "other" {
		return fmt.Sprintf("%s (%s)", id, kind)
	}
	return id
}

// appendAccounts joins each silver account against its portfolio
// to lift the silver-side `portfolios.kind` discriminator into
// the canonical taxonomy:
//
//   - kind=529           → TaxWrapper=529 (US education-savings).
//   - kind=trust_managed → TaxWrapper=trust_non_grantor.
//   - kind=daf           → AccountKind=donor_advised_fund +
//     TaxWrapper=charitable (Fidelity Charitable Giving Account;
//     fidelity-web DESIGN.md §12.3).
//   - kind=other         → leave TaxWrapper nil so a config-side
//     override can pin per-account values.
//
// ManagementStyle comes from silver's promoted column
// `accounts.management_style` (fidelity-web silver migrations 0003
// and 0006: '529' → 'automated' (a model-portfolio plan, not free
// selection), 'trust_managed' → 'discretionary', 'other'/NULL →
// NULL). A silver without the column falls back to deriving the
// style from portfolios.kind in the same shape.
//
// All taxonomy columns stay nil for accounts whose portfolio has
// no classification or no portfolio at all; the gold COALESCE
// upsert preserves whatever a later writer / override supplies.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, held map[string]canonical.AccountKind) error {
	hasMgmt, err := silver.HasColumn(ctx, c.db, "accounts", "management_style")
	if err != nil {
		return err
	}
	mgmtCol := "NULL"
	if hasMgmt {
		mgmtCol = "a.management_style"
	}
	q := fmt.Sprintf(`
SELECT a.snapshot_at, a.account_external_id, a.portfolio_external_id,
       a.nickname, a.payload, p.kind, COALESCE(%s, '')
  FROM accounts a
  LEFT JOIN portfolios p
    ON p.snapshot_at = a.snapshot_at
   AND p.portfolio_external_id = a.portfolio_external_id
 WHERE a.snapshot_at BETWEEN ? AND ?`, mgmtCol)
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	usd := "USD"
	for rows.Next() {
		var (
			snap                                 int64
			extID, payload, silverMgmt           string
			portfolioID, nickname, portfolioKind sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &portfolioID, &nickname, &payload,
			&portfolioKind, &silverMgmt); err != nil {
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
			Nickname:            silver.NullStringPtr(nickname),
			PortfolioExternalID: silver.NullStringPtr(portfolioID),
			FirstSeenAt:         snap,
			LastSeenAt:          snap,
			Payload:             json.RawMessage(payload),
		}
		applyPortfolioKindTaxonomy(portfolioKind.String, &change)
		applyHeldKind(extID, held, &change)
		if silverMgmt != "" {
			// Silver-side management_style (v3+) wins over the
			// adapter-derived value: trust_managed accounts get
			// discretionary from both paths and agree; 529
			// accounts get automated from silver only (the
			// adapter's kind→style mapping doesn't have a 529
			// entry, intentionally — the silver column is the
			// canonical source for that).
			s := canonical.ManagementStyle(silverMgmt)
			change.ManagementStyle = &s
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// applyPortfolioKindTaxonomy stamps TaxWrapper (plus AccountKind
// for the DAF, and on pre-v3 silvers ManagementStyle) on an
// AccountChange based on the joined silver portfolios.kind. See
// appendAccounts for the mapping rationale. No-op when kind is
// empty or 'other'.
//
// ManagementStyle here is only meaningful for the trust_managed
// branch — it's the backward-compat path for silvers without
// the v3 management_style column. v3+ silvers overwrite this
// in the caller with the explicit silver value (which also
// covers 529 → automated and daf → automated, the cases this
// helper doesn't classify).
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
	case "daf":
		// Donor-Advised Fund (fidelity-web silver v7+, DESIGN.md
		// §12.3): its own container kind so gold can include or
		// exclude the irrevocably-donated balance by kind, under
		// the charitable tax wrapper.
		change.AccountKind = canonical.AccountKindDonorAdvisedFund
		w := canonical.TaxWrapperCharitable
		change.TaxWrapper = &w
	}
}

// applyHeldKind types an account by what it holds (heldAccountKinds),
// whatever its portfolio says. One carrying a home loan's outstanding
// principal is a mortgage account, as the UBS adapter types its loans:
// it is what lets gold read the loan's balance series as a liability —
// the cash flow statement's interest/principal split, and the returns
// engine, which reports it only on the accounts grain's liability line
// and leaves it out of every coarser rollup (splitLiabilities). One
// carrying a deposit account's balance is a cash account, as every bank
// adapter types a checking or savings account.
func applyHeldKind(id string, held map[string]canonical.AccountKind, change *canonical.AccountChange) {
	if kind, ok := held[id]; ok {
		change.AccountKind = kind
	}
}

// pendingActivity names the row Fidelity lists money in flight under.
const pendingActivity = "Pending activity"

// addCurrentCash adds amt to the account's current cash in currency
// at snap: one balance per account, currency and kind, the core
// position's payload kept where there is one.
func addCurrentCash(batch *canonical.SnapshotBatch, snap int64, acct, currency string, amt canonical.Decimal,
	payload string, core bool) {
	for i := range batch.CashBalances {
		c := &batch.CashBalances[i]
		if c.AccountExternalID == acct && c.Currency == currency && c.BalanceKind == canonical.BalanceKindCurrent {
			c.Amount = c.Amount.Add(amt)
			if core {
				c.Payload = json.RawMessage(payload)
			}
			return
		}
	}
	batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
		SnapshotAt:        snap,
		AccountExternalID: acct,
		Currency:          currency,
		BalanceKind:       canonical.BalanceKindCurrent,
		Amount:            amt,
		Payload:           json.RawMessage(payload),
	})
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
//     name + asset class) plus PositionChange (the holding line),
//     whose book value is Fidelity's cost basis total, and its open
//     lots where a fetch still describes it (lots.go).
//
// A "Pending activity" row (named so in instrument_key or in
// description) is money in flight that Fidelity counts in the
// account's value: it adds to the account's current cash, beside the
// core position, so the account keeps its total and no position of
// no identity appears. A row with no instrument_key and no such name
// is skipped.
//
// `asset_class`, `currency`, and `is_core_position` are all
// promoted columns on silver; the adapter relies on them directly
// rather than re-deriving from instrument_key + description.
func (c *Connection) appendPositionsAndCash(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, lots *lotCarry) error {
	// CAST decimals to VARCHAR so SQLite's REAL → float64 round-
	// trip doesn't bleed precision before we parse into the
	// shopspring/decimal-backed canonical.Decimal.
	const q = `
SELECT snapshot_at, account_external_id, instrument_key,
       COALESCE(description, ''),
       COALESCE(asset_class, ''),
       currency,
       is_core_position,
       CAST(quantity         AS VARCHAR),
       CAST(current_value    AS VARCHAR),
       CAST(cost_basis_total AS VARCHAR),
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
			snap                                   int64
			acct, key, desc, silverClass, currency string
			isCore                                 int
			qtyStr, valueStr, costStr              sql.NullString
			payload                                string
		)
		if err := rows.Scan(&snap, &acct, &key, &desc, &silverClass, &currency,
			&isCore, &qtyStr, &valueStr, &costStr, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		pending := strings.EqualFold(key, pendingActivity) || strings.EqualFold(desc, pendingActivity)
		if key == "" && !pending {
			continue
		}

		if isCore != 0 || pending {
			amt, err := silver.DecimalOrZero(valueStr)
			if err != nil {
				return fmt.Errorf("cash amount parse (acct=%s instr=%s): %w", acct, key, err)
			}
			addCurrentCash(batch, snap, acct, currency, amt, payload, isCore != 0)
			continue
		}

		// Map silverClass + name to the single (exposure, vehicle)
		// pair emitted to gold.
		assetClassNew, vehicle := assetClassVehicleFor(silverClass, desc)
		symbol := key
		ccy := currency
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: key,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			Symbol:               &symbol,
			Name:                 silver.StrPtrIfNonEmpty(desc),
			Currency:             &ccy,
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              json.RawMessage(payload),
		})

		instrumentKey := key
		pos := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          key,
			InstrumentExternalID: &instrumentKey,
			AssetClass:           assetClassNew,
			Vehicle:              vehicle,
			Currency:             currency,
			Quantity:             silver.DecimalPtrOrNil(qtyStr),
			MarketValue:          silver.DecimalPtrOrNil(valueStr),
			Payload:              json.RawMessage(payload),
		}
		pos.SetBookValue(silver.DecimalPtrOrNil(costStr), lotBasis)
		lots.attach(batch, &pos)
		batch.Positions = append(batch.Positions, pos)
	}
	return rows.Err()
}
