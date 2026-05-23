package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// TransactionRow is one row of the gold transactions readout.
// Decimal columns come back as canonical strings (DuckDB CAST to
// VARCHAR with trailing-zero trim, matching PositionRow). The
// joined account / instrument fields are nullable and the
// consumer chooses fallbacks for display.
type TransactionRow struct {
	SilverSourceID        string
	TransactionExternalID string
	OccurredAt            int64
	AccountExternalID     string
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
	Description           *string // transactions.description; free-text label, see canonical.TransactionChange.Description
}

// SortOrder controls the row ordering for TransactionsBetween.
// Ascending is the default — oldest first, which matches the
// natural chronological reading. Descending is for the
// newest-first view (e.g. an inbox-style query).
type SortOrder int

const (
	SortAscending  SortOrder = iota // oldest first
	SortDescending                  // newest first
)

// TransactionsBetween returns every transaction whose occurred_at
// falls in [fromEpoch, toEpoch], inclusive. Sorted by
// occurred_at first (asc or desc, per `order`), then by
// (silver_source_id, transaction_external_id) as a stable
// tiebreaker so the output is deterministic across re-runs.
//
// Both bounds are required (the caller fills epoch / now for
// open-ended ranges). NULLs in joined columns are surfaced as
// nil pointers; consumer formatters fall back as needed.
func TransactionsBetween(ctx context.Context, db *sql.DB, fromEpoch, toEpoch int64, order SortOrder) ([]TransactionRow, error) {
	direction := "ASC"
	if order == SortDescending {
		direction = "DESC"
	}
	// Two LEFT JOINs against symbol_resolutions cover the two
	// lookup_kind discriminators populated by `wealthdb
	// resolve-symbols`: ID-keyed (matches t.instrument_external_id)
	// and name-keyed (matches t.description). COALESCE prefers the
	// joined instruments.symbol first; the LLM-derived fallback
	// only fires when instruments produced NULL.
	q := `
SELECT t.silver_source_id,
       t.transaction_external_id,
       t.occurred_at,
       t.account_external_id,
       a.display_name,
       a.relationship_id,
       a.nickname,
       a.account_category,
       t.instrument_external_id,
       COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
       i.name,
       i.asset_class,
       t.kind,
       t.currency,
       CAST(t.gross_amount AS VARCHAR) AS gross_str,
       CAST(t.net_amount   AS VARCHAR) AS net_str,
       CAST(t.quantity     AS VARCHAR) AS qty_str,
       CAST(t.price        AS VARCHAR) AS price_str,
       t.description
  FROM transactions t
  LEFT JOIN accounts a
    ON t.silver_source_id    = a.silver_source_id
   AND t.account_external_id = a.account_external_id
  LEFT JOIN instruments i
    ON t.silver_source_id        = i.silver_source_id
   AND t.instrument_external_id  = i.instrument_external_id
  LEFT JOIN symbol_resolutions sri
    ON sri.silver_source_id = t.silver_source_id
   AND sri.lookup_kind      = 'instrument_external_id'
   AND sri.lookup_value     = t.instrument_external_id
  LEFT JOIN symbol_resolutions srn
    ON srn.silver_source_id = t.silver_source_id
   AND srn.lookup_kind      = 'name'
   AND srn.lookup_value     = t.description
 WHERE t.occurred_at BETWEEN ? AND ?
 ORDER BY t.occurred_at ` + direction + `, t.silver_source_id, t.transaction_external_id`

	rows, err := db.QueryContext(ctx, q, fromEpoch, toEpoch)
	if err != nil {
		return nil, fmt.Errorf("TransactionsBetween: %w", err)
	}
	defer rows.Close()

	var out []TransactionRow
	for rows.Next() {
		var (
			r                                              TransactionRow
			displayName, relID, nickname, category         sql.NullString
			instr, symbol, name, assetClass                sql.NullString
			grossStr, netStr, qtyStr, priceStr             sql.NullString
			description                                    sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.TransactionExternalID, &r.OccurredAt,
			&r.AccountExternalID,
			&displayName, &relID, &nickname, &category,
			&instr, &symbol, &name, &assetClass,
			&r.Kind, &r.Currency,
			&grossStr, &netStr, &qtyStr, &priceStr,
			&description,
		); err != nil {
			return nil, fmt.Errorf("TransactionsBetween scan: %w", err)
		}
		r.Description = nullStringToPtr(description)
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
		out = append(out, r)
	}
	return out, rows.Err()
}
