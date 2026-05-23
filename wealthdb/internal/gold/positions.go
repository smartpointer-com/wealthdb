package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// PositionRow is one row of the consolidated positions output.
// All decimal columns come back as their canonical string form
// (DuckDB CAST to VARCHAR); the caller formats further if needed.
//
// The fields populated by LEFT JOIN against `accounts` /
// `instruments` (DisplayName, RelationshipID, Nickname,
// AccountCategory, Symbol, Name) are nullable; the consumer
// chooses when to fall back to AccountExternalID / PositionKey for
// user-facing output.
type PositionRow struct {
	SilverSourceID       string
	SnapshotAt           int64
	AccountExternalID    string
	DisplayName          *string // accounts.display_name
	RelationshipID       *string // accounts.relationship_id (UBS dimension)
	Nickname             *string // accounts.nickname
	AccountCategory      *string // accounts.account_category
	PositionKey          string
	InstrumentExternalID *string
	Symbol               *string // instruments.symbol
	Name                 *string // instruments.name
	AssetClass           string
	Currency             string
	Quantity             *string
	MarketValue          *string
}

// PositionsAsOf returns the consolidated portfolio as of the given
// Unix-seconds timestamp. For each silver source, the rows of the
// latest snapshot_at ≤ asOf are returned. Sorted by
// (silver_source_id, account_external_id, position_key) so output
// is deterministic.
//
// Implements docs/DESIGN.md §10.1 in straightforward SQL (the
// note at the top of §10 reminds us the wire-form is a functional
// spec; query rewriters / planner hints can replace this later
// without changing the contract).
func PositionsAsOf(ctx context.Context, db *sql.DB, asOf int64) ([]PositionRow, error) {
	// LEFT JOIN against symbol_resolutions (lookup_kind =
	// 'instrument_external_id' only — positions has no 'name'
	// equivalent column) so LLM-derived tickers from `wealthdb
	// resolve-symbols` fill in for instruments whose silver
	// adapter couldn't surface a symbol. COALESCE prefers the
	// instruments-table value when it exists.
	const q = `
WITH latest_per_source AS (
    SELECT silver_source_id, MAX(snapshot_at) AS snapshot_at
      FROM positions
     WHERE snapshot_at <= ?
     GROUP BY silver_source_id
)
SELECT p.silver_source_id,
       p.snapshot_at,
       p.account_external_id,
       a.display_name,
       a.relationship_id,
       a.nickname,
       a.account_category,
       p.position_key,
       p.instrument_external_id,
       COALESCE(i.symbol, sri.symbol) AS symbol,
       i.name,
       p.asset_class,
       p.currency,
       CAST(p.quantity     AS VARCHAR) AS quantity_str,
       CAST(p.market_value AS VARCHAR) AS market_value_str
  FROM positions p
  LEFT JOIN accounts a
    ON p.silver_source_id    = a.silver_source_id
   AND p.account_external_id = a.account_external_id
  LEFT JOIN instruments i
    ON p.silver_source_id        = i.silver_source_id
   AND p.instrument_external_id  = i.instrument_external_id
  LEFT JOIN symbol_resolutions sri
    ON sri.silver_source_id = p.silver_source_id
   AND sri.lookup_kind      = 'instrument_external_id'
   AND sri.lookup_value     = p.instrument_external_id
  JOIN latest_per_source l
    ON p.silver_source_id = l.silver_source_id
   AND p.snapshot_at      = l.snapshot_at
 ORDER BY p.silver_source_id, p.account_external_id, p.position_key`

	rows, err := db.QueryContext(ctx, q, asOf)
	if err != nil {
		return nil, fmt.Errorf("PositionsAsOf: %w", err)
	}
	defer rows.Close()

	var out []PositionRow
	for rows.Next() {
		var (
			r           PositionRow
			displayName sql.NullString
			relID       sql.NullString
			nickname    sql.NullString
			category    sql.NullString
			instr       sql.NullString
			symbol      sql.NullString
			name        sql.NullString
			qty         sql.NullString
			mvalue      sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.SnapshotAt, &r.AccountExternalID,
			&displayName, &relID, &nickname, &category,
			&r.PositionKey, &instr, &symbol, &name,
			&r.AssetClass, &r.Currency, &qty, &mvalue,
		); err != nil {
			return nil, fmt.Errorf("PositionsAsOf scan: %w", err)
		}
		r.DisplayName = nullStringToPtr(displayName)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.InstrumentExternalID = nullStringToPtr(instr)
		r.Symbol = nullStringToPtr(symbol)
		r.Name = nullStringToPtr(name)
		r.Quantity = trimmedDecimalPtr(qty)
		r.MarketValue = trimmedDecimalPtr(mvalue)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullStringToPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

// trimmedDecimalPtr drops trailing zeros (and a dangling decimal
// point) from DuckDB's CAST(decimal AS VARCHAR) output. The cast
// pads to the declared scale — DECIMAL(28,8) renders "100" as
// "100.00000000" — which is correct but visually noisy. Applied
// here so every consumer of PositionRow (table / csv / json
// formatters) gets the normalised form for free.
func trimmedDecimalPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := trimTrailingZeros(n.String)
	return &s
}

func trimTrailingZeros(s string) string {
	// No fractional digits → nothing to trim.
	if !strings.ContainsRune(s, '.') {
		return s
	}
	s = strings.TrimRight(s, "0")
	// "100." → "100"; leave "0" alone (which has no '.').
	s = strings.TrimRight(s, ".")
	return s
}
