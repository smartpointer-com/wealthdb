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
//     between other sources — so the registered ReturnsPolicy (policy.go)
//     hides their return rows at every grain; the balances and flows still
//     enter the aggregates, where only the transaction flow matters.
//
//   - CASH BALANCES, not positions or instruments — cash is not an instrument.
//     They land in gold's cash_balances table (report_cash synthesises a
//     read-time cash position from them, so they still surface in holdings):
//     a CLOSING balance for every day the balance moved, valued at that day's
//     end-of-day running balance from the transaction ledger (the exact cash
//     time series, back to the ledger's start). Unlike chase, the running
//     balance is present on every history row, so the series is exact with no
//     gaps. Plus a CURRENT balance from the roster's live balance at each
//     run whose roster listed the account (silver's run_accounts), so the
//     latest net worth reflects the balance now. An account the latest runs
//     leave out has no balance after its last listing run, so gold can end
//     it. Each closing balance is stamped at its day's last second, so an
//     as-of query at a day's end returns that day's closing balance. The
//     rate a statement states rides on a closing balance of its cycle
//     (rates.go).
//
//   - TRANSACTIONS: the whole deposit ledger. The collector signs amounts the
//     canonical way (positive = balance increase), so the source sign is
//     preserved. The history row's transactionId is the stable external id.
package firstcitizens

import (
	"context"
	"database/sql"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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

func (c *Connection) Close() error { return silver.CloseDB(&c.db) }
