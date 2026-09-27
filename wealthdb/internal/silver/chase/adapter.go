// Package chase projects the chase collector's silver — a JPMorgan Chase
// retail relationship holding deposit accounts (checking / savings) and credit
// cards — into canonical change records. See collectors/chase/ for the
// bronze/silver schema and the gold mapping this implements.
//
// Both products live in the same silver tables and are told apart by
// `accounts.product` ('dda' | 'card'). Neither carries an instrument, so the
// projection emits no positions:
//
//   - ACCOUNTS. One per roster account: AccountKind 'cash' for a deposit
//     account, 'card' for a credit card. DisplayName from the nickname
//     (falling back to the last-4 mask); TaxWrapper 'taxable_personal',
//     ManagementStyle 'self_directed'. All overridable via account_overrides.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument,
//     and a card is a revolving-credit liability that gold carries as negative
//     cash rather than a position. They land in gold's cash_balances table
//     (report_cash synthesises a read-time cash position from them, so they
//     still surface in holdings): a CLOSING balance for every day the balance
//     moved, plus a CURRENT balance from the roster's live figure so the latest
//     net worth reflects the balance now. An as-of query at any statement date
//     returns that statement's closing balance. See snapshots.go for the three
//     sources of a closing mark and how they are kept apart.
//
//     SIGN: silver stores every card figure the provider's way, so a card
//     balance is the POSITIVE amount owed. This adapter negates it — the
//     canonical convention for a liability is negative cash. Available credit
//     and the credit limit are not balances of anything owned and never become
//     rows.
//
//   - TRANSACTIONS: the whole ledger of both products. See transactions.go for
//     the sign treatment and the kind mapping.
//
// Requires chase silver schema 3 or later (cards and their statement coverage
// flag). `load` applies the migrations in place, so any silver a load has
// touched is at the current version.
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

func (c *Connection) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

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
