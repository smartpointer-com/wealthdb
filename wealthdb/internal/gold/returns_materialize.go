package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// materializeCurrencies is the output-currency set report_returns carries —
// the reporting currencies the `_multi` report macros emit (migration 0114),
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

// materializeGrains and materializePeriods span the full RunReturns matrix;
// together with materializeCurrencies each combination is one table partition.
var (
	materializeGrains  = []string{"accounts", "portfolios", "sources", "global"}
	materializePeriods = []string{"monthly", "quarterly", "annual", "total"}
)

// MaterializeParams configures a MaterializeReturns run. ToEpoch is the
// window end (Unix seconds; inception → ToEpoch, the CLI's default window);
// ComputedAt is stamped on every row so readers can tell how fresh the run
// is. The remaining fields carry the same wealthdb.cfg returns settings a
// CLI run applies (returnsCfgSettings).
type MaterializeParams struct {
	ToEpoch            int64
	ComputedAt         int64
	InceptionOverrides *InceptionOverrides
	ReturnsExclude     *ReturnsExclude
	ReturnsHide        *ReturnsHide
	PolicyOverrides    map[string]ReturnsPolicyOverride
	TransferMatching   *TransferMatching
}

// MaterializeReturns rewrites the report_returns table. It writes the full
// returns matrix — every grain × period × currency, each partition the
// verbatim output of one CLI-default RunReturns (method both, netting on,
// inception full, annualize auto, since-inception window, window_from_year=0)
// — plus, for each grain × currency × year in the data's span, a since-that-
// year total summary (window_from_year=Y) for the dashboard's start-year
// rescoping. The currencies are loaded in a single pass over the `_multi`
// report macros, and each currency's dataset drives every (grain, period) and
// windowed computation without re-querying — the loaded data depends only on
// the currency, never the grain, period or window. Rows are written in one
// transaction (DELETE all + batched INSERT), so a failed run leaves the
// previous materialization intact. Returns the inserted row count.
func MaterializeReturns(ctx context.Context, db *sql.DB, p MaterializeParams) (int, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("MaterializeReturns fx: %w", err)
	}
	datasets, err := loadReturnsDatasetsMulti(ctx, db, fx, p.PolicyOverrides, p.TransferMatching)
	if err != nil {
		return 0, err
	}

	type partition struct {
		grain, granularity, currency string
		windowFromYear               int
		rows                         []ReturnRow
	}
	var parts []partition
	// Base matrix: every (grain, period, currency) partition, since inception
	// (window_from_year = 0).
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
					ReturnsHide:        p.ReturnsHide,
				})
				parts = append(parts, partition{grain: grain, granularity: period, currency: ccy, rows: rows})
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
				rows := computeReturns(ds, ReturnParams{
					Level: grain, FromEpoch: from, ToEpoch: p.ToEpoch, OutCcy: ccy,
					Method: "both", Period: "total", Annualize: "auto",
					Netting: true, Inception: "full",
					InceptionOverrides: p.InceptionOverrides,
					ReturnsExclude:     p.ReturnsExclude,
					ReturnsHide:        p.ReturnsHide,
				})
				parts = append(parts, partition{grain: grain, granularity: "total", currency: ccy, windowFromYear: y, rows: rows})
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
			args = appendReturnRow(args, p.ComputedAt, part.currency, part.grain, part.granularity, part.windowFromYear, r)
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

// returnsInsertBatch is the number of rows per multi-row INSERT (× the 20
// columns = params per statement, well under DuckDB's limit).
const returnsInsertBatch = 128

// returnsInsertCols is report_returns' column count (keep in sync with the
// column list and appendReturnRow).
const returnsInsertCols = 20

const returnsInsertColumns = `report_returns (
    computed_at, currency, grain, granularity, window_from_year,
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
// windowFromYear is 0 for the base matrix and the start year for windowed
// summaries.
func appendReturnRow(dst []any, computedAt int64, currency, grain, granularity string, windowFromYear int, r ReturnRow) []any {
	return append(dst,
		computedAt, currency, grain, granularity, windowFromYear,
		r.SilverSourceID, r.EntityID, r.EntityLabel, r.Period, r.IsSummary,
		r.StartDay*86400, r.EndDay*86400, r.StartValue, r.EndValue, r.NetFlow,
		r.TWR, r.TWRAnnualized, r.MWR, r.MWRAnnualized,
		strings.Join(r.Quality, ";"))
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
	minYear := time.Unix(minDay*86400, 0).UTC().Year()
	maxYear := time.Unix(toEpoch, 0).UTC().Year()
	return minYear, maxYear
}

// loadReturnsDatasetsMulti loads every materialized currency's dataset in a
// single pass over each `_multi` macro: one scan of
// report_accounts_history_multi for the daily value spines and one of
// report_transactions_multi for the flows, versus one scan per currency of
// each. Currency-independent inputs (snapshot days, source kinds, portfolio
// names, fx) are read once and shared. The per-currency result is
// bit-identical to loadReturnsDataset(ccy).
func loadReturnsDatasetsMulti(ctx context.Context, db *sql.DB, fx fxBounds, ov map[string]ReturnsPolicyOverride, tm *TransferMatching) (map[string]*returnsDataset, error) {
	kinds, err := SourceKinds(ctx, db)
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

	// The daily value spine in every currency at once. The row set is
	// currency-independent (shared spine); only the value columns and their
	// NULL-ness differ per currency, so appendSeries' skip-if-NULL reproduces
	// each single-currency series exactly.
	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id, `+multiColumns("total_value")+`
		   FROM report_accounts_history_multi()
		  ORDER BY silver_source_id, account_external_id, as_of_day`)
	if err != nil {
		return nil, fmt.Errorf("MaterializeReturns history: %w", err)
	}
	var (
		asOf            int64
		src, acct, kind string
		label, base, pf sql.NullString
	)
	tots := make([]sql.NullString, len(materializeCurrencies))
	dest := []any{&asOf, &src, &acct, &kind, &label, &base, &pf}
	for i := range tots {
		dest = append(dest, &tots[i])
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("MaterializeReturns history scan: %w", err)
		}
		day := asOf / 86400
		for i, ccy := range materializeCurrencies {
			appendSeries(byCcy[ccy], kinds, pfNames, ov, src, acct, kind, label, base, pf, day, tots[i])
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Snapshot days are currency-independent: one scan, distributed to every
	// currency's map.
	maps := make([]map[string]*accountData, 0, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		maps = append(maps, byCcy[ccy])
	}
	if err := loadSnapshotDays(ctx, db, maps...); err != nil {
		return nil, err
	}

	// Flows in every currency at once, distributed per currency exactly as
	// attachFlows would for each.
	txns, err := loadTransactionsMulti(ctx, db)
	if err != nil {
		return nil, err
	}
	// Transfer-match candidates are collected per currency (the attached
	// universe can differ where FX is unresolved) but carry NATIVE amounts, so
	// every partition derives the same pairings for the legs it holds.
	cands := map[string][]crossCandidate{}
	collect := func(ccy string, attached bool, t txnMultiRow) {
		if attached && tm != nil {
			cands[ccy] = appendCrossCandidate(cands[ccy], t.src, t.acct, t.txID, t.ccy, t.occurredAt, t.netAmt)
		}
	}
	for _, t := range txns {
		for i, ccy := range materializeCurrencies {
			collect(ccy, attachOneFlow(byCcy[ccy], fx, ccy, t.src, t.acct, t.kind, t.occurredAt, t.txID, t.ccy, t.vals[i], t.returnsInternal), t)
		}
	}
	for _, ccy := range materializeCurrencies {
		matchCrossTransfers(cands[ccy], tm, byCcy[ccy])
	}

	out := make(map[string]*returnsDataset, len(materializeCurrencies))
	for _, ccy := range materializeCurrencies {
		ds := &returnsDataset{accts: byCcy[ccy], fx: fx}
		ds.finalize()
		out[ccy] = ds
	}
	return out, nil
}

// txnMultiRow is one transaction with its net amount converted to every
// materialized currency in a single pass (report_transactions_multi), for the
// multi-currency flow loader. Only the fields attachOneFlow and the transfer
// matcher read are carried.
type txnMultiRow struct {
	src, acct, txID, ccy string
	kind                 canonical.TxKind
	occurredAt           int64
	vals                 []*string // in materializeCurrencies order
	netAmt               *string   // native-currency net amount (transfer matching)
	// returnsInternal: the adapter's conduit verdict, as in flowTxnRow.
	returnsInternal bool
}

// loadTransactionsMulti loads every transaction once with its net amount in
// every materialized currency, ordered like report_transactions so
// per-currency flow append order matches the single-currency attachFlows.
func loadTransactionsMulti(ctx context.Context, db *sql.DB) ([]txnMultiRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT r.silver_source_id, r.account_external_id, r.occurred_at, r.kind,
		        r.currency, r.transaction_external_id, `+multiColumns("r.value")+`,
		        CAST(r.net_amount AS VARCHAR),
		        COALESCE(t.payload ->> 'returns_flow' = 'internal', FALSE) AS returns_internal
		   FROM report_transactions_multi(?, ?) r
		   LEFT JOIN transactions t
		          ON t.silver_source_id        = r.silver_source_id
		         AND t.transaction_external_id = r.transaction_external_id
		  ORDER BY r.occurred_at, r.silver_source_id, r.transaction_external_id`, int64(0), MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("MaterializeReturns transactions: %w", err)
	}
	defer rows.Close()
	var (
		out    []txnMultiRow
		cur    txnMultiRow
		kind   string
		netAmt sql.NullString
	)
	vals := make([]sql.NullString, len(materializeCurrencies))
	dest := []any{&cur.src, &cur.acct, &cur.occurredAt, &kind, &cur.ccy, &cur.txID}
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	dest = append(dest, &netAmt, &cur.returnsInternal)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("MaterializeReturns transactions scan: %w", err)
		}
		r := cur
		r.kind = canonical.TxKind(kind)
		r.vals = make([]*string, len(vals))
		for i, v := range vals {
			r.vals[i] = nullStringToPtr(v)
		}
		r.netAmt = nullStringToPtr(netAmt)
		out = append(out, r)
	}
	return out, rows.Err()
}
