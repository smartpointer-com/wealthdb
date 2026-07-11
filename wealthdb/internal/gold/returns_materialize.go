package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// materializeCurrencies is the output-currency set report_returns carries —
// the same trio the `_multi` report macros emit, loaded in one pass. A fourth
// currency is a deliberate schema decision (every partition triples the table),
// not a config knob.
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

// MaterializeReturns rewrites the report_returns table with the full returns
// matrix — 4 grains × 4 periods × 3 currencies, each partition the verbatim
// output of one CLI-default RunReturns (method both, netting on, inception
// full, annualize auto, since-inception window). The three currencies are
// loaded in a single pass over the `_multi` report macros, and each currency's
// dataset drives all 16 (grain, period) computations without re-querying — the
// loaded data depends only on the currency, never the grain or period. Rows are
// written in one transaction (DELETE all + batched INSERT), so a failed run
// leaves the previous materialization intact. Returns the inserted row count.
func MaterializeReturns(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("MaterializeReturns fx: %w", err)
	}
	datasets, err := loadReturnsDatasetsMulti(ctx, db, fx)
	if err != nil {
		return 0, err
	}

	type partition struct {
		grain, granularity, currency string
		rows                         []ReturnRow
	}
	var parts []partition
	for _, ccy := range materializeCurrencies {
		ds := datasets[ccy]
		for _, grain := range materializeGrains {
			for _, period := range materializePeriods {
				rows := computeReturns(ds, ReturnParams{
					Level: grain, FromEpoch: 0, ToEpoch: p.ToEpoch, OutCcy: ccy,
					Method: "both", Period: period, Annualize: "auto",
					Netting: true, Inception: "full",
					InceptionOverrides: p.InceptionOverrides,
					ReturnsExclude:     p.ReturnsExclude,
				})
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
	n := 0
	// Batched multi-row INSERT: one round trip per returnsInsertBatch rows
	// instead of per row. A prepared full-size statement handles the whole
	// runs of full batches; a final short statement flushes the remainder.
	full, err := tx.PrepareContext(ctx, returnsInsertSQL(returnsInsertBatch))
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("MaterializeReturns prepare: %w", err)
	}
	args := make([]any, 0, returnsInsertBatch*returnsInsertCols)
	rowsInBatch := 0
	for _, part := range parts {
		for _, r := range part.rows {
			args = appendReturnRow(args, p.ComputedAt, part.currency, part.grain, part.granularity, r)
			rowsInBatch++
			n++
			if rowsInBatch == returnsInsertBatch {
				if _, err := full.ExecContext(ctx, args...); err != nil {
					_ = full.Close()
					_ = tx.Rollback()
					return 0, fmt.Errorf("MaterializeReturns insert: %w", err)
				}
				args = args[:0]
				rowsInBatch = 0
			}
		}
	}
	if err := full.Close(); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("MaterializeReturns close stmt: %w", err)
	}
	if rowsInBatch > 0 {
		if _, err := tx.ExecContext(ctx, returnsInsertSQL(rowsInBatch), args...); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("MaterializeReturns insert remainder: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MaterializeReturns commit: %w", err)
	}
	return n, nil
}

// returnsInsertBatch is the number of rows per multi-row INSERT (× the 19
// columns = params per statement, well under DuckDB's limit).
const returnsInsertBatch = 128

// returnsInsertCols is report_returns' column count (keep in sync with the
// column list and appendReturnRow).
const returnsInsertCols = 19

const returnsInsertColumns = `report_returns (
    computed_at, currency, grain, granularity,
    silver_source_id, entity_id, entity_label, period, is_summary,
    start_day, end_day, start_value, end_value, net_flow,
    twr, twr_annualized, mwr, mwr_annualized, quality
)`

// returnsInsertSQL builds an INSERT with n value tuples (n × returnsInsertCols
// placeholders).
func returnsInsertSQL(n int) string {
	group := "(" + strings.Repeat("?, ", returnsInsertCols-1) + "?)"
	groups := make([]string, n)
	for i := range groups {
		groups[i] = group
	}
	return "INSERT INTO " + returnsInsertColumns + " VALUES " + strings.Join(groups, ", ")
}

// appendReturnRow appends one row's bind values (in the column order above) to
// dst. Money columns bind the engine's decimal strings as-is (DuckDB casts to
// DECIMAL(28,4)); nil stays NULL. StartDay/EndDay are epoch DAYS on ReturnRow —
// stored as epoch seconds, the repo convention for timestamp columns.
func appendReturnRow(dst []any, computedAt int64, currency, grain, granularity string, r ReturnRow) []any {
	return append(dst,
		computedAt, currency, grain, granularity,
		r.SilverSourceID, r.EntityID, r.EntityLabel, r.Period, r.IsSummary,
		r.StartDay*86400, r.EndDay*86400, r.StartValue, r.EndValue, r.NetFlow,
		r.TWR, r.TWRAnnualized, r.MWR, r.MWRAnnualized,
		strings.Join(r.Quality, ";"))
}

// loadReturnsDatasetsMulti loads all three currencies' datasets in a single
// pass over each `_multi` macro: one scan of report_accounts_history_multi for
// the daily value spines and one of report_transactions_multi for the flows,
// versus three scans each if loaded per currency. Currency-independent inputs
// (snapshot days, source kinds, portfolio names, fx) are read once and shared.
// The per-currency result is bit-identical to loadReturnsDataset(ccy).
func loadReturnsDatasetsMulti(ctx context.Context, db *sql.DB, fx fxBounds) (map[string]*returnsDataset, error) {
	kinds, err := loadSourceKinds(ctx, db)
	if err != nil {
		return nil, err
	}
	pfNames, err := loadPortfolioNames(ctx, db)
	if err != nil {
		return nil, err
	}

	byCcy := make(map[string]map[string]*accountData, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		byCcy[ccy] = map[string]*accountData{}
	}

	// The daily value spine in USD/CHF/EUR at once. The row set is currency-
	// independent (shared spine); only the value columns and their NULL-ness
	// differ per currency, so appendSeries' skip-if-NULL reproduces each
	// single-currency series exactly.
	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id,
		        CAST(total_value_usd AS VARCHAR), CAST(total_value_chf AS VARCHAR),
		        CAST(total_value_eur AS VARCHAR)
		   FROM report_accounts_history_multi()
		  ORDER BY silver_source_id, account_external_id, as_of_day`)
	if err != nil {
		return nil, fmt.Errorf("MaterializeReturns history: %w", err)
	}
	for rows.Next() {
		var (
			asOf                   int64
			src, acct, kind        string
			label, base, pf        sql.NullString
			totUSD, totCHF, totEUR sql.NullString
		)
		if err := rows.Scan(&asOf, &src, &acct, &kind, &label, &base, &pf,
			&totUSD, &totCHF, &totEUR); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("MaterializeReturns history scan: %w", err)
		}
		day := asOf / 86400
		appendSeries(byCcy["USD"], kinds, pfNames, src, acct, kind, label, base, pf, day, totUSD)
		appendSeries(byCcy["CHF"], kinds, pfNames, src, acct, kind, label, base, pf, day, totCHF)
		appendSeries(byCcy["EUR"], kinds, pfNames, src, acct, kind, label, base, pf, day, totEUR)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Snapshot days are currency-independent: one scan, distributed to all
	// three maps.
	maps := make([]map[string]*accountData, 0, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		maps = append(maps, byCcy[ccy])
	}
	if err := loadSnapshotDays(ctx, db, maps...); err != nil {
		return nil, err
	}

	// Flows in USD/CHF/EUR at once, distributed per currency exactly as
	// attachFlows would for each.
	txns, err := loadTransactionsMulti(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, t := range txns {
		attachOneFlow(byCcy["USD"], fx, "USD", t.src, t.acct, t.kind, t.occurredAt, t.txID, t.ccy, t.valUSD)
		attachOneFlow(byCcy["CHF"], fx, "CHF", t.src, t.acct, t.kind, t.occurredAt, t.txID, t.ccy, t.valCHF)
		attachOneFlow(byCcy["EUR"], fx, "EUR", t.src, t.acct, t.kind, t.occurredAt, t.txID, t.ccy, t.valEUR)
	}

	out := make(map[string]*returnsDataset, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		ds := &returnsDataset{outCcy: ccy, accts: byCcy[ccy], fx: fx}
		ds.finalize()
		out[ccy] = ds
	}
	return out, nil
}

// txnMultiRow is one transaction with its net amount converted to USD/CHF/EUR
// in a single pass (report_transactions_multi), for the multi-currency flow
// loader. Only the fields attachOneFlow needs are carried.
type txnMultiRow struct {
	src, acct, txID, ccy   string
	kind                   canonical.TxKind
	occurredAt             int64
	valUSD, valCHF, valEUR *string
}

// loadTransactionsMulti loads every transaction once with its net amount in
// USD/CHF/EUR, ordered like report_transactions so per-currency flow append
// order matches the single-currency attachFlows.
func loadTransactionsMulti(ctx context.Context, db *sql.DB) ([]txnMultiRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT silver_source_id, account_external_id, occurred_at, kind, currency,
		        transaction_external_id,
		        CAST(value_usd AS VARCHAR), CAST(value_chf AS VARCHAR), CAST(value_eur AS VARCHAR)
		   FROM report_transactions_multi(?, ?)
		  ORDER BY occurred_at, silver_source_id, transaction_external_id`, int64(0), maxEpoch)
	if err != nil {
		return nil, fmt.Errorf("MaterializeReturns transactions: %w", err)
	}
	defer rows.Close()
	var out []txnMultiRow
	for rows.Next() {
		var (
			r                txnMultiRow
			kind             string
			vUSD, vCHF, vEUR sql.NullString
		)
		if err := rows.Scan(&r.src, &r.acct, &r.occurredAt, &kind, &r.ccy, &r.txID,
			&vUSD, &vCHF, &vEUR); err != nil {
			return nil, fmt.Errorf("MaterializeReturns transactions scan: %w", err)
		}
		r.kind = canonical.TxKind(kind)
		r.valUSD = nullStringToPtr(vUSD)
		r.valCHF = nullStringToPtr(vCHF)
		r.valEUR = nullStringToPtr(vEUR)
		out = append(out, r)
	}
	return out, rows.Err()
}
