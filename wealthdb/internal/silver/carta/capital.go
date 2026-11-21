package carta

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// capitalAccounts are the funds' LP capital accounts dated in w, as the
// partner metrics state them: one per fund_metrics row that states a
// commitment, a called capital or distributions. The commitment, the
// called capital, the capital contributed (paid in), the distributions
// and the NAV pass as they are. A row a capital-account statement alone
// wrote states the NAV and the capital contributed, which the fund's
// position already carries, and none of the others: it is no capital
// account here. Carta states no unfunded figure and no multiple.
func (c *Connection) capitalAccounts(ctx context.Context, w canonical.Window, acct string) ([]canonical.CapitalAccountChange, error) {
	const q = `
SELECT entity_external_id, snapshot_at, COALESCE(currency, ''), commitment, called_capital, capital_contributed,
       distributions, net_asset_value
  FROM fund_metrics
 WHERE snapshot_at BETWEEN ? AND ?
   AND (commitment IS NOT NULL OR called_capital IS NOT NULL OR distributions IS NOT NULL)
 ORDER BY entity_external_id, snapshot_at`
	return silver.Query(ctx, c.db, "capitalAccounts", q, func(rows *sql.Rows) (canonical.CapitalAccountChange, error) {
		var (
			entity                                       int64
			ca                                           canonical.CapitalAccountChange
			ccy                                          string
			commitment, called, contributed, back, value sql.NullString
		)
		if err := rows.Scan(&entity, &ca.AsOf, &ccy, &commitment, &called, &contributed, &back, &value); err != nil {
			return ca, err
		}
		account := acct
		ca.AccountExternalID, ca.InstrumentExternalID, ca.Currency = &account, instrumentID(entity), normCcy(ccy)
		ca.Commitment, ca.Called = silver.DecimalPtrOrNil(commitment), silver.DecimalPtrOrNil(called)
		ca.PaidIn, ca.Distributed = silver.DecimalPtrOrNil(contributed), silver.DecimalPtrOrNil(back)
		ca.NAV = silver.DecimalPtrOrNil(value)
		return ca, nil
	}, w.Start, w.End)
}
