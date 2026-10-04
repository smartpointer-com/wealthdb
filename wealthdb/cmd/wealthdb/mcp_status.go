package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// The MCP server's freshness reports. The CLI prints status and
// snapshots as prose for a reader at a terminal; a model reads a table
// better, so these two are tables with the same content. No silver
// path appears in either: the filesystem layout is not the model's
// business.

// statusRow is one configured source's freshness.
type statusRow struct {
	source, kind string
	st           *gold.SourceStatus // nil when the source has not been loaded
	failed       bool               // gold could not report on the source
	newData      string             // yes | no | unknown
}

func statusColumns() []columnSpec[statusRow] {
	count := func(f func(*gold.SourceStatus) int) func(statusRow) string {
		return func(r statusRow) string {
			if r.st == nil {
				return ""
			}
			return fmt.Sprintf("%d", f(r.st))
		}
	}
	date := func(f func(*gold.SourceStatus) int64) func(statusRow) string {
		return func(r statusRow) string {
			if r.st == nil || f(r.st) < 0 {
				return ""
			}
			return formatDate(f(r.st))
		}
	}
	return []columnSpec[statusRow]{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r statusRow) string { return r.source }},
		{Name: "kind", Align: output.AlignLeft, Extract: func(r statusRow) string { return r.kind }},
		{Name: "state", Align: output.AlignLeft, Extract: func(r statusRow) string {
			switch {
			case r.failed:
				return "error"
			case r.st == nil:
				return "not loaded"
			}
			return "loaded"
		}},
		{Name: "positions", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.PositionsCount })},
		{Name: "transactions", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.TransactionsCount })},
		{Name: "first_snapshot", Align: output.AlignLeft, Extract: date(func(s *gold.SourceStatus) int64 { return s.OldestSnapshotAt })},
		{Name: "latest_snapshot", Align: output.AlignLeft, Extract: date(func(s *gold.SourceStatus) int64 { return s.LatestSnapshotAt })},
		{Name: "latest_transaction", Align: output.AlignLeft, Extract: date(func(s *gold.SourceStatus) int64 { return s.LatestTransactionAt })},
		{Name: "last_loaded", Align: output.AlignLeft, Extract: func(r statusRow) string {
			if r.st == nil {
				return ""
			}
			return formatDateTime(r.st.LastLoadedAt)
		}},
		{Name: "new_data", Align: output.AlignLeft, Extract: func(r statusRow) string { return r.newData }},
		{Name: "uncategorized_spending", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.UncategorizedSpendCount })},
		{Name: "uncategorized_income", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.UncategorizedIncomeCount })},
		{Name: "other_kind", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.OtherTxKindCount })},
		{Name: "guessed_kind", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.GuessedTxKindCount })},
		{Name: "other_asset_class", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.OtherAssetClassCount })},
		{Name: "no_vehicle", Align: output.AlignRight, Extract: count(func(s *gold.SourceStatus) int { return s.MissingVehicleCount })},
	}
}

var defaultStatusColumns = []string{"silver_source", "kind", "state", "positions", "transactions",
	"first_snapshot", "latest_snapshot", "latest_transaction", "last_loaded", "new_data"}

// statusVerboseColumns are the counters status -v prints: what no
// adapter or enrichment pass could place.
var statusVerboseColumns = []string{"uncategorized_spending", "uncategorized_income",
	"other_kind", "guessed_kind", "other_asset_class", "no_vehicle"}

// statusReport is one row per configured source. verbose adds the
// taxonomy and enrichment counters to the default columns.
func statusReport(cfg *config.Config, verbose bool) *report {
	defaults := defaultStatusColumns
	if verbose {
		defaults = append(append([]string{}, defaultStatusColumns...), statusVerboseColumns...)
	}
	return newReport(statusColumns(), defaults, func(ctx context.Context, db *sql.DB) ([]statusRow, error) {
		out := make([]statusRow, 0, len(cfg.SilverSources))
		for i := range cfg.SilverSources {
			src := &cfg.SilverSources[i]
			out = append(out, sourceStatus(ctx, db, src, verbose))
		}
		return out, nil
	})
}

// sourceStatus reads one source's gold-side status and asks its silver
// whether a load would bring anything new. A silver that cannot be
// opened — one in WAL mode on a read-only mount, say — leaves new_data
// unknown rather than failing the row.
func sourceStatus(ctx context.Context, db *sql.DB, src *config.SilverSource, verbose bool) statusRow {
	r := statusRow{source: src.ID, kind: src.Kind, newData: "unknown"}
	st, err := gold.StatusForSource(ctx, db, src.ID, verbose)
	if err != nil {
		r.failed = true
		return r
	}
	r.st = st
	if st == nil {
		r.newData = ""
		return r
	}
	if silverSt, err := probeSilverStatus(ctx, src); err == nil {
		r.newData = "no"
		if silverSt.LatestChangeNumber > st.HighWatermark {
			r.newData = "yes"
		}
	}
	return r
}

// statusDetail is status for one source: its counts and ranges as
// key/value rows, then its most recent loads.
func statusDetail(ctx context.Context, db *sql.DB, cfg *config.Config, id string, verbose bool) (string, error) {
	src, ok := cfg.Lookup(id)
	if !ok {
		ids := make([]string, len(cfg.SilverSources))
		for i, s := range cfg.SilverSources {
			ids[i] = s.ID
		}
		return "", fmt.Errorf("status: no source %q; the sources are %s", id, strings.Join(ids, ", "))
	}
	row := sourceStatus(ctx, db, src, verbose)
	if row.failed {
		return "", fmt.Errorf("status: gold could not report on source %q", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status %s · %s\n", id, src.Kind)
	if row.st == nil {
		b.WriteString("not loaded yet: gold holds nothing from this source")
		return b.String(), nil
	}
	st := row.st
	kv := output.Table{Columns: []string{"field", "value"}, Aligns: []output.Alignment{output.AlignLeft, output.AlignLeft}}
	add := func(k, v string) { kv.Rows = append(kv.Rows, []string{k, v}) }
	add("first_loaded", formatDateTime(st.FirstLoadedAt))
	add("last_loaded", formatDateTime(st.LastLoadedAt))
	add("new_data", row.newData)
	add("positions", fmt.Sprintf("%d", st.PositionsCount))
	add("cash_balances", fmt.Sprintf("%d", st.CashBalancesCount))
	add("fx_rates", fmt.Sprintf("%d", st.FxRatesCount))
	add("transactions", fmt.Sprintf("%d", st.TransactionsCount))
	add("snapshots", formatRange(st.OldestSnapshotAt, st.LatestSnapshotAt))
	add("transaction_dates", formatRange(st.OldestTransactionAt, st.LatestTransactionAt))
	if verbose {
		add("uncategorized_spending", fmt.Sprintf("%d", st.UncategorizedSpendCount))
		add("uncategorized_income", fmt.Sprintf("%d", st.UncategorizedIncomeCount))
		add("excluded_unmapped", fmt.Sprintf("%d", st.ExcludedUnmappedCount))
		add("other_kind", fmt.Sprintf("%d", st.OtherTxKindCount))
		add("guessed_kind", fmt.Sprintf("%d", st.GuessedTxKindCount))
		add("other_asset_class", fmt.Sprintf("%d", st.OtherAssetClassCount))
		add("no_vehicle", fmt.Sprintf("%d", st.MissingVehicleCount))
		add("cashflow_excluded_by_kind", fmt.Sprintf("%d", st.CashflowExcludedByKindCount))
		add("cashflow_no_far_account", fmt.Sprintf("%d", st.CashflowNoFarAccountCount))
		for _, a := range st.PerKindActivity {
			add("latest_"+a.AccountKind, fmt.Sprintf("%d accounts · snapshot %s · transaction %s", a.Accounts,
				formatOptionalDate(a.LatestSnapshotAt), formatOptionalDate(a.LatestTransactionAt)))
		}
	}
	writeMarkdownTable(&b, kv)

	audit, err := gold.RecentLoadAudit(ctx, db, id, 5)
	if err != nil {
		return "", err
	}
	if len(audit) > 0 {
		b.WriteString("\nrecent loads, newest first\n")
		loads := output.Table{
			Columns: []string{"loaded_at", "window", "snapshots_loaded", "transactions_loaded"},
			Aligns:  []output.Alignment{output.AlignLeft, output.AlignLeft, output.AlignRight, output.AlignRight},
		}
		for _, r := range audit {
			loads.Rows = append(loads.Rows, []string{
				formatDateTime(r.LoadedAt / 1_000_000_000),
				formatDate(r.WindowStart) + ".." + formatDate(r.WindowEnd),
				fmt.Sprintf("%d", r.SnapshotsLoaded), fmt.Sprintf("%d", r.TransactionsLoaded),
			})
		}
		writeMarkdownTable(&b, loads)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// snapshotRow is one day a source has data for.
type snapshotRow struct {
	source, date string
	count        int // snapshots taken that day
}

func snapshotColumns() []columnSpec[snapshotRow] {
	return []columnSpec[snapshotRow]{
		{Name: "silver_source", Align: output.AlignLeft, Extract: func(r snapshotRow) string { return r.source }},
		{Name: "date", Align: output.AlignLeft, Extract: func(r snapshotRow) string { return r.date }},
		{Name: "snapshots", Align: output.AlignRight, Extract: func(r snapshotRow) string { return fmt.Sprintf("%d", r.count) }},
	}
}

// snapshotsReport is one row per source and day gold has data for,
// oldest first, intra-day snapshots counted on their day's row as
// `wealthdb snapshots` prints them. latestOnly keeps each source's
// newest day.
func snapshotsReport(latestOnly bool) *report {
	registry := snapshotColumns()
	return newReport(registry, columnNames(registry), func(ctx context.Context, db *sql.DB) ([]snapshotRow, error) {
		ids, err := gold.ListSilverSources(ctx, db)
		if err != nil {
			return nil, err
		}
		var out []snapshotRow
		for _, id := range ids {
			times, err := gold.ListSnapshotTimes(ctx, db, id)
			if err != nil {
				return nil, err
			}
			var days []snapshotRow
			for _, t := range times {
				d := formatDate(t)
				if n := len(days); n > 0 && days[n-1].date == d {
					days[n-1].count++
					continue
				}
				days = append(days, snapshotRow{source: id, date: d, count: 1})
			}
			if latestOnly && len(days) > 0 {
				days = days[len(days)-1:]
			}
			out = append(out, days...)
		}
		return out, nil
	})
}
