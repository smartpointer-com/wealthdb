// Package raiffeisenat projects the raiffeisen_at collector's silver (an
// Austrian Raiffeisen / Mein ELBA retail deposit relationship — checking /
// savings) into canonical change records. See collectors/raiffeisen_at/ for
// the bronze/silver schema and the gold mapping this implements.
//
// A deposit relationship is cash-only, so the projection is simple (and
// parallel to the chase / firstcitizens siblings):
//
//   - One ACCOUNT (AccountKind 'cash') per deposit account. DisplayName from
//     the account type (Kontoart) and the last-4 mask; TaxWrapper
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
//     balance from the roster's live balance at each run whose roster listed
//     the account (silver's run_accounts), so the latest net worth reflects
//     the balance now. An account the latest runs leave out has no balance
//     after its last listing run, so gold can end it. Each day's saldo is
//     stamped at the day's last second, so the series gives the day's value.
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved. The kontoumsaetze row id is the stable external id. The
//     text columns — counterparty (the participant line, else the
//     card-network merchant name), provider_category (the source category
//     slug) and description (the purpose line, else short order purpose +
//     payment reference) — follow the contract documented on Transactions
//     in transactions.go; they are a projection of text only and never
//     influence a row's kind or amount.
package raiffeisenat

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
