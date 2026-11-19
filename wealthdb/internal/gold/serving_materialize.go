package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// latestArg is the as-of a report macro reads as "the latest snapshot".
const latestArg = "9223372036854775807"

// servingTable is a table that holds a report macro's rows verbatim:
// source is the macro's call. A source that names todayArg reads today
// there (call).
type servingTable struct{ table, source string }

// todayArg stands, in a serving source, for the last second of the day
// the tables are materialized on.
const todayArg = "$today"

// call is the source with today, the last second of a day, in place of
// todayArg.
func (s servingTable) call(today int64) string {
	return strings.ReplaceAll(s.source, todayArg, strconv.FormatInt(today, 10))
}

// macro is the name of the macro the table's rows come from.
func (s servingTable) macro() string {
	name, _, _ := strings.Cut(s.source, "(")
	return name
}

// servingTables are the serving tables, each with the macro whose rows
// it holds verbatim. The list is their one registry: applyDerived gives
// each table its macro's columns, and MaterializeServingTables rewrites
// its rows.
var servingTables = []servingTable{
	{"serving_sources_history", "report_sources_history_multi()"},
	{"serving_accounts_history", "report_accounts_history_multi()"},
	{"serving_cash_history", "report_cash_history_multi()"},
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
	{"serving_marks", "report_marks_multi()"},
	{"serving_spending_vs_year", "report_spending_vs_year_multi(" + todayArg + ")"},
}

// servingMaterialized are the tables MaterializeServingTables rewrites.
var servingMaterialized = func() []string {
	names := make([]string, len(servingTables))
	for i, s := range servingTables {
		names[i] = s.table
	}
	return names
}()

// MaterializeServingTables rewrites the tables the Metabase serving
// views read (rewriteTables): the daily histories, the latest holdings
// and every transaction line, each its report macro's rows. A macro
// computes its whole output on every read, a history every day from
// the first snapshot, whatever a query asks for, so the dashboards read
// the output from a table instead. Of the params it reads ToEpoch, the
// last second of today, alone. It returns the number of rows written.
func MaterializeServingTables(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	return rewriteTables(ctx, db, "MaterializeServingTables", beginPinned, servingMaterialized, func(tx *sql.Tx) (int, error) {
		total := 0
		for _, s := range servingTables {
			res, err := tx.ExecContext(ctx, `INSERT INTO `+s.table+` SELECT * FROM `+s.call(p.ToEpoch))
			if err != nil {
				return 0, fmt.Errorf("MaterializeServingTables %s: %w", s.table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("MaterializeServingTables %s: rows: %w", s.table, err)
			}
			total += int(n)
		}
		return total, nil
	})
}
