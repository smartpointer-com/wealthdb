package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// CashAsOf returns one synthetic PositionRow per (silver_source_id,
// account_external_id, currency) whose latest non-zero cash
// balance ≤ asOf is non-zero. Multi-currency accounts get one row
// per currency.
//
// When multiple balance_kind rows exist for the same (account,
// currency, snapshot), the highest-precedence kind wins —
// `current` first (Schwab), then `closing` (UBS/Swissquote), then
// the various less-canonical kinds. This avoids double-counting
// an account that exposes "opening" + "closing" + "available" at
// the same snapshot.
//
// Returned rows reuse PositionRow rather than a sibling type so
// cmd/wealthdb's column extractors apply unchanged. The synthetic
// fields are:
//
//   AssetClass = "cash"        (display-only; not stored in gold)
//   PositionKey = "cash:<CCY>"  (groups cash rows under each account)
//   Symbol      = <CCY>         (so the symbol column renders the currency)
//   Name        = "Cash <CCY>"  (so the name column reads naturally)
//   Quantity    = nil           (cash has no unit count)
//   MarketValue = the amount    (in the row's currency)
//
// The account-level fields (DisplayName, RelationshipID, Nickname,
// AccountCategory) come from the same LEFT JOIN against accounts
// PositionsAsOf uses.
func CashAsOf(ctx context.Context, db *sql.DB, asOf int64) ([]PositionRow, error) {
	const q = `
WITH latest_per_source AS (
    SELECT silver_source_id, MAX(snapshot_at) AS snapshot_at
      FROM cash_balances
     WHERE snapshot_at <= ?
     GROUP BY silver_source_id
),
ranked AS (
    SELECT cb.silver_source_id, cb.snapshot_at, cb.account_external_id,
           cb.currency, cb.balance_kind, cb.amount,
           CASE cb.balance_kind
               WHEN 'current'    THEN 1
               WHEN 'closing'    THEN 2
               WHEN 'available'  THEN 3
               WHEN 'aggregated' THEN 4
               WHEN 'opening'    THEN 5
               WHEN 'initial'    THEN 6
               WHEN 'projected'  THEN 7
               ELSE 99
           END AS kind_rank
      FROM cash_balances cb
      JOIN latest_per_source l
        ON cb.silver_source_id = l.silver_source_id
       AND cb.snapshot_at      = l.snapshot_at
),
chosen AS (
    SELECT silver_source_id, snapshot_at, account_external_id,
           currency, balance_kind, amount,
           ROW_NUMBER() OVER (
               PARTITION BY silver_source_id, account_external_id, currency
               ORDER BY kind_rank, balance_kind
           ) AS rn
      FROM ranked
)
SELECT c.silver_source_id,
       c.snapshot_at,
       c.account_external_id,
       a.display_name,
       a.relationship_id,
       a.nickname,
       a.account_category,
       c.currency,
       CAST(c.amount AS VARCHAR) AS amount_str
  FROM chosen c
  LEFT JOIN accounts a
    ON c.silver_source_id    = a.silver_source_id
   AND c.account_external_id = a.account_external_id
 WHERE c.rn = 1
   AND c.amount != 0
 ORDER BY c.silver_source_id, c.account_external_id, c.currency`

	rows, err := db.QueryContext(ctx, q, asOf)
	if err != nil {
		return nil, fmt.Errorf("CashAsOf: %w", err)
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
			amount      sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.SnapshotAt, &r.AccountExternalID,
			&displayName, &relID, &nickname, &category,
			&r.Currency, &amount,
		); err != nil {
			return nil, fmt.Errorf("CashAsOf scan: %w", err)
		}
		r.DisplayName = nullStringToPtr(displayName)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		r.AccountCategory = nullStringToPtr(category)
		r.MarketValue = trimmedDecimalPtr(amount)
		// Synthetic fields. "cash:<CCY>" sorts deterministically
		// per account-currency; the symbol column reads the
		// currency code; the name reads "Cash <CCY>". No
		// instrument row to join — leave InstrumentExternalID nil.
		r.AssetClass = "cash"
		r.PositionKey = "cash:" + r.Currency
		ccy := r.Currency
		r.Symbol = &ccy
		nm := "Cash " + r.Currency
		r.Name = &nm
		out = append(out, r)
	}
	return out, rows.Err()
}
