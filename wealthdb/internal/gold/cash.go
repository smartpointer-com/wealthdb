package gold

import (
	"context"
	"database/sql"
)

// CashAsOf returns one synthetic PositionRow per (silver_source_id,
// account_external_id, currency) whose latest cash balance at or
// before asOf is non-zero (cash_chosen), with the amount converted to
// outCcy. Multi-currency accounts get one row per currency.
//
// When multiple balance_kind rows exist for the same (account,
// currency, snapshot), the highest-precedence kind wins — `current`
// first (Schwab), then `closing` (UBS/Swissquote), etc. — so an
// account exposing opening + closing + available at one snapshot
// isn't double-counted.
//
// Rows reuse PositionRow (so cmd/wealthdb's column extractors apply
// unchanged); the report_cash macro sets the synthetic fields
// (asset_class='cash', vehicle='demand_deposit',
// position_key='cash:<CCY>', symbol=<CCY>, name='Cash <CCY>', quantity
// NULL, market_value = the amount). See migration 0123.
func CashAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]PositionRow, error) {
	return scanPositionRows(ctx, db, "CashAsOf",
		`SELECT * FROM report_cash(?, ?)`, asOf, outCcy)
}
