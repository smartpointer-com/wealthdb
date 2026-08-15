// Package firstcitizens projects the firstcitizens collector's silver (a First
// Citizens Bank retail deposit relationship — checking / savings) into
// canonical change records. See collectors/firstcitizens/ for the bronze/silver
// schema and the gold mapping this implements.
//
// A deposit relationship is cash-only, so the projection is simple (and
// parallel to the chase sibling, whose silver is column-compatible):
//
//   - One ACCOUNT (AccountKind 'cash') per deposit account. DisplayName from
//     the nickname (falling back to the last-4 mask); TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed'. All overridable via
//     account_overrides. These are conduit accounts — cash passes through them
//     between other sources — so returns for the accounts themselves are
//     excluded downstream; only the transaction flow matters.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument.
//     They land in gold's cash_balances table (report_cash synthesises a
//     read-time cash position from them, so they still surface in holdings):
//     a CLOSING balance for every day the balance moved, valued at that day's
//     end-of-day running balance from the transaction ledger (the exact cash
//     time series, back to the ledger's start). Unlike chase, the running
//     balance is present on every history row, so the series is exact with no
//     gaps. Plus a CURRENT balance from the roster's live balance, so the
//     latest net worth reflects the balance now. An as-of query at any date
//     returns that day's closing balance.
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved. The history row's transactionId is the stable external id.
package firstcitizens

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

const kindName = "firstcitizens"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "firstcitizens silver")
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
// from the latest accounts snapshot. First Citizens retail is USD, but the
// currency is read rather than assumed; a missing/blank value falls back to USD.
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
