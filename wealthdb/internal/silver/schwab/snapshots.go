package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// snapshotStream yields one canonical.SnapshotBatch per
// silver snapshot_at in the change window. Loads the whole window
// up front; at personal-portfolio scale (≲50 positions × ≲10
// new snapshots per load), this stays trivially in memory.
type snapshotStream struct {
	batches []canonical.SnapshotBatch
	idx     int
}

// Snapshots collects every snapshot-grain row in the change window
// from accounts, account_balances, and positions, splits each
// silver row into the right canonical record type, and groups
// them by snapshot_at so the caller can apply one batch per
// snapshot.
func (c *apiReader) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return &snapshotStream{}, nil
	}

	// Collect distinct snapshot_at values in the window so we can
	// build batches in chronological order. We use dump_runs as
	// the authoritative list — every silver row's snapshot_at must
	// correspond to a dump_runs entry.
	snapshotTimes, err := c.snapshotTimesInWindow(ctx, w)
	if err != nil {
		return nil, err
	}

	// One batch per snapshot_at. The map gives O(1) routing as we
	// walk each table's rows.
	byTime := make(map[int64]*canonical.SnapshotBatch, len(snapshotTimes))
	for _, t := range snapshotTimes {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendAccountBalances(ctx, w, byTime); err != nil {
		return nil, err
	}
	// instrumentNames is consulted as a fallback by appendPositions
	// when the per-position instrument descriptor lacks a
	// description (common for EQUITY rows from /accounts). The
	// schwab-api --with-instruments mode populates a separate
	// instruments table that we treat as the authoritative source
	// for symbol → human-readable name.
	instrumentNames, err := c.latestKnownInstrumentNames(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime, instrumentNames); err != nil {
		return nil, err
	}

	out := &snapshotStream{batches: make([]canonical.SnapshotBatch, 0, len(snapshotTimes))}
	for _, t := range snapshotTimes {
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

// snapshotTimesInWindow returns the distinct dump_runs.snapshot_at
// values in [w.Start, w.End], in chronological order.
func (c *apiReader) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT snapshot_at FROM dump_runs
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("snapshotTimesInWindow: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("snapshotTimesInWindow scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// schwabAccountPayload covers the {accountNumber, hashValue}
// shape Schwab silver writes. account_external_id is the hash;
// the human-readable account number lives in payload only.
type schwabAccountPayload struct {
	AccountNumber string `json:"accountNumber"`
}

// appendAccounts emits one AccountChange per accounts row in the
// window. Schwab accounts are brokerage-kind; the silver
// account_external_id is the Schwab hashValue. DisplayName is set
// to the plaintext accountNumber from the payload so user-facing
// output can show something more recognisable than the hash. The
// optional `nickname` column (schwab silver v3+) carries the
// user-set label from /userPreference; we forward it as Nickname.
// AccountCategory stays nil — Schwab's `account_type` is CASH or
// MARGIN, which is margin enablement rather than a wealth-
// management wrapper category, so the user fills it in via the
// config-side override.
func (c *apiReader) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	hasNickname, err := c.hasColumn(ctx, "accounts", "nickname")
	if err != nil {
		return err
	}
	q := `SELECT snapshot_at, account_external_id, payload, NULL FROM accounts WHERE snapshot_at BETWEEN ? AND ?`
	if hasNickname {
		q = `SELECT snapshot_at, account_external_id, payload, nickname FROM accounts WHERE snapshot_at BETWEEN ? AND ?`
	}
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap     int64
			extID    string
			payload  string
			nickname sql.NullString
		)
		if err := rows.Scan(&snap, &extID, &payload, &nickname); err != nil {
			return fmt.Errorf("appendAccounts scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var p schwabAccountPayload
		_ = json.Unmarshal([]byte(payload), &p) // best-effort

		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			DisplayName:       strPtrIfNonEmpty(p.AccountNumber),
			BaseCurrency:      strPtrIfNonEmpty("USD"),
			Nickname:          nullStringPtr(nickname),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// schwabBalancePayload covers the fields we currently extract
// from account_balances rows. Schwab's full balance object has
// many fields; we only need the cash component for the gold
// cash_balances table.
type schwabBalancePayload struct {
	CashBalance *canonical.Decimal `json:"cashBalance"`
	// Some balance subtypes nest the value differently; we expand
	// this struct as new shapes appear.
}

// appendAccountBalances emits CashBalanceChange rows from
// account_balances. Each silver row maps to at most one canonical
// row: a balance payload without a cashBalance field is skipped
// (no useful cash quantity to project).
func (c *apiReader) appendAccountBalances(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, balance_kind, payload
  FROM account_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccountBalances: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap        int64
			extID, kind string
			payload     string
		)
		if err := rows.Scan(&snap, &extID, &kind, &payload); err != nil {
			return fmt.Errorf("appendAccountBalances scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var bp schwabBalancePayload
		if err := json.Unmarshal([]byte(payload), &bp); err != nil {
			return fmt.Errorf("appendAccountBalances row (snap=%d, kind=%s): %w", snap, kind, err)
		}
		if bp.CashBalance == nil {
			continue
		}

		bk := canonicalBalanceKind(kind)
		if !bk.Valid() {
			// Unknown silver balance_kind; skip rather than emit a
			// row that would fail the gold-side enum guard.
			continue
		}
		batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
			SnapshotAt:        snap,
			AccountExternalID: extID,
			Currency:          "USD", // Schwab retail is USD-only.
			BalanceKind:       bk,
			Amount:            *bp.CashBalance,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

func canonicalBalanceKind(silverKind string) canonical.BalanceKind {
	switch silverKind {
	case "initial":
		return canonical.BalanceKindInitial
	case "current":
		return canonical.BalanceKindCurrent
	case "projected":
		return canonical.BalanceKindProjected
	case "aggregated":
		return canonical.BalanceKindAggregated
	default:
		return ""
	}
}

// schwabInstrument is the embedded instrument descriptor that
// appears under both positions and transactions[].transferItems[].
type schwabInstrument struct {
	AssetType   string `json:"assetType"`
	CUSIP       string `json:"cusip"`
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
}

// schwabPositionPayload covers the fields we extract from each
// positions row.
type schwabPositionPayload struct {
	LongQuantity    canonical.Decimal  `json:"longQuantity"`
	ShortQuantity   canonical.Decimal  `json:"shortQuantity"`
	AveragePrice    *canonical.Decimal `json:"averagePrice"`
	MarketValue     *canonical.Decimal `json:"marketValue"`
	Instrument      schwabInstrument   `json:"instrument"`
}

// appendPositions emits InstrumentChange + PositionChange for each
// non-cash position, or CashBalanceChange for CASH_EQUIVALENT and
// CURRENCY positions. See docs/adapters/schwab.md §4. The
// optional instrumentNames map (latest-known per symbol from the
// silver `instruments` table; empty when --with-instruments was
// never used) fills in InstrumentChange.Name when Schwab's per-
// position instrument descriptor has no description (typical for
// EQUITY rows out of /accounts).
func (c *apiReader) appendPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch, instrumentNames map[string]string) error {
	const q = `
SELECT snapshot_at, account_external_id, instrument_key, payload
  FROM positions
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			snap          int64
			extID, posKey string
			payload       string
		)
		if err := rows.Scan(&snap, &extID, &posKey, &payload); err != nil {
			return fmt.Errorf("appendPositions scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}

		var pp schwabPositionPayload
		if err := json.Unmarshal([]byte(payload), &pp); err != nil {
			return fmt.Errorf("appendPositions row (snap=%d, key=%s): %w", snap, posKey, err)
		}

		// Cash-like assetType → cash_balances, not positions.
		if isCashAssetType(pp.Instrument.AssetType) {
			amount := canonical.Decimal{}
			if pp.MarketValue != nil {
				amount = *pp.MarketValue
			}
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: extID,
				Currency:          "USD",
				BalanceKind:       canonical.BalanceKindCurrent,
				Amount:            amount,
				Payload:           json.RawMessage(payload),
			})
			continue
		}

		// Real security position.
		instrExtID := posKey
		ac := assetClassFor(pp.Instrument.AssetType)

		name := pp.Instrument.Description
		if name == "" && pp.Instrument.Symbol != "" {
			name = instrumentNames[pp.Instrument.Symbol]
		}
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrExtID,
			AssetClass:           ac,
			CUSIP:                strPtrIfNonEmpty(pp.Instrument.CUSIP),
			Symbol:               strPtrIfNonEmpty(pp.Instrument.Symbol),
			Name:                 strPtrIfNonEmpty(name),
			Currency:             strPtrIfNonEmpty("USD"),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
		})

		quantity := pp.LongQuantity.Sub(pp.ShortQuantity)
		instrExtIDPtr := &instrExtID
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    extID,
			PositionKey:          posKey,
			InstrumentExternalID: instrExtIDPtr,
			AssetClass:           ac,
			Currency:             "USD",
			Quantity:             &quantity,
			MarketValue:          pp.MarketValue,
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// strPtrIfNonEmpty returns a *string to s, or nil when s is empty.
// Convenient for converting JSON-decoded strings (which default to
// "") into the nullable shape canonical types use.
func strPtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullStringPtr converts a sql.NullString to *string, returning
// nil for both SQL NULL and the empty string (the latter is
// indistinguishable from "unset" for free-text label columns).
func nullStringPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	s := n.String
	return &s
}

// hasColumn reports whether table contains a column with the given
// name. SQLite-only; uses PRAGMA table_info via a query rather
// than a Pragma helper so it works through database/sql.
func (c *apiReader) hasColumn(ctx context.Context, table, column string) (bool, error) {
	// PRAGMA table_info doesn't accept parameter binding, so the
	// caller must pass a trusted table name. Both call sites here
	// pass string literals.
	rows, err := c.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("hasColumn(%s.%s): %w", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid                                      int
			name, ctype                              string
			notnull, pk                              int
			dfltValue                                sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("hasColumn scan: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// hasTable reports whether the silver SQLite contains a table of
// the given name.
func (c *apiReader) hasTable(ctx context.Context, table string) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`,
		table,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("hasTable(%s): %w", table, err)
	}
	return n > 0, nil
}

// latestKnownInstrumentNames returns symbol → human-readable name
// from the `instruments` table, picking the row with the highest
// snapshot_at for each symbol. Returns an empty (non-nil) map when
// the silver doesn't have the table at all (older schwab-api,
// or --with-instruments never used). "Latest as of now" rather than
// "latest as of snapshot": the cross-bank schema doesn't preserve
// per-snapshot instrument descriptions, so callers get the freshest
// label we know about for that symbol.
func (c *apiReader) latestKnownInstrumentNames(ctx context.Context) (map[string]string, error) {
	exists, err := c.hasTable(ctx, "instruments")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	if !exists {
		return out, nil
	}
	const q = `
SELECT i.symbol, i.payload
  FROM instruments i
  JOIN (SELECT symbol, MAX(snapshot_at) AS max_snap
          FROM instruments
         GROUP BY symbol) m
    ON i.symbol = m.symbol
   AND i.snapshot_at = m.max_snap`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("latestKnownInstrumentNames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sym, payload string
		if err := rows.Scan(&sym, &payload); err != nil {
			return nil, fmt.Errorf("latestKnownInstrumentNames scan: %w", err)
		}
		var p struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		if p.Description != "" {
			out[sym] = p.Description
		}
	}
	return out, rows.Err()
}

