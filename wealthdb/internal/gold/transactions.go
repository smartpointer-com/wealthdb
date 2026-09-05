package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// TransactionRow is one row of the gold transactions readout.
// Decimal columns come back as canonical strings (DuckDB CAST to
// VARCHAR with trailing-zero trim, matching PositionRow). The
// joined account / instrument fields are nullable and the consumer
// chooses fallbacks for display.
//
// The field ORDER is load-bearing: TransactionsBetween scans
// report_transactions positionally, so this struct, the scan targets
// there, and the macro's projection must be edited together.
type TransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
	AccountKind           *string // accounts.account_kind
	DisplayName           *string // accounts.display_name
	RelationshipID        *string
	Nickname              *string
	AccountCategory       *string
	InstrumentExternalID  *string
	Symbol                *string // instruments.symbol
	Name                  *string // instruments.name
	AssetClass            *string // instruments.asset_class
	Kind                  string
	Currency              string
	GrossAmount           *string
	NetAmount             *string
	Quantity              *string
	Price                 *string
	Description           *string // transactions.description; free-text label
	// ValueOutCcy is NetAmount converted to the requested output
	// currency at occurred_at by the report_transactions macro (flat
	// nearest-rate FX in SQL). Nil when no FX path resolves.
	ValueOutCcy *string
}

// SortOrder controls the row ordering for TransactionsBetween.
// Ascending is the default — oldest first; Descending is newest
// first.
type SortOrder int

const (
	SortAscending  SortOrder = iota // oldest first
	SortDescending                  // newest first
)

// TransactionsBetween returns every transaction whose occurred_at
// falls in [fromEpoch, toEpoch], inclusive, with net_amount
// converted to outCcy at occurred_at. Sorted by occurred_at (asc or
// desc per `order`), then (silver_source_id,
// transaction_external_id) as a stable tiebreaker. The query and FX
// are the report_transactions table macro (migration 0021, re-issued
// with account_kind in 0039); the macro emits ascending, so the
// descending case re-sorts here.
//
// `SELECT *` with a positional Scan: any column added to the macro
// must be added to TransactionRow and to the scan list below in the
// same position, in the same change as the migration.
func TransactionsBetween(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, outCcy string, order SortOrder) ([]TransactionRow, error) {
	q := `SELECT * FROM report_transactions(?, ?, ?)`
	if order == SortDescending {
		q += ` ORDER BY occurred_at DESC, silver_source_id, transaction_external_id`
	}

	rows, err := db.QueryContext(ctx, q, fromEpoch, toEpoch, outCcy)
	if err != nil {
		return nil, fmt.Errorf("TransactionsBetween: %w", err)
	}
	defer rows.Close()

	var out []TransactionRow
	for rows.Next() {
		var (
			r                                      TransactionRow
			acctKind                               sql.NullString
			displayName, relID, nickname, category sql.NullString
			instr, symbol, name, assetClass        sql.NullString
			grossStr, netStr, qtyStr, priceStr     sql.NullString
			description, valueOut                  sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
			&r.AccountExternalID,
			&acctKind, &displayName, &relID, &nickname, &category,
			&instr, &symbol, &name, &assetClass,
			&r.Kind, &r.Currency,
			&grossStr, &netStr, &qtyStr, &priceStr,
			&description, &valueOut,
		); err != nil {
			return nil, fmt.Errorf("TransactionsBetween scan: %w", err)
		}
		r.Description = nullStringToPtr(description)
		r.AccountKind = nullStringToPtr(acctKind)
		r.DisplayName = nullStringToPtr(displayName)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.InstrumentExternalID = nullStringToPtr(instr)
		r.Symbol = nullStringToPtr(symbol)
		r.Name = nullStringToPtr(name)
		r.AssetClass = nullStringToPtr(assetClass)
		r.GrossAmount = trimmedDecimalPtr(grossStr)
		r.NetAmount = trimmedDecimalPtr(netStr)
		r.Quantity = trimmedDecimalPtr(qtyStr)
		r.Price = trimmedDecimalPtr(priceStr)
		r.ValueOutCcy = trimmedDecimalPtr(valueOut)
		out = append(out, r)
	}
	return out, rows.Err()
}
