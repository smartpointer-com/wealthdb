package relevate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// Historical-snapshot reader for the relevate silver migration 0002
// tables (`historical_position_snapshots`, `historical_cash_balances`),
// reconstructed from the Quartalsbericht PDFs that the documents
// phase of the loader already saves under bronze.
//
// These tables live in PARALLEL to the live `positions` /
// `cash_balances` paths. The identity model differs:
//
//   - Live `positions` rows are derived from the modelportfolio
//     endpoint and carry only a TARGET allocation (0..1 fraction);
//     market value is computed at adapter time as
//     securities_balance × allocation. Instrument identity uses
//     Relevate's internal numeric security.id.
//   - Historical PDF rows carry ACTUAL units held + the CHF market
//     value verbatim. The PDF doesn't expose the internal
//     security.id, only ISIN — so historical positions use ISIN
//     as the instrument key.
//
// snapshotsHistorical does NOT spawn its own SnapshotStream. The
// caller (Connection.Snapshots) merges historical batches into the
// same byTime map the live readers populate, so a snapshot_at that
// has BOTH live and historical content gets a single combined
// batch.

// hasHistoricalTables returns true when migration 0002 has been
// applied to the silver. Older silvers (still on 0001) silently
// skip the historical projection.
func (c *Connection) hasHistoricalTables(ctx context.Context) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
 WHERE type = 'table'
   AND name IN ('historical_position_snapshots', 'historical_cash_balances')
`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasHistoricalTables: %w", err)
	}
	return n == 2, nil
}

// historicalSnapshotTimes returns the distinct snapshot_at values
// present in either historical table within [start, end]. Returns
// nil when migration 0002 hasn't been applied.
func (c *Connection) historicalSnapshotTimes(
	ctx context.Context, start, end int64,
) ([]int64, error) {
	ok, err := c.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return nil, err
	}
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM historical_position_snapshots WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL SELECT snapshot_at FROM historical_cash_balances    WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, start, end, start, end)
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

// historicalRange returns MIN/MAX snapshot_at across the two
// historical tables. Both -1 when the silver has no historical
// rows (or hasn't been migrated to 0002). Used by ChangeWindow to
// extend Start backwards so the loader's window-DELETE covers
// existing historical rows before the re-INSERT.
func (c *Connection) historicalRange(ctx context.Context) (int64, int64, error) {
	ok, err := c.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return -1, -1, err
	}
	var (
		posMin, posMax   sql.NullInt64
		cashMin, cashMax sql.NullInt64
	)
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM historical_position_snapshots`,
	).Scan(&posMin, &posMax); err != nil {
		return -1, -1, fmt.Errorf("historicalRange positions: %w", err)
	}
	if err := c.db.QueryRowContext(ctx,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM historical_cash_balances`,
	).Scan(&cashMin, &cashMax); err != nil {
		return -1, -1, fmt.Errorf("historicalRange cash: %w", err)
	}
	lo, hi := int64(-1), int64(-1)
	merge := func(n sql.NullInt64) {
		if !n.Valid {
			return
		}
		if lo == -1 || n.Int64 < lo {
			lo = n.Int64
		}
		if hi == -1 || n.Int64 > hi {
			hi = n.Int64
		}
	}
	merge(posMin)
	merge(posMax)
	merge(cashMin)
	merge(cashMax)
	return lo, hi, nil
}

// appendHistoricalPositions reads historical_position_snapshots and
// emits one PositionChange + one InstrumentChange per row. An
// AccountChange is emitted ONCE per (snapshot_at, account) tuple so
// gold can attribute positions to a real account row for historical
// dates that don't have a live dump_run.
//
// Account attributes (TaxWrapper, ManagementStyle, BaseCurrency,
// AccountKind) mirror the live appendAccounts decisions — Relevate
// is uniformly vested-benefits / automated / CHF / brokerage. The
// live and historical AccountChanges therefore agree everywhere
// they overlap, and gold's per-column upsert keeps the more
// detailed live one (with DisplayName and AccountCategory) on dates
// the live path observes.
func (c *Connection) appendHistoricalPositions(
	ctx context.Context, w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
) error {
	ok, err := c.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return err
	}
	const q = `
SELECT snapshot_at, account_external_id, isin,
       security_name, COALESCE(asset_class, ''),
       currency, units, market_value, payload
  FROM historical_position_snapshots
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalPositions: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperVestedBenefits
	style := canonical.ManagementStyleAutomated
	chf := "CHF"

	// Dedup so AccountChange + InstrumentChange aren't emitted
	// once per holding within the same snapshot.
	accountEmitted := map[[2]int64]bool{}
	instrumentEmitted := map[[2]int64]bool{}

	for rows.Next() {
		var (
			snap           int64
			acct, isin     string
			secName, asset string
			ccy            string
			units, mv      sql.NullFloat64
			payload        string
		)
		if err := rows.Scan(&snap, &acct, &isin, &secName, &asset,
			&ccy, &units, &mv, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		// One AccountChange per (snap, account).
		acctKey := [2]int64{snap, int64(strHash(acct))}
		if !accountEmitted[acctKey] {
			accountEmitted[acctKey] = true
			w := wrapper
			s := style
			baseCcy := chf
			batch.Accounts = append(batch.Accounts, canonical.AccountChange{
				AccountExternalID: acct,
				AccountKind:       canonical.AccountKindBrokerage,
				BaseCurrency:      &baseCcy,
				TaxWrapper:        &w,
				ManagementStyle:   &s,
				FirstSeenAt:       snap,
				LastSeenAt:        snap,
			})
		}

		// V2 taxonomy pair, shared by the instrument and position
		// changes below.
		acNew, veh := taxonomyFor(asset, secName)

		// One InstrumentChange per (snap, ISIN).
		instKey := [2]int64{snap, int64(strHash(isin))}
		if !instrumentEmitted[instKey] {
			instrumentEmitted[instKey] = true
			isinCopy := isin
			nameCopy := secName
			ic := canonical.InstrumentChange{
				InstrumentExternalID: isin, // ISIN as the identity key here
				ISIN:                 &isinCopy,
				Symbol:               &isinCopy,
				Name:                 &nameCopy,
				AssetClass:           acNew,
				Vehicle:              veh,
				Currency:             &chf,
				FirstSeenAt:          snap,
				LastSeenAt:           snap,
			}
			batch.Instruments = append(batch.Instruments, ic)
		}

		// PositionChange — market_value is direct from the PDF
		// (CHF, the portfolio reference currency).
		isinCopy := isin
		change := canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    acct,
			PositionKey:          isin,
			InstrumentExternalID: &isinCopy,
			AssetClass:           acNew,
			Vehicle:              veh,
			Currency:             ccy,
			Payload:              json.RawMessage(payload),
		}
		if units.Valid {
			q := canonical.NewDecimalFromFloat(units.Float64)
			change.Quantity = &q
		}
		if mv.Valid {
			d := canonical.NewDecimalFromFloat(mv.Float64)
			change.MarketValue = &d
		}
		batch.Positions = append(batch.Positions, change)
	}
	return rows.Err()
}

// appendHistoricalCash reads historical_cash_balances and emits a
// single CashBalanceChange per (snapshot_at, account, currency) by
// summing 'cash' + 'accrued_interest' rows into one
// BalanceKindCurrent amount — matching the live path's
// "total liquid value awaiting investment" semantics. Pure
// 'accrued_interest' rows would otherwise inflate the cash bucket
// against gold's portfolio totals.
//
// An AccountChange is emitted ONCE per (snap, account) so any
// snapshot that has only cash and no positions still attributes
// to a real account row in gold.
func (c *Connection) appendHistoricalCash(
	ctx context.Context, w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
) error {
	ok, err := c.hasHistoricalTables(ctx)
	if err != nil || !ok {
		return err
	}
	const q = `
SELECT snapshot_at, account_external_id, currency, SUM(amount)
  FROM historical_cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
   AND balance_kind IN ('cash', 'accrued_interest')
 GROUP BY snapshot_at, account_external_id, currency`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalCash: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperVestedBenefits
	style := canonical.ManagementStyleAutomated
	chf := "CHF"
	accountEmitted := map[[2]int64]bool{}

	for rows.Next() {
		var (
			snap   int64
			acct   string
			ccy    string
			amount sql.NullFloat64
		)
		if err := rows.Scan(&snap, &acct, &ccy, &amount); err != nil {
			return err
		}
		if !amount.Valid {
			continue
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		acctKey := [2]int64{snap, int64(strHash(acct))}
		if !accountEmitted[acctKey] {
			accountEmitted[acctKey] = true
			w := wrapper
			s := style
			baseCcy := chf
			batch.Accounts = append(batch.Accounts, canonical.AccountChange{
				AccountExternalID: acct,
				AccountKind:       canonical.AccountKindBrokerage,
				BaseCurrency:      &baseCcy,
				TaxWrapper:        &w,
				ManagementStyle:   &s,
				FirstSeenAt:       snap,
				LastSeenAt:        snap,
			})
		}

		amt := canonical.NewDecimalFromFloat(amount.Float64)
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

// strHash is FNV-1a 64-bit — same one UBS's historical reader
// uses, copied here so the relevate package stays self-contained.
// Used to compress account / ISIN string keys into the dedup
// (int64, int64) tuples without allocating a per-call map.
func strHash(s string) uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
