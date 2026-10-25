package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots collects every snapshot-grain row in the change window
// from accounts, account_balances, and positions, splits each
// silver row into the right canonical record type, and groups
// them by snapshot_at so the caller can apply one batch per
// snapshot.
func (c *apiReader) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}

	// Collect distinct snapshot_at values in the window so we can
	// build batches in chronological order. We use dump_runs as
	// the authoritative list — every silver row's snapshot_at must
	// correspond to a dump_runs entry.
	snapshotTimes, err := dumpTimesInWindow(ctx, c.db, w)
	if err != nil {
		return nil, fmt.Errorf("schwab api: %w", err)
	}

	// One batch per snapshot_at. The map gives O(1) routing as we
	// walk each table's rows.
	byTime := silver.NewBatches(snapshotTimes)

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
	if err := c.appendPositions(ctx, byTime, instrumentNames); err != nil {
		return nil, err
	}
	if err := c.appendInstrumentBonds(ctx, w, byTime); err != nil {
		return nil, err
	}

	return byTime.Stream(), nil
}

// appendInstrumentBonds adds an InstrumentChange for each bond the
// `instruments` table describes in the window: its name and its terms
// (bondTermsPayload), whether or not a position holds it. The table is
// what the --with-instruments mode reads of the bonds a transaction or a
// statement names, which the positions may no longer hold; its key is
// the bond's CUSIP, as a position's and a statement's are. Gold's upsert
// fills the name and the payload a statement's row of the same bond
// leaves empty.
func (c *apiReader) appendInstrumentBonds(ctx context.Context, w canonical.Window, byTime silver.Batches) error {
	exists, err := silver.HasTables(ctx, c.db, "instruments")
	if err != nil || !exists {
		return err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT snapshot_at, symbol, payload FROM instruments
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at, symbol`, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendInstrumentBonds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var snap int64
		var symbol, payload string
		if err := rows.Scan(&snap, &symbol, &payload); err != nil {
			return fmt.Errorf("appendInstrumentBonds scan: %w", err)
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var i schwabInstrument
		if json.Unmarshal([]byte(payload), &i) != nil {
			continue
		}
		acNew, vehicle := taxonomyFor(i.AssetType, i.Type, i.Description)
		if vehicle != canonical.VehicleBond {
			continue
		}
		key := preferredInstrumentKey(i)
		if key == "" {
			key = symbol
		}
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: key,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			CUSIP:                silver.StrPtrIfNonEmpty(i.CUSIP),
			Symbol:               silver.StrPtrIfNonEmpty(symbol),
			Name:                 silver.StrPtrIfNonEmpty(i.Description),
			Currency:             silver.StrPtrIfNonEmpty("USD"),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              bondTermsPayload(vehicle, i),
		})
	}
	return rows.Err()
}

// dumpTimesInWindow returns the distinct dump_runs.snapshot_at values
// of a silver (either feed) in [w.Start, w.End], in chronological order.
func dumpTimesInWindow(ctx context.Context, db *sql.DB, w canonical.Window) ([]int64, error) {
	const q = `
SELECT snapshot_at FROM dump_runs
 WHERE snapshot_at BETWEEN ? AND ?
 ORDER BY snapshot_at`
	rows, err := db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("dump_runs in window: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("dump_runs in window scan: %w", err)
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
// management wrapper category, so it is filled in via the
// config-side override.
func (c *apiReader) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	hasNickname, err := silver.HasColumn(ctx, c.db, "accounts", "nickname")
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
			DisplayName:       silver.StrPtrIfNonEmpty(p.AccountNumber),
			BaseCurrency:      silver.StrPtrIfNonEmpty("USD"),
			Nickname:          silver.StrPtrIfNonEmpty(nickname.String),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// schwabBalancePayload covers the field read from account_balances
// rows. Schwab's full balance object has many fields; only the cash
// component reaches the gold cash_balances table.
type schwabBalancePayload struct {
	CashBalance *canonical.Decimal `json:"cashBalance"`
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
	Type        string `json:"type"`
	CUSIP       string `json:"cusip"`
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
	// MaturityDate is a bond's, as the API stamps it:
	// 2099-09-09T05:00:00.000+00:00. VariableRate is its coupon, in
	// percent: the schwab-api collector reads it so.
	MaturityDate string   `json:"maturityDate"`
	VariableRate *float64 `json:"variableRate"`
}

// schwabPositionPayload covers the fields we extract from each
// positions row.
type schwabPositionPayload struct {
	LongQuantity  canonical.Decimal  `json:"longQuantity"`
	ShortQuantity canonical.Decimal  `json:"shortQuantity"`
	AveragePrice  *canonical.Decimal `json:"averagePrice"`
	MarketValue   *canonical.Decimal `json:"marketValue"`
	Instrument    schwabInstrument   `json:"instrument"`
}

// appendPositions emits InstrumentChange + PositionChange for each
// non-cash position, or CashBalanceChange for CASH_EQUIVALENT and
// CURRENCY positions. See docs/adapters/schwab.md §4. The
// optional instrumentNames map (latest-known per symbol from the
// silver `instruments` table; empty when --with-instruments was
// never used) fills in InstrumentChange.Name when Schwab's per-
// position instrument descriptor has no description (typical for
// EQUITY rows out of /accounts). A position's book value is its
// market value less its open P/L (apiBookValue).
//
// The mark guard (marks.go) reads every position in snapshot order, so a
// spike reads the same in a load of the window as in a load of the whole
// history. A spiked observation's value is at the previous mark, its
// valued_at is the snapshot that mark is from, and its payload keeps
// Schwab's value as `stated_market_value`. Its book value comes from
// Schwab's value, which the open P/L is measured against.
func (c *apiReader) appendPositions(ctx context.Context, byTime map[int64]*canonical.SnapshotBatch, instrumentNames map[string]string) error {
	// Silver migration 0005 promoted the open P/L; an older silver
	// states none, and its positions carry no book value.
	hasOpenPL, err := silver.HasColumn(ctx, c.db, "positions", "unrealized_gain_loss")
	if err != nil {
		return err
	}
	openPLCol := "NULL"
	if hasOpenPL {
		openPLCol = "unrealized_gain_loss"
	}
	q := fmt.Sprintf(`
SELECT snapshot_at, account_external_id, instrument_key, payload, %s
  FROM positions
 ORDER BY snapshot_at, account_external_id, instrument_key`, openPLCol)
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()

	// The first pass reads every position and feeds the mark guard; the
	// second emits the window's rows with the guard's fixes.
	type apiPosition struct {
		snap          int64
		extID, posKey string
		payload       string
		openPL        sql.NullFloat64
		pp            schwabPositionPayload
		cash          bool
		assetClass    canonical.AssetClass
		vehicle       canonical.Vehicle
		name          string
		quantity      canonical.Decimal
	}
	var read []apiPosition
	guard := newMarkGuard(c.marks)
	for rows.Next() {
		var r apiPosition
		if err := rows.Scan(&r.snap, &r.extID, &r.posKey, &r.payload, &r.openPL); err != nil {
			return fmt.Errorf("appendPositions scan: %w", err)
		}
		if err := json.Unmarshal([]byte(r.payload), &r.pp); err != nil {
			return fmt.Errorf("appendPositions row (snap=%d, key=%s): %w", r.snap, r.posKey, err)
		}
		guard.snapshot(r.snap)
		// Cash-like assetType → cash_balances, not positions.
		r.cash = isCashAssetType(r.pp.Instrument.AssetType)
		if !r.cash {
			r.name = r.pp.Instrument.Description
			if r.name == "" && r.pp.Instrument.Symbol != "" {
				r.name = instrumentNames[r.pp.Instrument.Symbol]
			}
			// taxonomyFor maps the assetType/type signal to the single
			// (exposure, vehicle) pair emitted to gold; name feeds the
			// ETF/fund exposure refinement.
			r.assetClass, r.vehicle = taxonomyFor(r.pp.Instrument.AssetType, r.pp.Instrument.Type, r.name)
			r.quantity = r.pp.LongQuantity.Sub(r.pp.ShortQuantity)
			if guardsMarks(r.vehicle) {
				guard.observe(markSeries{r.extID, r.posKey}, r.snap, r.quantity, r.pp.MarketValue)
			}
		}
		read = append(read, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fixes := guard.fixes()

	for _, r := range read {
		// byTime holds the window's snapshots only.
		batch, inWindow := byTime[r.snap]
		if !inWindow {
			continue
		}
		if r.cash {
			amount := canonical.Decimal{}
			if r.pp.MarketValue != nil {
				amount = *r.pp.MarketValue
			}
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        r.snap,
				AccountExternalID: r.extID,
				Currency:          "USD",
				BalanceKind:       canonical.BalanceKindCurrent,
				Amount:            amount,
				Payload:           json.RawMessage(r.payload),
			})
			continue
		}

		// Real security position.
		instrExtID := r.posKey
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrExtID,
			AssetClass:           r.assetClass,
			Vehicle:              r.vehicle,
			CUSIP:                silver.StrPtrIfNonEmpty(r.pp.Instrument.CUSIP),
			Symbol:               silver.StrPtrIfNonEmpty(r.pp.Instrument.Symbol),
			Name:                 silver.StrPtrIfNonEmpty(r.name),
			Currency:             silver.StrPtrIfNonEmpty("USD"),
			FirstSeenAt:          r.snap,
			LastSeenAt:           r.snap,
			Payload:              bondTermsPayload(r.vehicle, r.pp.Instrument),
		})

		quantity := r.quantity
		value, valuedAt := r.pp.MarketValue, int64(0)
		if f, ok := fixes[markSeries{r.extID, r.posKey}][r.snap]; ok {
			value, valuedAt = &f.value, f.from
		}
		pos := canonical.PositionChange{
			SnapshotAt:           r.snap,
			ValuedAt:             valuedAt,
			AccountExternalID:    r.extID,
			PositionKey:          r.posKey,
			InstrumentExternalID: &instrExtID,
			AssetClass:           r.assetClass,
			Vehicle:              r.vehicle,
			Currency:             "USD",
			Quantity:             &quantity,
			MarketValue:          value,
			Payload:              json.RawMessage(r.payload),
		}
		if valuedAt != 0 {
			pos.Payload = silver.PayloadWith(r.payload, map[string]any{
				"stated_market_value": r.pp.MarketValue.String()})
		}
		pos.SetBookValue(apiBookValue(r.pp, r.openPL), derivedBasis)
		batch.Positions = append(batch.Positions, pos)
	}
	return nil
}

// apiBookValue is a position's cost basis from its market value and the
// open P/L silver promotes beside it: the long side's P/L, or the short
// side's when the position is short. A row long and short at once
// states the P/L of its short side only, which says nothing of the
// whole; it carries no basis.
func apiBookValue(pp schwabPositionPayload, openPL sql.NullFloat64) *canonical.Decimal {
	long, short := pp.LongQuantity.IsPositive(), pp.ShortQuantity.IsPositive()
	if long && short {
		return nil
	}
	return basisFromOpenPL(pp.MarketValue, openPL, short)
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
	exists, err := silver.HasTables(ctx, c.db, "instruments")
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
