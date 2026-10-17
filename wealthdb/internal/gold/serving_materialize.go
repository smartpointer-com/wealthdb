package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// latestArg is the as-of a report macro reads as "the latest snapshot",
// the argument the report models pass too.
const latestArg = "9223372036854775807"

// servingTables are the tables MaterializeServingTables rewrites, each
// with the macro whose rows it holds verbatim (migration 0132).
var servingTables = []struct{ table, source string }{
	{"serving_sources_history", "report_sources_history_multi()"},
	{"serving_accounts_history", "report_accounts_history_multi()"},
	{"serving_positions_history", "report_positions_history_multi()"},
	{"serving_portfolios_history", "report_portfolios_history_multi()"},
	{"serving_global_history", "report_global_history_multi()"},
	{"serving_card_balances_history", "report_card_balances_history_multi()"},
	{"serving_global_latest", "report_global_multi(" + latestArg + ")"},
	{"serving_sources_latest", "report_sources_multi(" + latestArg + ")"},
	{"serving_portfolios_latest", "report_portfolios_multi(" + latestArg + ")"},
	{"serving_accounts_latest", "report_accounts_multi(" + latestArg + ")"},
	{"serving_positions_latest", "report_positions_multi(" + latestArg + ")"},
	{"serving_transactions", "report_transactions_multi(0, " + latestArg + ")"},
	{"serving_spending_transactions", "report_spending_transactions_multi(0, " + latestArg + ")"},
	{"serving_income_transactions", "report_income_transactions_multi(0, " + latestArg + ")"},
	{"serving_cashflow_transactions", "report_cashflow_transactions_multi(0, " + latestArg + ")"},
}

// MaterializeServingTables rewrites the tables the Metabase serving
// views read: the daily histories, the latest holdings and every
// transaction line, each its report macro's rows. A macro computes its
// whole output on every read, a history every day from the first
// snapshot, whatever a query asks for, so the dashboards read the
// output from a table instead. It returns the number of rows written.
// One transaction, so a reader sees the old tables or the new ones.
func MaterializeServingTables(ctx context.Context, db *sql.DB) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeServingTables begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	total := 0
	for _, s := range servingTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table); err != nil {
			return 0, fmt.Errorf("MaterializeServingTables %s: delete: %w", s.table, err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO `+s.table+` SELECT * FROM `+s.source)
		if err != nil {
			return 0, fmt.Errorf("MaterializeServingTables %s: %w", s.table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("MaterializeServingTables %s: rows: %w", s.table, err)
		}
		total += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeServingTables commit: %w", err)
	}
	return total, nil
}
