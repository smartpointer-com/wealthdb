package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// materializeCurrencies is the output-currency set report_returns carries —
// the reporting currencies the `_multi` report macros emit,
// loaded in one pass. Another currency is a schema decision rather than a
// config knob: the macros must emit its columns, and every currency adds a
// full set of partitions to the table. web/test_provision.py reads this
// literal and holds it to the macros and the dashboards' list.
var materializeCurrencies = []string{"USD", "CHF", "EUR", "GBP"}

// MaterializedCurrencies returns the currencies report_returns carries, in
// materialization order.
func MaterializedCurrencies() []string {
	return append([]string(nil), materializeCurrencies...)
}

// multiColumns is `CAST(<prefix>_<ccy> AS VARCHAR)` for each materialized
// currency, in materializeCurrencies order: the `_multi` value columns a
// loader scans.
func multiColumns(prefix string) string {
	cols := make([]string, len(materializeCurrencies))
	for i, ccy := range materializeCurrencies {
		cols[i] = fmt.Sprintf("CAST(%s_%s AS VARCHAR)", prefix, strings.ToLower(ccy))
	}
	return strings.Join(cols, ", ")
}

// materializeGrains (every grain, finest first) and materializePeriods span
// the full RunReturns matrix; together with materializeCurrencies each
// combination is one table partition.
var (
	materializeGrains  = grainNames()
	materializePeriods = []string{"monthly", "quarterly", "annual", "total"}
)

// MaterializeParams configures a web-materialize run: every phase takes
// it, and each reads the fields it needs. ToEpoch is the window end
// (Unix seconds; inception → ToEpoch, the CLI's default window);
// ComputedAt is stamped on every row so readers can tell how fresh the
// run is. ReturnsSettings are the same wealthdb.cfg returns settings a
// CLI run applies (returnsCfgSettings). CashflowLookback is
// the cash flow projection's look-back in months (MaterializeYield).
type MaterializeParams struct {
	ToEpoch    int64
	ComputedAt int64
	ReturnsSettings
	CashflowLookback int
}

// beginTx begins a transaction of db and returns the function that
// ends it by commit: beginPinned, or beginPlain.
type beginTx func(ctx context.Context, db *sql.DB) (*sql.Tx, func() error, error)

// beginPlain is beginPinned without the pin, for a phase whose fill
// only writes rows it computed before: the pin's reads would be pure
// cost.
func beginPlain(ctx context.Context, db *sql.DB) (*sql.Tx, func() error, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin: %w", err)
	}
	return tx, func() error {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}, nil
}

// rewriteTables replaces the rows of tables in one transaction, begun
// by begin: it deletes every table's rows, then fill writes the new
// ones and returns how many. A reader sees the old tables or the new
// ones, and a failed run leaves the old ones. A fill that reads gold at
// many instants begins pinned (beginPinned). op prefixes the frame's
// errors; fill's errors pass as they are.
func rewriteTables(ctx context.Context, db *sql.DB, op string, begin beginTx, tables []string,
	fill func(*sql.Tx) (int, error)) (int, error) {
	tx, commit, err := begin(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after commit
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return 0, fmt.Errorf("%s delete %s: %w", op, t, err)
		}
	}
	n, err := fill(tx)
	if err != nil {
		return 0, err
	}
	if err := commit(); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return n, nil
}

// returnsMaterialized are the tables MaterializeReturns rewrites.
var returnsMaterialized = []string{"report_returns", "benchmark_returns", "benchmark_mwr", "report_bridge",
	"report_private_markets"}

// MaterializeReturns rewrites report_returns, the benchmark tables, the
// bridge and the private-markets rows. It writes the full returns matrix —
// every grain × period × currency, each partition the verbatim output of one
// CLI-default RunReturns (method both, netting on, inception full, annualize
// auto, since-inception window, window_from_year=0) — plus, for each grain ×
// currency × year in the data's span, a since-that-year total summary
// (window_from_year=Y) for the dashboard's start-year rescoping. The
// currencies are loaded in a single pass over the `_multi` report macros, and
// each currency's dataset drives every (grain, period) and windowed
// computation without re-querying — the loaded data depends only on the
// currency, never the grain, period or window. The benchmark tables
// (computeBenchmarks) and the bridge (computeMaterialBridge) read the same
// datasets; the private-markets rows (computeMaterialPrivate) are
// RunPrivate's in each currency. Everything is computed before the tables are
// rewritten (rewriteTables), so the write needs no pin. Returns the row count
// of report_returns and the benchmark tables.
func MaterializeReturns(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("MaterializeReturns fx: %w", err)
	}
	datasets, err := loadReturnsDatasets(ctx, db, everyCurrency(), fx, p.ReturnsSettings)
	if err != nil {
		return 0, err
	}

	var parts []returnsPartition
	// Base matrix: every (grain, period, currency) partition, since inception
	// (window_from_year = 0).
	for _, ccy := range materializeCurrencies {
		ds := datasets[ccy]
		for _, grain := range materializeGrains {
			for _, period := range materializePeriods {
				rows := computeReturns(ds, p.defaultRun(grain, period, ccy, 0, p.ToEpoch))
				parts = append(parts, returnsPartition{grain: grain, granularity: period, currency: ccy, rows: rows})
			}
		}
	}
	// Windowed summaries: for each start year in the data's span, a since-that-
	// year total per grain and currency, so the dashboard can rescope the
	// scalars past the degenerate inception period. Cheap — the datasets are
	// already loaded, so each is just another in-memory computeReturns with a
	// later FromEpoch (period 'total' emits only the summary row).
	minYear, maxYear := datasetYearRange(datasets, p.ToEpoch)
	for _, ccy := range materializeCurrencies {
		ds := datasets[ccy]
		for _, grain := range materializeGrains {
			for y := minYear; y <= maxYear; y++ {
				from := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
				rows := computeReturns(ds, p.defaultRun(grain, "total", ccy, from, p.ToEpoch))
				parts = append(parts, returnsPartition{grain: grain, granularity: "total", currency: ccy, windowFromYear: y, rows: rows})
			}
		}
	}

	globals := map[[2]string][]ReturnRow{}
	for _, part := range parts {
		if part.grain == grainGlobal.name && part.windowFromYear == 0 {
			globals[[2]string{part.currency, part.granularity}] = part.rows
		}
	}
	bench, err := computeBenchmarks(ctx, db, datasets, fx, p, minYear, maxYear, globals)
	if err != nil {
		return 0, err
	}
	bridge, err := computeMaterialBridge(ctx, db, datasets, p)
	if err != nil {
		return 0, err
	}
	private, err := computeMaterialPrivate(ctx, db)
	if err != nil {
		return 0, err
	}

	return rewriteTables(ctx, db, "MaterializeReturns", beginPlain, returnsMaterialized, func(tx *sql.Tx) (int, error) {
		n, err := writeReturnRows(ctx, tx, parts, p.ComputedAt)
		if err != nil {
			return 0, fmt.Errorf("MaterializeReturns: %w", err)
		}
		k, err := bench.write(ctx, tx, p.ComputedAt)
		if err != nil {
			return 0, fmt.Errorf("MaterializeReturns: %w", err)
		}
		if err := bridge.write(ctx, tx, p.ComputedAt); err != nil {
			return 0, fmt.Errorf("MaterializeReturns: %w", err)
		}
		if err := private.write(ctx, tx, p.ComputedAt); err != nil {
			return 0, fmt.Errorf("MaterializeReturns: %w", err)
		}
		return n + k, nil
	})
}

// returnsPartition is one RunReturns output: its rows and the key
// report_returns files them under. windowFromYear is 0 for the base
// matrix and the start year for windowed summaries.
type returnsPartition struct {
	grain, granularity, currency string
	windowFromYear               int
	rows                         []ReturnRow
}

const returnsCols = `computed_at, currency, grain, granularity, window_from_year,
    silver_source_id, entity_id, entity_label, period, is_summary,
    start_day, end_day, start_value, end_value, net_flow,
    twr, twr_annualized, mwr, mwr_annualized, quality`

// writeReturnRows inserts every partition's rows into report_returns, in
// partition order, and returns how many. Money columns carry the
// engine's decimal strings, cast to DECIMAL(28,4); nil stays NULL.
// StartDay/EndDay are epoch DAYS on ReturnRow, stored as epoch seconds,
// the repo convention for timestamp columns.
func writeReturnRows(ctx context.Context, tx *sql.Tx, parts []returnsPartition, computedAt int64) (int, error) {
	type ref struct{ part, row int }
	var refs []ref
	for i, part := range parts {
		for j := range part.rows {
			refs = append(refs, ref{i, j})
		}
	}
	if len(refs) == 0 {
		return 0, nil
	}
	err := insertColumns(ctx, tx, "report_returns", "report_returns", returnsCols, len(refs),
		func(i int, args []any) []any {
			part := &parts[refs[i].part]
			r := &part.rows[refs[i].row]
			return append(args,
				computedAt, part.currency, part.grain, part.granularity, part.windowFromYear,
				r.SilverSourceID, r.EntityID, r.EntityLabel, r.Period, r.IsSummary,
				r.StartDay*SecondsPerDay, r.EndDay*SecondsPerDay, r.StartValue, r.EndValue, r.NetFlow,
				r.TWR, r.TWRAnnualized, r.MWR, r.MWRAnnualized,
				strings.Join(r.Quality, ";"))
		})
	return len(refs), err
}

// datasetYearRange is the span of start years to materialize windowed summaries
// for: the year of the earliest value across all loaded accounts through the
// year of the window end (toEpoch, ≈ today). Returns an empty range (min > max)
// when there is no data, so the windowed loop is skipped.
func datasetYearRange(datasets map[string]*returnsDataset, toEpoch int64) (int, int) {
	minDay := int64(-1)
	for _, ds := range datasets {
		for _, a := range ds.accts {
			if len(a.series) == 0 {
				continue
			}
			if d := a.series[0].day; minDay < 0 || d < minDay {
				minDay = d
			}
		}
	}
	if minDay < 0 {
		return 1, 0 // no data → empty range
	}
	minYear := time.Unix(minDay*SecondsPerDay, 0).UTC().Year()
	maxYear := time.Unix(toEpoch, 0).UTC().Year()
	return minYear, maxYear
}

// everyCurrency reads the `_multi` macros, in every materialized currency.
func everyCurrency() spineSource {
	return spineSource{currencies: materializeCurrencies, op: "MaterializeReturns",
		history: `SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id, ` + multiColumns("total_value") + `
		   FROM report_accounts_history_multi()
		  ORDER BY silver_source_id, account_external_id, as_of_day`,
		transactions: `SELECT r.silver_source_id, r.account_external_id, r.occurred_at, r.kind,
		        r.currency, r.transaction_external_id, ` + multiColumns("r.value") +
			fmt.Sprintf(flowTxnJoin, "report_transactions_multi(?, ?)"),
		transactionsArgs: []any{int64(0), MaxEpoch}, decimal: nullStringToPtr}
}
