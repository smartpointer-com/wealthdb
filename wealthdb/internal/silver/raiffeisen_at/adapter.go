// Package raiffeisenat projects the raiffeisen_at collector's silver (an
// Austrian Raiffeisen / Mein ELBA retail deposit relationship — checking /
// savings) into canonical change records. See collectors/raiffeisen_at/ for
// the bronze/silver schema and the gold mapping this implements.
//
// A deposit relationship is cash-only, so the projection is simple (and
// parallel to the chase / firstcitizens siblings):
//
//   - One ACCOUNT (AccountKind 'cash') per deposit account. DisplayName from
//     the account type (e.g. 'Gehaltekonto') and the last-4 mask; TaxWrapper
//     'taxable_personal', ManagementStyle 'self_directed'. All overridable via
//     account_overrides. These are conduit accounts — cash passes through them
//     between other sources — so the registered ReturnsPolicy (policy.go)
//     hides their return rows at every grain; the balances and flows still
//     enter the aggregates, where only the transaction flow matters.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument.
//     A CLOSING balance for every day the daily-balance series carries a saldo
//     (the `kontostaende` series, in silver's daily_balances table — unlike the
//     US siblings, the transaction history has no per-row running balance, so
//     the cash time series comes from this dedicated series). Plus a CURRENT
//     balance from the roster's live balance, so the latest net worth reflects
//     the balance now.
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved. The kontoumsaetze row id is the stable external id.
package raiffeisenat

import (
	"context"
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// kindName is the silver_kind / collector name (the whitelist entry the
// 0035 gold migration adds).
const kindName = "raiffeisen_at"

func init() {
	silver.Register(&Adapter{})
}

type Adapter struct{}

func (*Adapter) Kind() string { return kindName }

func (*Adapter) Open(_ context.Context, spec silver.OpenSpec) (silver.Connection, error) {
	db, err := silver.OpenReadOnlySQLite(spec.Path, "raiffeisen_at silver")
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

// accountCurrencies maps each account_external_id (IBAN) to its ISO currency,
// read from the latest accounts snapshot. Austrian Raiffeisen retail is EUR,
// but the currency is read rather than assumed; a missing/blank value falls
// back to EUR.
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

// currencyOf returns the account's currency, defaulting to EUR.
func currencyOf(m map[string]string, accountID string) string {
	if ccy := m[accountID]; ccy != "" {
		return ccy
	}
	return "EUR"
}
