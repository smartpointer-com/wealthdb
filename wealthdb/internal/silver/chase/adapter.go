// Package chase projects the chase collector's silver (a JPMorgan Chase
// retail relationship: deposit accounts and credit cards, told apart by
// `accounts.product`) into canonical change records: one account per roster
// account, its balances as cash with a card's debt negated, and the whole
// ledger of both products. It needs chase silver schema 3 or later. See
// docs/adapters/chase.md for the mapping contract.
package chase

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

const kindName = "chase"

// productCard is the `accounts.product` discriminator for a credit card; the
// other value is 'dda' (checking / savings), which every non-card row carries.
const productCard = "card"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "chase silver")
	if err != nil {
		return nil, err
	}
	return &Connection{db: db}, nil
}

type Connection struct {
	db *sql.DB
}

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }

// accountFacts is what a projection needs to know about an account beyond the
// row it is reading: its ISO currency and its product. Transactions have no
// product of their own — they inherit their account's.
type accountFacts struct {
	currency string
	product  string
}

// accountFactsByID reads each account's latest roster snapshot.
func (c *Connection) accountFactsByID(ctx context.Context) (map[string]accountFacts, error) {
	const q = `
SELECT a.account_external_id, COALESCE(a.currency, ''), a.product
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(a2.snapshot_at) FROM accounts a2
                         WHERE a2.account_external_id = a.account_external_id)`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]accountFacts{}
	for rows.Next() {
		var id string
		var f accountFacts
		if err := rows.Scan(&id, &f.currency, &f.product); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

// currencyOf returns the account's currency, defaulting to USD. Chase retail
// is USD, but the currency is read rather than assumed.
func currencyOf(m map[string]accountFacts, accountID string) string {
	if ccy := m[accountID].currency; ccy != "" {
		return ccy
	}
	return "USD"
}

// isCard reports whether the account is a credit card. An id with no roster
// row reads as a deposit, matching the product column's DEFAULT.
func isCard(m map[string]accountFacts, accountID string) bool {
	return m[accountID].product == productCard
}
