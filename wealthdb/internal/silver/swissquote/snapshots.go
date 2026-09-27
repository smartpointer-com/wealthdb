package swissquote

import (
	"context"
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
	byTime := make(map[int64]*canonical.SnapshotBatch, len(times))
	for _, t := range times {
		byTime[t] = &canonical.SnapshotBatch{}
	}

	if err := c.appendAccounts(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendPositions(ctx, w, byTime); err != nil {
		return nil, err
	}
	if err := c.appendCurrencyBalancesAndFxRates(ctx, w, byTime); err != nil {
		return nil, err
	}

	out := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		out = append(out, *byTime[t])
	}
	return silver.NewSnapshotStream(out), nil
}

// snapshotTimesInWindow returns the union of distinct snapshot_at
// values across dump_runs, positions, and currency_balances —
// every silver table whose `snapshot_at` carries semantic
// meaning. Historical positions reconstructed from Portfolio
// Performance PDFs (silver migration 0004, source='pp:<doc_id>')
// land with snapshot_at = the PDF's effective as-of date rather
// than the dump's run time, so they're absent from dump_runs but
// present in positions. UNION'ing them keeps the byTime dispatch
// in Snapshots() aware of every snapshot the adapter is about to
// emit a batch for.
func (c *Connection) snapshotTimesInWindow(ctx context.Context, w canonical.Window) ([]int64, error) {
	const q = `
SELECT DISTINCT snapshot_at FROM (
    SELECT snapshot_at FROM dump_runs         WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM positions         WHERE snapshot_at BETWEEN ? AND ?
    UNION ALL
    SELECT snapshot_at FROM currency_balances WHERE snapshot_at BETWEEN ? AND ?
)
ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End, w.Start, w.End, w.Start, w.End)
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

// ---- accounts ------------------------------------------------------------

// appendAccounts emits one AccountChange per silver accounts row,
// promoting the silver `account_product` discriminator (or
// `account_type` on pre-v5 silvers) into both AccountCategory
// (free-text descriptor, preserved verbatim) and TaxWrapper
// (canonical enum, mapped per the table documented in the
// swissquote README "Gold-layer integration" section):
//
//	Trading / Savings      → taxable_personal (default)
//	Säule 3a               → pillar_3a
//	Freizügigkeit          → vested_benefits
//	anything else / empty  → leave TaxWrapper nil so the config-
//	                         side override or the COALESCE upsert
//	                         can supply a value.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	// Silver v5 renamed `account_type` to `account_product`. Read
	// whichever exists so older silvers (pre-v5) still load until
	// the silver loader is re-run.
	productCol := ""
	if has, err := silver.HasColumn(ctx, c.db, "accounts", "account_product"); err != nil {
		return err
	} else if has {
		productCol = "account_product"
	} else if has, err := silver.HasColumn(ctx, c.db, "accounts", "account_type"); err != nil {
		return err
	} else if has {
		productCol = "account_type"
	}

	q := `SELECT snapshot_at, account_external_id, payload, '' FROM accounts WHERE snapshot_at BETWEEN ? AND ?`
	if productCol != "" {
		q = fmt.Sprintf(`SELECT snapshot_at, account_external_id, payload, COALESCE(%s, '') FROM accounts WHERE snapshot_at BETWEEN ? AND ?`, productCol)
	}
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendAccounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap    int64
			extID   string
			payload string
			product string
		)
		if err := rows.Scan(&snap, &extID, &payload, &product); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		change := canonical.AccountChange{
			AccountExternalID: extID,
			AccountKind:       canonical.AccountKindBrokerage,
			AccountCategory:   silver.StrPtrIfNonEmpty(product),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		}
		if w := taxWrapperFor(product); w != "" {
			change.TaxWrapper = &w
		}
		batch.Accounts = append(batch.Accounts, change)
	}
	return rows.Err()
}

// ---- positions -----------------------------------------------------------

// positionPayload mirrors the Swissquote position payload fields
// the silver loader extracts. Two shapes are handled:
//
//	live (XLS export):   quantity, price, unit_cost, total_value,
//	                     total_value_chf
//	historical (PDF):    quantity, market_price, avg_price,
//	                     valuation_chf  (no `total_value`)
//
// market_value resolution: prefer total_value; fall back to
// valuation_chf when the row's currency is CHF (the historical
// positions here are all CHF); else compute from
// quantity × market_price as a last resort.
type positionPayload struct {
	AssetClass   string             `json:"asset_class"`
	Currency     string             `json:"currency"`
	Symbol       string             `json:"symbol"`
	Quantity     *canonical.Decimal `json:"quantity"`
	Price        *canonical.Decimal `json:"price"`
	MarketPrice  *canonical.Decimal `json:"market_price"`
	UnitCost     *canonical.Decimal `json:"unit_cost"`
	TotalValue   *canonical.Decimal `json:"total_value"`
	ValuationCHF *canonical.Decimal `json:"valuation_chf"`
}

// effectiveMarketValue resolves market_value across the two
// payload shapes Swissquote silver produces. See positionPayload.
func (p *positionPayload) effectiveMarketValue() *canonical.Decimal {
	if p.TotalValue != nil {
		return p.TotalValue
	}
	if p.Currency == "CHF" && p.ValuationCHF != nil {
		return p.ValuationCHF
	}
	if p.Quantity != nil && p.MarketPrice != nil {
		v := p.Quantity.Mul(*p.MarketPrice)
		return &v
	}
	return nil
}

// appendPositions emits one PositionChange and one
// InstrumentChange per positions row. The silver `positions`
// table mixes two provenances (migration 0004 `source` column):
//
//	`live`        — current Portfolio Overview XLS export
//	`pp:<doc_id>` — reconstructed from a Portfolio Performance
//	                PDF, with snapshot_at = the PDF's as-of date
//
// `name` and `isin` were promoted in silver migration 0003
// (scraped from the Portfolio Overview DOM tooltip and FullQuote
// link href respectively). Both are nullable — pre-migration
// rows leave them NULL — and the adapter falls back via
// hasColumn so older silvers still load.
//
// Identity contract:
//   - instrument_external_id / position_key = ISIN when known
//     for ANY row sharing the same `(symbol, currency)` tuple,
//     else `symbol + '@' + currency`. The fallback chain is
//     resolved once via isinBySymbol() so pre-migration live
//     rows that lack a column-level ISIN still pick up the ISIN
//     observed in a later live or historical row. This keeps
//     positions for one instrument unified across the silver
//     migration 0003 boundary AND across the live/historical
//     boundary, where the historical PDF stores the instrument's
//     long name in `symbol` while live XLS stores the ticker.
//   - When no ISIN is reachable for a symbol (Swissquote never
//     surfaced one), the symbol@currency fallback is the stable
//     per-bank identifier and gold's ix_instruments_isin is just
//     unused for that row.
func (c *Connection) appendPositions(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	hasName, err := silver.HasColumn(ctx, c.db, "positions", "name")
	if err != nil {
		return err
	}
	hasISIN, err := silver.HasColumn(ctx, c.db, "positions", "isin")
	if err != nil {
		return err
	}
	isinBySymbol, err := c.buildISINBySymbol(ctx, hasISIN)
	if err != nil {
		return err
	}

	q := `SELECT snapshot_at, account_external_id, symbol, currency, payload, '', '' FROM positions WHERE snapshot_at BETWEEN ? AND ?`
	switch {
	case hasName && hasISIN:
		q = `SELECT snapshot_at, account_external_id, symbol, currency, payload, COALESCE(name, ''), COALESCE(isin, '') FROM positions WHERE snapshot_at BETWEEN ? AND ?`
	case hasName:
		q = `SELECT snapshot_at, account_external_id, symbol, currency, payload, COALESCE(name, ''), '' FROM positions WHERE snapshot_at BETWEEN ? AND ?`
	case hasISIN:
		q = `SELECT snapshot_at, account_external_id, symbol, currency, payload, '', COALESCE(isin, '') FROM positions WHERE snapshot_at BETWEEN ? AND ?`
	}
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendPositions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap                    int64
			extID, symbol, currency string
			payload                 string
			name, isin              string
		)
		if err := rows.Scan(&snap, &extID, &symbol, &currency, &payload, &name, &isin); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p positionPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("appendPositions row (snap=%d, %s@%s): %w", snap, symbol, currency, err)
		}
		// Map the section header + security name to the single
		// (exposure, vehicle) pair emitted to gold.
		acNew, vehicle := taxonomyFor(p.AssetClass, name)

		effectiveISIN := isin
		if effectiveISIN == "" {
			effectiveISIN = isinBySymbol[symbol+"@"+currency]
		}
		var positionKey string
		if effectiveISIN != "" {
			positionKey = effectiveISIN
		} else {
			positionKey = symbol + "@" + currency
		}

		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: positionKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			ISIN:                 silver.StrPtrIfNonEmpty(effectiveISIN),
			Symbol:               silver.StrPtrIfNonEmpty(symbol),
			Name:                 silver.StrPtrIfNonEmpty(name),
			Currency:             silver.StrPtrIfNonEmpty(currency),
			FirstSeenAt:          snap,
			LastSeenAt:           snap,
			Payload:              json.RawMessage(payload),
		})

		instrIDCopy := positionKey
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           snap,
			AccountExternalID:    extID,
			PositionKey:          positionKey,
			InstrumentExternalID: &instrIDCopy,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             currency,
			Quantity:             p.Quantity,
			MarketValue:          p.effectiveMarketValue(),
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// buildISINBySymbol returns a `symbol + '@' + currency` → ISIN
// lookup built from every positions row that carries an ISIN
// (including historical PDF rows added in silver migration 0004).
// Used by appendPositions to give pre-migration-0003 rows that
// lack a column-level ISIN the same ISIN-keyed position_key as
// the post-migration rows for the same logical instrument. Empty
// map when the silver has no `isin` column at all.
func (c *Connection) buildISINBySymbol(ctx context.Context, hasISIN bool) (map[string]string, error) {
	out := map[string]string{}
	if !hasISIN {
		return out, nil
	}
	const q = `
SELECT symbol, currency, isin
  FROM positions
 WHERE isin IS NOT NULL AND isin != ''
 ORDER BY snapshot_at`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("buildISINBySymbol: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var symbol, currency, isin string
		if err := rows.Scan(&symbol, &currency, &isin); err != nil {
			return nil, err
		}
		out[symbol+"@"+currency] = isin
	}
	return out, rows.Err()
}

// ---- currency_balances → CashBalance + FxRate ----------------------------

type currencyBalancePayload struct {
	CashBalance *canonical.Decimal `json:"cash_balance"`
	RateToCHF   *canonical.Decimal `json:"rate_to_chf"`
}

// appendCurrencyBalancesAndFxRates does double duty per
// docs/adapters/swissquote.md §5: each silver currency_balances
// row produces one CashBalanceChange (the cash component in the
// row's currency) AND — for non-CHF rows — one FxRateChange
// (base=CHF, quote=row's currency, mid_rate=rate_to_chf).
func (c *Connection) appendCurrencyBalancesAndFxRates(ctx context.Context, w canonical.Window, byTime map[int64]*canonical.SnapshotBatch) error {
	const q = `
SELECT snapshot_at, account_external_id, currency, payload
  FROM currency_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendCurrencyBalancesAndFxRates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap            int64
			extID, currency string
			payload         string
		)
		if err := rows.Scan(&snap, &extID, &currency, &payload); err != nil {
			return err
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		var p currencyBalancePayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return fmt.Errorf("currency_balances payload (snap=%d, ccy=%s): %w", snap, currency, err)
		}

		if p.CashBalance != nil {
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        snap,
				AccountExternalID: extID,
				Currency:          currency,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            *p.CashBalance,
				Payload:           json.RawMessage(payload),
			})
		}

		// FX rate, only for non-CHF rows (CHF→CHF is 1.0 trivially
		// and not worth a row). Convention: mid_rate = (1 quote in
		// base units), matching UBS. Swissquote's rate_to_chf is
		// "1 of this currency = N CHF", which is exactly that
		// mid_rate when base=CHF, quote=currency.
		if currency != "CHF" && p.RateToCHF != nil && !p.RateToCHF.IsZero() {
			batch.FxRates = append(batch.FxRates, canonical.FxRateChange{
				SnapshotAt:    snap,
				BaseCurrency:  "CHF",
				QuoteCurrency: currency,
				MidRate:       *p.RateToCHF,
				Payload:       json.RawMessage(payload),
			})
		}
	}
	return rows.Err()
}
