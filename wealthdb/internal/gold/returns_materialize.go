package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// materializeCurrencies is the output-currency set report_returns carries —
// the same trio the `_multi` report macros emit. A fourth currency is a
// deliberate schema decision (every partition triples the table), not a
// config knob.
var materializeCurrencies = []string{"USD", "CHF", "EUR"}

// materializeGrains and materializePeriods span the full RunReturns matrix;
// together with materializeCurrencies each combination is one table partition.
var (
	materializeGrains  = []string{"accounts", "portfolios", "sources", "global"}
	materializePeriods = []string{"monthly", "quarterly", "annual", "total"}
)

// MaterializeParams configures a MaterializeReturns run. ToEpoch is the
// window end (Unix seconds; inception → ToEpoch, the CLI's default window);
// ComputedAt is stamped on every row so readers can tell how fresh the run
// is. InceptionOverrides / ReturnsExclude carry the same wealthdb.cfg
// settings a CLI run applies.
type MaterializeParams struct {
	ToEpoch            int64
	ComputedAt         int64
	InceptionOverrides *InceptionOverrides
	ReturnsExclude     *ReturnsExclude
}

// MaterializeReturns rewrites the report_returns table: one RunReturns call
// per (grain, granularity, currency) partition with the CLI-default knobs
// (method both, netting on, inception full, annualize auto, since-inception
// window), rows inserted verbatim. All partitions are computed first, then
// written in a single transaction (DELETE all + INSERT all), so a failed run
// leaves the previous materialization intact. Returns the inserted row count.
func MaterializeReturns(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	type partition struct {
		grain, granularity, currency string
		rows                         []ReturnRow
	}
	var parts []partition
	for _, ccy := range materializeCurrencies {
		for _, grain := range materializeGrains {
			for _, period := range materializePeriods {
				rows, err := RunReturns(ctx, db, ReturnParams{
					Level: grain, FromEpoch: 0, ToEpoch: p.ToEpoch, OutCcy: ccy,
					Method: "both", Period: period, Annualize: "auto",
					Netting: true, Inception: "full",
					InceptionOverrides: p.InceptionOverrides,
					ReturnsExclude:     p.ReturnsExclude,
				})
				if err != nil {
					return 0, fmt.Errorf("MaterializeReturns (%s, %s, %s): %w", grain, period, ccy, err)
				}
				parts = append(parts, partition{grain: grain, granularity: period, currency: ccy, rows: rows})
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MaterializeReturns begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM report_returns`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("MaterializeReturns delete: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO report_returns (
    computed_at, currency, grain, granularity,
    silver_source_id, entity_id, entity_label, period, is_summary,
    start_day, end_day, start_value, end_value, net_flow,
    twr, twr_annualized, mwr, mwr_annualized, quality
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("MaterializeReturns prepare: %w", err)
	}
	n := 0
	for _, part := range parts {
		for _, r := range part.rows {
			// Money columns bind the engine's decimal strings as-is (DuckDB
			// casts to DECIMAL(28,4)); nil stays NULL. StartDay/EndDay are
			// epoch DAYS on ReturnRow — stored as epoch seconds, the repo
			// convention for timestamp columns.
			if _, err := stmt.ExecContext(ctx,
				p.ComputedAt, part.currency, part.grain, part.granularity,
				r.SilverSourceID, r.EntityID, r.EntityLabel, r.Period, r.IsSummary,
				r.StartDay*86400, r.EndDay*86400, r.StartValue, r.EndValue, r.NetFlow,
				r.TWR, r.TWRAnnualized, r.MWR, r.MWRAnnualized,
				strings.Join(r.Quality, ";"),
			); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return 0, fmt.Errorf("MaterializeReturns insert (%s, %s, %s): %w",
					part.grain, part.granularity, part.currency, err)
			}
			n++
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("MaterializeReturns close stmt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeReturns commit: %w", err)
	}
	return n, nil
}
