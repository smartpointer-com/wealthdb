package gold

import (
	"context"
	"database/sql"

	"github.com/ptu/wealthdb/internal/canonical"
)

// CashAsOf returns one synthetic PositionRow per (silver_source_id,
// account_external_id, currency) whose latest non-zero cash balance
// ≤ asOf is non-zero, with the amount converted to outCcy. Multi-
// currency accounts get one row per currency.
//
// When multiple balance_kind rows exist for the same (account,
// currency, snapshot), the highest-precedence kind wins — `current`
// first (Schwab), then `closing` (UBS/Swissquote), etc. — so an
// account exposing opening + closing + available at one snapshot
// isn't double-counted.
//
// Rows reuse PositionRow (so cmd/wealthdb's column extractors apply
// unchanged); the report_cash macro sets the synthetic fields
// (asset_class='cash', position_key='cash:<CCY>', symbol=<CCY>,
// name='Cash <CCY>', quantity NULL, market_value = the amount). See
// migration 0021.
func CashAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) ([]PositionRow, error) {
	return scanPositionRows(ctx, db, "CashAsOf",
		`SELECT * FROM report_cash(?, ?)`, effectiveAsOf(asOf, mode), outCcy)
}
