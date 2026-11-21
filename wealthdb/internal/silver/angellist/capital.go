package angellist

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// capitalAccounts are the positions' capital accounts dated in w, as the
// portal's positions table states them: one per offering whose node
// states a commitment or a TVPI. The node is the one the latest dump
// that listed the position captured, so the account stands at that
// dump's portfolio data date, else at the dump's time. The commitment is
// the node's commitmentAmount, else its investmentAmount, which a node
// that states no commitment states instead. The calls draw on it over
// time, or pay it at once. The contributed capital (called), the
// realized value (distributed), the unrealized value (the NAV) and the
// TVPI pass as they are. The portal states no unfunded figure, DPI or
// IRR for a position; its portfolio summary states them for the whole
// account only.
func (c *Connection) capitalAccounts(ctx context.Context, w canonical.Window, account string) ([]canonical.CapitalAccountChange, error) {
	if account == "" {
		return nil, nil
	}
	const q = `
WITH n AS (
    SELECT o.position_external_id, COALESCE(s.data_date, o.last_seen_at) AS as_of,
           COALESCE(json_extract(o.payload, '$.commitmentAmount.currency'),
                    json_extract(o.payload, '$.investmentAmount.currency'),
                    json_extract(o.payload, '$.contributedAmount.currency'), o.currency) AS currency,
           CAST(COALESCE(json_extract(o.payload, '$.commitmentAmount.fractional'),
                         json_extract(o.payload, '$.investmentAmount.fractional')) AS TEXT) AS commitment,
           CAST(json_extract(o.payload, '$.contributedAmount.fractional') AS TEXT) AS called,
           CAST(json_extract(o.payload, '$.realizedValue.fractional') AS TEXT) AS distributed,
           CAST(json_extract(o.payload, '$.unrealizedValue.fractional') AS TEXT) AS nav,
           json_extract(o.payload, '$.tvpi') AS tvpi
      FROM offerings o
      LEFT JOIN portfolio_summary s ON s.snapshot_at = o.last_seen_at
     WHERE json_valid(o.payload))
SELECT position_external_id, as_of, currency, commitment, called, distributed, nav, tvpi
  FROM n
 WHERE (commitment IS NOT NULL OR tvpi IS NOT NULL) AND as_of BETWEEN ? AND ?
 ORDER BY position_external_id`
	return silver.Query(ctx, c.db, "capitalAccounts", q, func(rows *sql.Rows) (canonical.CapitalAccountChange, error) {
		var (
			ca                                  canonical.CapitalAccountChange
			commitment, called, back, nav, tvpi sql.NullString
			ccy                                 sql.NullString
		)
		if err := rows.Scan(&ca.InstrumentExternalID, &ca.AsOf, &ccy, &commitment, &called, &back, &nav, &tvpi); err != nil {
			return ca, err
		}
		acct := account
		ca.AccountExternalID, ca.Currency = &acct, "USD"
		if ccy.Valid && ccy.String != "" {
			ca.Currency = ccy.String
		}
		ca.Commitment, ca.Called = minorText(commitment), minorText(called)
		ca.Distributed, ca.NAV = minorText(back), minorText(nav)
		if d := silver.DecimalPtrOrNil(tvpi); d != nil {
			f := d.InexactFloat64()
			ca.TVPI = &f
		}
		return ca, nil
	}, w.Start, w.End)
}

// minorText is a minor-unit (cents) amount as text, scaled to a Decimal;
// nil where it is NULL or no number.
func minorText(s sql.NullString) *canonical.Decimal {
	d := silver.DecimalPtrOrNil(s)
	if d != nil {
		*d = d.Shift(-2)
	}
	return d
}
