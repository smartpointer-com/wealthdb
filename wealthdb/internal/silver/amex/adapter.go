// Package amex projects the amex collector's silver — an American Express
// credit- and charge-card relationship — into canonical change records: one
// card account per card, its outstanding balance as negative cash, and the
// whole card ledger. Nothing reaches the returns engine; the source earns its
// keep in spending. docs/adapters/amex.md documents the mapping, the one sign
// flip (signedBalance) and the kind rules.
package amex

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "amex"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "amex silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

// accountCurrencies maps each account_external_id to its ISO currency, read
// from the latest accounts snapshot. The relationship is USD, but the currency
// is read rather than assumed; a missing/blank value falls back to USD.
func (c *Connection) accountCurrencies(ctx context.Context) (map[string]string, error) {
	const q = `
SELECT a.account_external_id, COALESCE(a.currency, '')
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(a2.snapshot_at) FROM accounts a2
                         WHERE a2.account_external_id = a.account_external_id)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, ccy string
		if err := rows.Scan(&id, &ccy); err != nil {
			return nil, err
		}
		out[id] = ccy
	}
	return out, rows.Err()
}

// currencyOf returns the account's currency, defaulting to USD.
func currencyOf(m map[string]string, accountID string) string {
	if ccy := m[accountID]; ccy != "" {
		return ccy
	}
	return "USD"
}
