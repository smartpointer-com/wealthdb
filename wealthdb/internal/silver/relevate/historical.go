package relevate

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
// The historical readers do NOT spawn their own SnapshotStream.
// The caller (Connection.Snapshots) merges historical rows into the
// same byTime map the live readers populate, so a snapshot_at that
// has BOTH live and historical content gets a single combined
// batch.

// hasHistorical reports whether the silver holds the historical tables
// (silver migration 0002). Older silvers skip the historical projection.
func (c *Connection) hasHistorical() bool {
	return c.schema.historicalPositions && c.schema.historicalCash
}

// historicalSnapshotTimes returns the distinct snapshot_at values
// present in either historical table within the window. Returns
// nil when migration 0002 hasn't been applied.
func (c *Connection) historicalSnapshotTimes(ctx context.Context, w canonical.Window) ([]int64, error) {
	if !c.hasHistorical() {
		return nil, nil
	}
	return silver.SnapshotTimesOf(ctx, c.db, w, nil,
		"historical_position_snapshots", "historical_cash_balances")
}

// historicalAccount is the account a historical row attributes to on
// a date with no live dump_run: the live appendAccounts decisions
// without the display name and category the PDFs do not state.
func historicalAccount(acct string, snap int64) canonical.AccountChange {
	wrapper := canonical.TaxWrapperVestedBenefits
	style := canonical.ManagementStyleAutomated
	chf := "CHF"
	return canonical.AccountChange{
		AccountExternalID: acct,
		AccountKind:       canonical.AccountKindBrokerage,
		BaseCurrency:      &chf,
		TaxWrapper:        &wrapper,
		ManagementStyle:   &style,
		FirstSeenAt:       snap,
		LastSeenAt:        snap,
	}
}

// snapKey names one id at one snapshot, for emitting a dimension
// record once per batch.
type snapKey struct {
	snap int64
	id   string
}

// historicalRange returns MIN/MAX snapshot_at across the two
// historical tables. Both -1 when the silver has no historical
// rows (or hasn't been migrated to 0002). Used by ChangeWindow to
// extend Start backwards so the loader's window-DELETE covers
// existing historical rows before the re-INSERT.
func (c *Connection) historicalRange(ctx context.Context) (int64, int64, error) {
	if !c.hasHistorical() {
		return -1, -1, nil
	}
	return silver.Extrema(ctx, c.db, "historicalRange", []string{
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM historical_position_snapshots`,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM historical_cash_balances`,
	})
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
	if !c.hasHistorical() {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, isin,
       security_name, COALESCE(asset_class, ''),
       currency, units, market_value, payload
  FROM historical_position_snapshots
 WHERE snapshot_at BETWEEN ? AND ?`

	chf := "CHF"

	// Dedup so AccountChange + InstrumentChange aren't emitted
	// once per holding within the same snapshot.
	accountEmitted := map[snapKey]bool{}
	instrumentEmitted := map[snapKey]bool{}

	if err := silver.ScanRows(ctx, c.db, "appendHistoricalPositions", q, func(rows *sql.Rows) error {
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
			return nil
		}

		// One AccountChange per (snap, account).
		if k := (snapKey{snap, acct}); !accountEmitted[k] {
			accountEmitted[k] = true
			batch.Accounts = append(batch.Accounts, historicalAccount(acct, snap))
		}

		// The (asset_class, vehicle) pair, shared by the instrument
		// and position changes below.
		acNew, veh := taxonomyFor(asset, secName)

		// One InstrumentChange per (snap, ISIN).
		if k := (snapKey{snap, isin}); !instrumentEmitted[k] {
			instrumentEmitted[k] = true
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
		return nil
	}, w.Start, w.End); err != nil {
		return err
	}
	return nil
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
	if !c.hasHistorical() {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, currency, SUM(amount)
  FROM historical_cash_balances
 WHERE snapshot_at BETWEEN ? AND ?
   AND balance_kind IN ('cash', 'accrued_interest')
 GROUP BY snapshot_at, account_external_id, currency`

	accountEmitted := map[snapKey]bool{}

	if err := silver.ScanRows(ctx, c.db, "appendHistoricalCash", q, func(rows *sql.Rows) error {
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
			return nil
		}
		batch, ok := byTime[snap]
		if !ok {
			return nil
		}

		if k := (snapKey{snap, acct}); !accountEmitted[k] {
			accountEmitted[k] = true
			batch.Accounts = append(batch.Accounts, historicalAccount(acct, snap))
		}

		amt := canonical.NewDecimalFromFloat(amount.Float64)
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
