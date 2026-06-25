package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// Historical-snapshot reader for the ubs-web silver
// migration 0002 tables (`historical_position_snapshots`,
// `historical_cash_balances`). These tables are reconstructed
// from PDF Statements of Assets (quarterly) and Account Statements
// (monthly) — see the silver migration comment for the full
// shape. They live in PARALLEL to the live-fetch `positions` /
// `accounts` tables.
//
// Why surface them here rather than fold into the live web
// reader: the identity model differs (live web uses 4-char
// portfolio codes like RNNN/NNNN, the PDFs use PSN-aligned
// 'BBBBAAAAAAAANN'), the temporal grain differs (intra-day live
// vs end-of-period PDF), and the date range pre-dates live web.
// The merge orchestrator runs this stream first so historical
// pre-PSN-start dates are populated, then live web emits
// dimensions in the overlap, then PSN takes over facts.

// snapshotsHistorical emits one batch per distinct as_of_date in
// the window plus one batch per distinct period_end. Returns an
// empty stream when no historical rows fall inside [w.Start, w.End].
//
// Window semantics: the loader's window-DELETE deletes positions
// and cash_balances rows WHERE snapshot_at BETWEEN w.Start AND
// w.End, so any historical timestamp we emit must be inside the
// window or the next load will collide on the gold PK. The
// webReader's ChangeWindow extends Start back to MIN(historical
// times) when there's a new dump_run, so this loop's WHERE clause
// is just a defensive filter — it could equivalently read every
// historical row. Filtering keeps the read cheap when the loader
// is incrementally advancing past a single new dump_run.
func (r *webReader) snapshotsHistorical(
	ctx context.Context,
	w canonical.Window,
	safekeepingByPortfolio map[string]string,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}

	byTime := make(map[int64]*canonical.SnapshotBatch)
	getBatch := func(t int64) *canonical.SnapshotBatch {
		if b, ok := byTime[t]; ok {
			return b
		}
		b := &canonical.SnapshotBatch{}
		byTime[t] = b
		return b
	}

	if err := r.appendHistoricalSecurities(ctx, w, getBatch, safekeepingByPortfolio); err != nil {
		return nil, err
	}
	if err := r.appendHistoricalCashBalances(ctx, w, getBatch); err != nil {
		return nil, err
	}
	// Mortgages run LAST and peek at byTime directly (not getBatch)
	// so they attach to existing portfolio snapshots without
	// creating new ones — see the function comment.
	if err := r.appendHistoricalMortgages(ctx, w, byTime); err != nil {
		return nil, err
	}

	times := make([]int64, 0, len(byTime))
	for t := range byTime {
		times = append(times, t)
	}
	sortInt64Asc(times)

	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batches = append(batches, *byTime[t])
	}
	return silver.NewSnapshotStream(batches), nil
}

// appendHistoricalSecurities emits security positions from
// `historical_position_snapshots` (rows where instrument_isin IS
// NOT NULL). UBS PDFs don't surface the safekeeping account
// reliably, so silver leaves account_external_id='' on these
// rows. We attach them to the per-portfolio overlay account
// pattern PSN already uses for forward contracts (account_kind=
// 'overlay', '<portfolio>:overlay'), preserving the invariant
// that every position is owned by some account row.
//
// Cash rows (instrument_isin IS NULL) from this same table are
// intentionally skipped — they overlap with `historical_cash_
// balances` rows on the same (account, currency, period_end) and
// would collide on the gold cash_balances PK. The monthly cash-
// balances table is the richer source (opening + closing per
// month vs quarter-end only), so we use it exclusively for cash.
// safekeepingByPortfolio maps a portfolio_external_id to the PSN
// safekeeping account that holds its securities (1:1 portfolios
// only — see psnReader.safekeepingByPortfolio). When a portfolio
// is present, its historical securities attach to that real
// safekeeping account_external_id, giving account-by-account
// continuity across the web→PSN cutover. When absent (PSN not
// configured, or an ambiguous 1:many portfolio) the security
// falls back to the synthetic per-portfolio overlay account.
func (r *webReader) appendHistoricalSecurities(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	safekeepingByPortfolio map[string]string,
) error {
	const q = `
SELECT as_of_date, portfolio_external_id, instrument_isin, currency_iso,
       units, market_value, market_value_currency,
       cost_price, market_price, accrued_interest, description, payload
  FROM historical_position_snapshots
 WHERE instrument_isin IS NOT NULL
   AND as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalSecurities: %w", err)
	}
	defer rows.Close()

	// Track which (snapshot, portfolio) accounts and portfolios
	// we've already emitted to avoid one PortfolioChange /
	// AccountChange per holding.
	accountEmitted := map[[2]int64]bool{}
	portfolioEmitted := map[[2]int64]bool{}

	for rows.Next() {
		var (
			asOf                            int64
			portID, isin, ccy, mvCcy        string
			descr                           sql.NullString
			units, mv, cost, price, accrued sql.NullFloat64
			payload                         string
		)
		if err := rows.Scan(&asOf, &portID, &isin, &ccy, &units, &mv, &mvCcy,
			&cost, &price, &accrued, &descr, &payload); err != nil {
			return err
		}
		batch := getBatch(asOf)

		portKey := [2]int64{asOf, int64(strHash(portID))}
		if !portfolioEmitted[portKey] {
			portfolioEmitted[portKey] = true
			batch.Portfolios = append(batch.Portfolios, canonical.PortfolioChange{
				PortfolioExternalID: portID,
				BaseCurrency:        silver.StrPtrIfNonEmpty(mvCcy),
				FirstSeenAt:         asOf,
				LastSeenAt:          asOf,
			})
		}

		// Prefer the real PSN safekeeping account so this security's
		// history is continuous with the PSN-era holdings on the
		// same account. Fall back to the synthetic overlay when no
		// unambiguous mapping exists.
		accountID, mapped := safekeepingByPortfolio[portID]
		if !mapped {
			accountID = overlayAccountID(portID)
		}
		if !accountEmitted[portKey] {
			accountEmitted[portKey] = true
			pid := portID
			ac := canonical.AccountChange{
				AccountExternalID:   accountID,
				PortfolioExternalID: &pid,
				FirstSeenAt:         asOf,
				LastSeenAt:          asOf,
			}
			if mapped {
				// Real safekeeping account. Leave DisplayName nil so
				// the PSN-era AccountChange (which carries the proper
				// mandate name) wins via gold's latest-last_seen_at
				// upsert guard; our contribution just extends
				// first_seen_at back to the PDF era.
				ac.AccountKind = canonical.AccountKindSafekeeping
			} else {
				ac.AccountKind = canonical.AccountKindOverlay
				ac.DisplayName = silver.StrPtrIfNonEmpty("Portfolio overlay (historical)")
			}
			batch.Accounts = append(batch.Accounts, ac)
		}

		// Some historical rows carry a synthetic, non-ISIN-shaped
		// instrument key — e.g. the overview-derived precious-metals
		// position for a portfolio UBS issues no per-instrument
		// Statement-of-Assets page for ("PM-<portfolio>"). It still
		// needs a stable instrument/position identity, but must not
		// claim a canonical ISIN, so leave InstrumentChange.ISIN nil
		// for those.
		isinCopy := isin
		var isinPtr *string
		if looksLikeISIN(isin) {
			isinPtr = &isinCopy
		}
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: isin,
			AssetClass:           canonical.AssetClassOther,
			ISIN:                 isinPtr,
			Name:                 silver.StrPtrIfNonEmpty(descr.String),
			Currency:             silver.StrPtrIfNonEmpty(ccy),
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		})

		positionCcy := mvCcy
		if positionCcy == "" {
			positionCcy = ccy
		}
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    accountID,
			PositionKey:          isin,
			InstrumentExternalID: &isinCopy,
			AssetClass:           canonical.AssetClassOther,
			Currency:             positionCcy,
			Quantity:             decimalFromNullFloat(units),
			MarketValue:          decimalFromNullFloat(mv),
			BookValue:            bookValueFromUnitsCost(units, cost),
			AccruedInterest:      decimalFromNullFloat(accrued),
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// appendHistoricalCashBalances emits opening and closing balance
// rows from `historical_cash_balances`. One CashBalanceChange per
// non-NULL balance is produced — opening at period_start, closing
// at period_end. Per-IBAN AccountChange is emitted once per
// (period_end, IBAN) so accounts the PDFs reference but live web
// hasn't observed still appear in gold.
//
// total_debits / total_credits are kept in the payload but not
// projected to gold — they're monthly aggregates, not point-in-
// time balances, so they don't map onto cash_balances semantics.
func (r *webReader) appendHistoricalCashBalances(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
) error {
	const q = `
SELECT period_end, period_start, account_external_id, currency_iso,
       opening_balance, closing_balance, payload
  FROM historical_cash_balances
 WHERE period_end BETWEEN ? AND ?
    OR period_start BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalCashBalances: %w", err)
	}
	defer rows.Close()

	// One AccountChange per (period_end, account) is enough —
	// re-emitting at each period is harmless via the per-column
	// upsert, but we cap it to one per batch to keep the volume
	// down.
	accountEmitted := map[[2]int64]bool{}

	for rows.Next() {
		var (
			periodEnd, periodStart int64
			acctID, ccy            string
			open, close            sql.NullFloat64
			payload                string
		)
		if err := rows.Scan(&periodEnd, &periodStart, &acctID, &ccy,
			&open, &close, &payload); err != nil {
			return err
		}

		emitAccount := func(snap int64) {
			key := [2]int64{snap, int64(strHash(acctID))}
			if accountEmitted[key] {
				return
			}
			accountEmitted[key] = true
			c := ccy
			batch := getBatch(snap)
			batch.Accounts = append(batch.Accounts, canonical.AccountChange{
				AccountExternalID: acctID,
				AccountKind:       canonical.AccountKindCash,
				BaseCurrency:      &c,
				FirstSeenAt:       snap,
				LastSeenAt:        snap,
			})
		}

		if open.Valid && periodStart >= w.Start && periodStart <= w.End {
			emitAccount(periodStart)
			getBatch(periodStart).CashBalances = append(getBatch(periodStart).CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        periodStart,
				AccountExternalID: acctID,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindOpening,
				Amount:            canonical.NewDecimalFromFloat(open.Float64),
				Payload:           json.RawMessage(payload),
			})
		}
		if close.Valid && periodEnd >= w.Start && periodEnd <= w.End {
			emitAccount(periodEnd)
			getBatch(periodEnd).CashBalances = append(getBatch(periodEnd).CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        periodEnd,
				AccountExternalID: acctID,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            canonical.NewDecimalFromFloat(close.Float64),
				Payload:           json.RawMessage(payload),
			})
		}
	}
	return rows.Err()
}

// appendHistoricalMortgages attaches each PDF-derived mortgage
// balance point (one per Maturity Notice "As at" date) to the
// portfolio snapshot at that same as_of_date, emitting the same
// triple as the live path — Account + Instrument + Position keyed
// by the UBS-internal mortgage account number, AccountKind /
// AssetClass = mortgage, MarketValue already negative from silver.
//
// It takes `byTime` directly (not getBatch) and PEEKS rather than
// creates: a mortgage row is only emitted when a real portfolio
// snapshot already exists at its as_of_date (a Statement-of-Assets
// securities batch or an Account-Statement cash batch). Maturity
// notices and Statement-of-Assets PDFs are both quarter-end, so in
// the normal case they line up exactly. The one that wouldn't —
// UBS issues the next interest-roll quarter's Maturity Notice
// ahead of time, so there's a future-dated mortgage row with no
// portfolio snapshot behind it — must NOT spawn a mortgage-only
// snapshot, or gold's MAX(snapshot_at)-per-source "today" query
// lands on that future date and every other position vanishes
// from the view. Peeking (and skipping unanchored dates) prevents
// that. Mortgages must therefore run AFTER securities + cash in
// snapshotsHistorical so the batches they anchor to already exist.
func (r *webReader) appendHistoricalMortgages(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
) error {
	ok, err := r.hasHistoricalMortgagesTable(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	const q = `
SELECT as_of_date, account_external_id, currency_iso,
       outstanding_balance, product_name, rate_type,
       collateral_description, payload
  FROM historical_mortgages
 WHERE as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalMortgages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			asOf                              int64
			extID, currency, payload          string
			outstanding                       sql.NullFloat64
			productName, rateType, collateral sql.NullString
		)
		if err := rows.Scan(&asOf, &extID, &currency, &outstanding,
			&productName, &rateType, &collateral, &payload); err != nil {
			return err
		}
		_ = rateType // surfaced via payload

		// Peek — never create. Skip mortgage rows whose as_of_date
		// has no real portfolio snapshot behind it (e.g. the
		// future-dated next-quarter Maturity Notice).
		batch, ok := byTime[asOf]
		if !ok || (len(batch.Positions) == 0 && len(batch.CashBalances) == 0) {
			continue
		}

		display := sql.NullString{
			Valid:  productName.Valid && collateral.Valid,
			String: productName.String + ", " + collateral.String,
		}
		if !display.Valid {
			display = productName
		}
		extIDCopy := extID
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindMortgage,
			DisplayName:       silver.StrPtrIfNonEmpty(display.String),
			BaseCurrency:      silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:       asOf,
			LastSeenAt:        asOf,
			Payload:           json.RawMessage(payload),
		})
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: extID,
			AssetClass:           canonical.AssetClassMortgage,
			Name:                 silver.StrPtrIfNonEmpty(display.String),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		})
		var mv *canonical.Decimal
		if outstanding.Valid {
			d := canonical.NewDecimalFromFloat(outstanding.Float64)
			mv = &d
		}
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    extID,
			PositionKey:          extID,
			InstrumentExternalID: &extIDCopy,
			AssetClass:           canonical.AssetClassMortgage,
			Currency:             currency,
			MarketValue:          mv,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// hasHistoricalMortgagesTable returns true if the silver carries
// the migration-0005 historical_mortgages table. Older silvers
// loaded before that migration just return false; the projection
// is skipped silently.
func (r *webReader) hasHistoricalMortgagesTable(ctx context.Context) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM sqlite_master
         WHERE type = 'table' AND name = 'historical_mortgages'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasHistoricalMortgagesTable: %w", err)
	}
	return n > 0, nil
}

// historicalRange returns MIN/MAX as_of_date across the historical
// tables. Both values are -1 when the silver carries no historical
// rows. Used by ChangeWindow to extend Start backwards so the
// loader's window-DELETE covers existing historical rows before
// the re-INSERT.
func (r *webReader) historicalRange(ctx context.Context) (int64, int64, error) {
	var (
		posMin, posMax   sql.NullInt64
		cashMin, cashMax sql.NullInt64
		mortMin, mortMax sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`,
	).Scan(&posMin, &posMax); err != nil {
		return -1, -1, fmt.Errorf("historicalRange positions: %w", err)
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(period_start), MAX(period_end) FROM historical_cash_balances`,
	).Scan(&cashMin, &cashMax); err != nil {
		return -1, -1, fmt.Errorf("historicalRange cash: %w", err)
	}
	ok, err := r.hasHistoricalMortgagesTable(ctx)
	if err != nil {
		return -1, -1, err
	}
	if ok {
		if err := r.db.QueryRowContext(ctx,
			`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_mortgages`,
		).Scan(&mortMin, &mortMax); err != nil {
			return -1, -1, fmt.Errorf("historicalRange mortgages: %w", err)
		}
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
	merge(mortMin)
	merge(mortMax)
	return lo, hi, nil
}

// hasHistoricalTables reports whether the ubs-web silver carries
// the migration-0002 historical tables. Older silvers (rebuilt
// against migration 0001 only) won't have them, and the adapter
// must still load — falling back to the live-only stream.
func (r *webReader) hasHistoricalTables(ctx context.Context) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
 WHERE type = 'table'
   AND name IN ('historical_position_snapshots', 'historical_cash_balances')`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasHistoricalTables: %w", err)
	}
	return n == 2, nil
}

// strHash is FNV-1a 64-bit. Used to compress string keys into the
// dedup maps so we can index on (int64, int64) tuples rather than
// allocating a `map[struct{ts int64; s string}]` per call. The
// hash collision risk for ~thousands of account / portfolio IDs
// is negligible.
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

func sortInt64Asc(xs []int64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func decimalFromNullFloat(n sql.NullFloat64) *canonical.Decimal {
	if !n.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(n.Float64)
	return &d
}

func bookValueFromUnitsCost(units, cost sql.NullFloat64) *canonical.Decimal {
	if !units.Valid || !cost.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(units.Float64 * cost.Float64)
	return &d
}
