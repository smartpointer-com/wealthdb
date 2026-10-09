package gold

import (
	"context"
	"database/sql"
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
	// AssetClass (exposure) + Vehicle (wrapper): the 2-D taxonomy.
	AssetClass  string
	Vehicle     string
	Currency    string
	Quantity    *string
	MarketValue *string
	// ValueOutCcy is MarketValue converted to the requested output
	// currency by the report_positions / report_cash macro (flat
	// nearest-rate FX in SQL). Nil when no FX path resolves.
	ValueOutCcy *string
	// The cost basis the source states (gold's book_value), in the
	// position's currency and converted like ValueOutCcy, with the
	// figures derived from it (migration 0116, docs/GAINS.md). The
	// basis fields are nil on a cash row and wherever the source states
	// no basis, and the unrealized gain is nil too on a line no cost
	// basis describes, such as a mortgage. AccruedInterest is the
	// source's; CleanValue is MarketValue less it.
	BookValue        *string
	BookValueOutCcy  *string
	AccruedInterest  *string
	CleanValue       *string
	UnrealizedGain   *string
	UnrealizedOutCcy *string
	UnrealizedRatio  *float64
	BasisStamp       *string // origin/method/fees, e.g. stated/lots/included
	AcquisitionDate  *string // YYYY-MM-DD
}

// PositionsAsOf returns the consolidated portfolio as of the given
// Unix-seconds timestamp, with market_value converted to outCcy.
// For each silver source, the rows of the latest snapshot_at ≤ asOf
// are returned, sorted by (silver_source_id, account_external_id,
// position_key). FX (and everything else) is computed in SQL by the
// report_positions table macro (see migrations 0020/0021); this is
// just the scan. See docs/DESIGN.md §10.1.
func PositionsAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]PositionRow, error) {
	return scanPositionRows(ctx, db, "PositionsAsOf",
		`SELECT * FROM report_positions(?, ?)`, asOf, outCcy)
}

// scanPositionRows runs a report_positions / report_cash macro query
// (both emit the same position shape) and scans the rows.
func scanPositionRows(ctx context.Context, db *sql.DB, label, q string, args ...any) ([]PositionRow, error) {
	return scanRows(ctx, db, label, q, args, func(r *PositionRow) []any {
		return []any{
			&r.SilverSourceID, &r.SnapshotAt, &r.AccountExternalID,
			str(&r.DisplayName), str(&r.RelationshipID), str(&r.Nickname), str(&r.AccountCategory),
			&r.PositionKey, str(&r.InstrumentExternalID), str(&r.Symbol), str(&r.Name),
			&r.AssetClass, nonNull(&r.Vehicle), &r.Currency,
			dec(&r.Quantity), dec(&r.MarketValue), dec(&r.ValueOutCcy),
			dec(&r.BookValue), dec(&r.BookValueOutCcy), dec(&r.AccruedInterest), dec(&r.CleanValue),
			dec(&r.UnrealizedGain), dec(&r.UnrealizedOutCcy), flt(&r.UnrealizedRatio),
			str(&r.BasisStamp), str(&r.AcquisitionDate),
		}
	})
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

// sixAggPtrs trims the six aggregate value columns every report_*
// macro projects — positions / cash / total, each in base and out
// currency — returning them in that order for the caller to assign to
// its row struct.
func sixAggPtrs(pvb, cvb, tvb, pvo, cvo, tvo sql.NullString) (*string, *string, *string, *string, *string, *string) {
	return trimmedDecimalPtr(pvb), trimmedDecimalPtr(cvb), trimmedDecimalPtr(tvb),
		trimmedDecimalPtr(pvo), trimmedDecimalPtr(cvo), trimmedDecimalPtr(tvo)
}
