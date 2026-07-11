package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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
	AssetClass           string  // legacy 1-D class (control column)
	// AssetClassNew + Vehicle are the 2-D taxonomy pair (TAXONOMY.md);
	// empty for any row a source hasn't migrated yet.
	AssetClassNew string
	Vehicle       string
	Currency      string
	Quantity             *string
	MarketValue          *string
	// ValueOutCcy is MarketValue converted to the requested output
	// currency by the report_positions / report_cash macro (flat
	// nearest-rate FX in SQL). Nil when no FX path resolves.
	ValueOutCcy *string
}

// PositionsAsOf returns the consolidated portfolio as of the given
// Unix-seconds timestamp, with market_value converted to outCcy.
// For each silver source, the rows of the latest snapshot_at ≤ asOf
// are returned, sorted by (silver_source_id, account_external_id,
// position_key). FX (and everything else) is computed in SQL by the
// report_positions table macro (see migrations 0020/0021); this is
// just the scan. See docs/DESIGN.md §10.1.
func PositionsAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) ([]PositionRow, error) {
	return scanPositionRows(ctx, db, "PositionsAsOf",
		`SELECT * FROM report_positions(?, ?)`, effectiveAsOf(asOf, mode), outCcy)
}

// scanPositionRows runs a report_positions / report_cash macro query
// (both emit the same 16-column position shape) and scans the rows.
func scanPositionRows(ctx context.Context, db *sql.DB, label, q string, args ...any) ([]PositionRow, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer rows.Close()

	var out []PositionRow
	for rows.Next() {
		var (
			r                                             PositionRow
			displayName, relID, nickname, category, instr sql.NullString
			symbol, name, qty, mvalue, valueOut           sql.NullString
			assetClassNew, vehicle                        sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.SnapshotAt, &r.AccountExternalID,
			&displayName, &relID, &nickname, &category,
			&r.PositionKey, &instr, &symbol, &name,
			&r.AssetClass, &assetClassNew, &vehicle, &r.Currency, &qty, &mvalue, &valueOut,
		); err != nil {
			return nil, fmt.Errorf("%s scan: %w", label, err)
		}
		r.AssetClassNew = assetClassNew.String
		r.Vehicle = vehicle.String
		r.DisplayName = nullStringToPtr(displayName)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.InstrumentExternalID = nullStringToPtr(instr)
		r.Symbol = nullStringToPtr(symbol)
		r.Name = nullStringToPtr(name)
		r.Quantity = trimmedDecimalPtr(qty)
		r.MarketValue = trimmedDecimalPtr(mvalue)
		r.ValueOutCcy = trimmedDecimalPtr(valueOut)
		out = append(out, r)
	}
	return out, rows.Err()
}

// effectiveAsOf maps the FX mode to the as-of bound passed to the
// report macros. Historic uses the real asOf (the macros pick the
// nearest rate at-or-before each line's own snapshot). Current
// ignores asOf — a max bound makes the latest snapshot win; the
// per-line FX still resolves flat at each line's snapshot day (the
// 1.x FX engine's literal "latest rate" semantics are not preserved,
// which is acceptable for this rarely-used mode — see web/DESIGN.md).
func effectiveAsOf(asOf int64, mode canonical.FxMode) int64 {
	if mode == canonical.FxModeCurrent {
		return math.MaxInt64
	}
	return asOf
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
