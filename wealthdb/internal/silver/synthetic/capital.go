package synthetic

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// readCapitalAccounts reads the capital accounts dated in w, as a
// generator wrote them, oldest first. A figure that is no decimal is a
// defect in whatever wrote it, and fails the read.
func (c *Connection) readCapitalAccounts(ctx context.Context, w canonical.Window) ([]canonical.CapitalAccountChange, error) {
	if !c.schema.capital {
		return nil, nil
	}
	return silver.Query(ctx, c.db, "synthetic capital accounts", `
SELECT as_of, account_id, instrument_id, source_ref, currency, commitment, called, paid_in, unfunded,
       distributed, nav, tvpi, dpi, payload
  FROM capital_accounts
 WHERE as_of BETWEEN ? AND ?
 ORDER BY as_of, instrument_id, source_ref`, func(rows *sql.Rows) (canonical.CapitalAccountChange, error) {
		var (
			ca      canonical.CapitalAccountChange
			account sql.NullString
			payload string
			money   [6]sql.NullString
			ratios  [2]sql.NullString
		)
		if err := rows.Scan(&ca.AsOf, &account, &ca.InstrumentExternalID, &ca.SourceRef, &ca.Currency, &money[0],
			&money[1], &money[2], &money[3], &money[4], &money[5], &ratios[0], &ratios[1], &payload); err != nil {
			return ca, err
		}
		ca.AccountExternalID = silver.NullStringPtr(account)
		parse := func(s sql.NullString) (*canonical.Decimal, error) {
			if !s.Valid {
				return nil, nil
			}
			d, err := canonical.NewDecimalFromString(s.String)
			if err != nil {
				return nil, fmt.Errorf("synthetic capital account (%s, %d): %q: %w", ca.InstrumentExternalID, ca.AsOf,
					s.String, err)
			}
			return &d, nil
		}
		figures := []**canonical.Decimal{&ca.Commitment, &ca.Called, &ca.PaidIn, &ca.Unfunded, &ca.Distributed, &ca.NAV}
		for i, m := range money {
			d, err := parse(m)
			if err != nil {
				return ca, err
			}
			*figures[i] = d
		}
		multiples := []**float64{&ca.TVPI, &ca.DPI}
		for i, r := range ratios {
			d, err := parse(r)
			if err != nil {
				return ca, err
			}
			if d == nil {
				continue
			}
			f := d.InexactFloat64()
			*multiples[i] = &f
		}
		ca.Payload = payloadWith(payload, nil)
		return ca, nil
	}, w.Start, w.End)
}
