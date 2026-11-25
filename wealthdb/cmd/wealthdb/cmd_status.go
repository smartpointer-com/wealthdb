package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

func init() {
	register("status", cmdStatus)
}

func cmdStatus(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	verbose := fs.Bool("v", false, "verbose: include taxonomy-drift, enrichment-backlog and per-account-kind counts")
	marks := fs.Bool("marks", false, "the share of value marked within five days of each month end")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb status [<silver_source_id>] [-v]
       wealthdb status --marks [FROM [TO]] [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

Without <id>: one line per silver source in the config — kind,
path, gold-side counts, the date its holdings were last observed,
watermark, silver-side change number, and a * if a
`+"`wealthdb load`"+` would bring in new data.

With <id>: a detailed per-source block — counts per gold table,
silver Status() extrema, when the source's holdings and its ledger
were last observed, the stored watermark, and the most recent
load_audit rows (window=[all] for a load that re-read the whole
history). A source is observed when a run reads it, whether
or not anything changed: a portal that marks quarterly is observed
through its latest download, its snapshot range through the
quarter's end.

-v additionally counts 'other'-bucketed rows per source (asset_
class='other' positions, kind='other' transactions), the
transactions an adapter kinded by the sign of the amount or by the
source's coarser type instead (raw value in payload.source_kind),
and positions with no 2-D vehicle pair yet, so taxonomy drift in
the adapters is visible. It also reports each enrichment family's
backlog and the transactions its reports cannot see because their
kind is 'other', the two row-level ways the cash flow statement can
be quietly wrong, and — for a source holding more than one account
kind — how fresh each kind's data is, so a card population that
stops updating behind a current deposit population is visible.

--marks answers how far a monthly return can be trusted. One row per
month end in gold's history, whatever FROM and TO keep, the whole
history by default:

  marked_pct    the share of value marked within five days, by USD value
                in every currency, so the figure has one definition
  value_marked  that value, in the output currency
  value_total   the value of the positions on the daily history that day
  quality       no_positions: a month end with no line, so no marked_pct;
                unpriced_lines=N: N lines have no rate and leave the sums

A line's age is the month end less the day its mark is from (valued_at,
else its snapshot). Liabilities, cash and lines of no value are out. -x
and -p reach the two money columns; marked_pct does not move.`)
	}
	if err := parseFlags(fs, subargs, "status"); err != nil {
		return err
	}
	if *marks {
		return runStatusMarks(ctx, g, fs, rf, *verbose, stdout, stderr)
	}
	if isSet(fs, "f", "format", "C", "columns", "F", "filter", "x", "currency", "p", "privacy") {
		return errs.Newf(errs.ExitUsage, "status: -f, -C, -F, -x and -p belong to --marks")
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "status: at most one silver_source_id allowed")
	}

	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if fs.NArg() == 1 {
		return runStatusDetailed(ctx, db, cfg, g.ConfigPath, fs.Arg(0), *verbose, stdout)
	}
	runStatusOverview(ctx, db, cfg, g.ConfigPath, *verbose, stdout)
	return nil
}

// runStatusMarks is `status --marks`. Its positionals are a window, not a
// source. Its two money columns are in the output currency, so it says
// on stderr what does not convert to it, as a report does (runReport).
func runStatusMarks(ctx context.Context, g globalFlags, fs *flag.FlagSet, rf *reportFlags, verbose bool, stdout, stderr io.Writer) error {
	if verbose {
		return errs.Newf(errs.ExitUsage, "status: --marks takes no -v")
	}
	fromEpoch, toEpoch, err := parseReturnsWindow(fs.Args(), g.clock())
	if err != nil {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "status: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "status")
	if err != nil {
		return err
	}
	req := request{view: "marks", currency: outCcy, from: fromEpoch, to: toEpoch}
	open := func() (*sql.DB, error) { return openGoldForReport(ctx, g, cfg, outCcy, stderr) }
	return writeReport(ctx, marksReport(req), rf.output(fmtChoice, marksFamily, req, cfg, g.clock()), open, stdout)
}

// marksReport is `status --marks`, the runner the CLI and the MCP server
// share.
func marksReport(req request) *report {
	registry := buildMarkColumnRegistry(req.currency)
	return newReport(registry, columnNames(registry), func(ctx context.Context, db *sql.DB) ([]gold.MarkRow, error) {
		return gold.MarkAges(ctx, db, req.from, req.to, req.currency)
	})
}

func buildMarkColumnRegistry(outCcy string) []columnSpec[gold.MarkRow] {
	return []columnSpec[gold.MarkRow]{
		asDate(strCol("month_end", func(r gold.MarkRow) string { return formatDate(r.MonthEnd) })),
		percentCol("marked_pct", "", func(r gold.MarkRow) string { return formatPercent(r.MarkedPct) }),
		outCcyCol("value_marked", outCcy, func(r gold.MarkRow) *string { return r.ValueMarked }),
		outCcyCol("value_total", outCcy, func(r gold.MarkRow) *string { return r.ValueTotal }),
		qualityCol(func(r gold.MarkRow) string { return r.Quality }),
	}
}

// runStatusOverview prints a line per configured source, then a line
// for the currencies gold holds amounts in that do not convert to the
// config's currency (warnUnconvertible). The commands it names read the
// config at configPath (collectFor, setupCommand).
func runStatusOverview(ctx context.Context, db *sql.DB, cfg *config.Config, configPath string, verbose bool, stdout io.Writer) {
	if len(cfg.SilverSources) == 0 {
		fmt.Fprintln(stdout, "status: no silver sources configured")
		return
	}
	results := gold.StatusForSources(ctx, db, sourceIDs(cfg), verbose)
	for i := range cfg.SilverSources {
		printOneLineStatus(ctx, results[i], &cfg.SilverSources[i], configPath, verbose, stdout)
	}
	warnUnconvertible(ctx, db, configPath, cfg.DefaultCurrency, "status", stdout)
}

// sourceIDs are the configured sources' ids, in config order.
func sourceIDs(cfg *config.Config) []string {
	ids := make([]string, len(cfg.SilverSources))
	for i, s := range cfg.SilverSources {
		ids[i] = s.ID
	}
	return ids
}

// runStatusDetailed prints one source's status. The load it names where
// gold is behind, and the collect command of a source with no data yet,
// read the config at configPath (commandFor, collectFor).
func runStatusDetailed(ctx context.Context, db *sql.DB, cfg *config.Config, configPath, id string, verbose bool, stdout io.Writer) error {
	src, ok := cfg.Lookup(id)
	if !ok {
		return fmt.Errorf("status: silver source %q not found in config", id)
	}

	st, err := gold.StatusForSource(ctx, db, id, verbose)
	if err != nil {
		return err
	}
	if st == nil {
		if line, ok := noDataYet(configPath, src); ok {
			fmt.Fprintf(stdout, "%s (%s)\n  %s\n", id, src.Kind, line)
			return nil
		}
		fmt.Fprintf(stdout, "%s (%s, %s)\n  not registered in gold; run `%s` first\n",
			id, src.Kind, src.Path, commandFor(configPath, "load "+id))
		return nil
	}

	silverStatus, silverErr := probeSilverStatus(ctx, src)
	advancedHint := ""
	if silverErr == nil && silverAhead(st, silverStatus) {
		advancedHint = "  (silver has new data — run `" + commandFor(configPath, "load "+id) + "`)"
	}

	fmt.Fprintf(stdout, "%s [%s] %s\n", id, st.Kind, st.Path)
	if missing := missingFeeds(src); len(missing) > 0 {
		fmt.Fprintf(stdout, "  ERROR: %s\n", missingLine(missing))
	}
	fmt.Fprintf(stdout, "  high_watermark:  %s%s\n", formatWatermark(st.HighWatermark), advancedHint)
	fmt.Fprintf(stdout, "  first_loaded:    %s\n", formatDateTime(st.FirstLoadedAt))
	fmt.Fprintf(stdout, "  last_loaded:     %s\n", formatDateTime(st.LastLoadedAt))
	fmt.Fprintf(stdout, "  observed:        holdings %s, ledger %s\n",
		formatOptionalDate(st.ObservedSnapshotsThrough), formatOptionalDate(st.ObservedTransactionsThrough))
	fmt.Fprintln(stdout, "  gold-side counts:")
	fmt.Fprintf(stdout, "    positions:     %d\n", st.PositionsCount)
	fmt.Fprintf(stdout, "    cash_balances: %d\n", st.CashBalancesCount)
	fmt.Fprintf(stdout, "    fx_rates:      %d\n", st.FxRatesCount)
	fmt.Fprintf(stdout, "    transactions:  %d\n", st.TransactionsCount)
	fmt.Fprintf(stdout, "  snapshot range:  %s\n", formatRange(st.OldestSnapshotAt, st.LatestSnapshotAt))
	fmt.Fprintf(stdout, "  tx range:        %s\n", formatRange(st.OldestTransactionAt, st.LatestTransactionAt))

	if verbose {
		printStatusVerbose(stdout, st)
	}

	if silverErr != nil {
		fmt.Fprintf(stdout, "  silver-side:     unreachable (%s)\n", silverErr.Error())
	} else {
		fmt.Fprintln(stdout, "  silver-side:")
		fmt.Fprintf(stdout, "    LatestChangeNumber: %d\n", silverStatus.LatestChangeNumber)
		fmt.Fprintf(stdout, "    snapshot range:     %s\n", formatRange(silverStatus.OldestSnapshotAt, silverStatus.LatestSnapshotAt))
		fmt.Fprintf(stdout, "    tx range:           %s\n", formatRange(silverStatus.OldestTransactionAt, silverStatus.LatestTransactionAt))
		fmt.Fprintf(stdout, "    observed:           holdings %s, ledger %s\n",
			formatOptionalDate(silverStatus.ObservedSnapshotsThrough), formatOptionalDate(silverStatus.ObservedTransactionsThrough))
	}

	// Most-recent load audit.
	audit, err := gold.RecentLoadAudit(ctx, db, id, 5)
	if err != nil {
		return err
	}
	if len(audit) > 0 {
		fmt.Fprintln(stdout, "  recent loads (newest first):")
		for _, r := range audit {
			before := "(first load)"
			if r.ChangeNumberBefore.Valid {
				before = fmt.Sprintf("%d", r.ChangeNumberBefore.Int64)
			}
			fmt.Fprintf(stdout, "    %s  %s → %d  window=[%s]  +%d snap +%d tx\n",
				formatDateTime(r.LoadedAt/1_000_000_000), before, r.ChangeNumberAfter,
				formatWindow(r.WindowStart, r.WindowEnd),
				r.SnapshotsLoaded, r.TransactionsLoaded)
		}
	}
	return nil
}

// printOneLineStatus emits a single-line summary for the overview
// view. Tolerates a per-source error: the line says ERROR and the
// message, and the next source still prints.
func printOneLineStatus(ctx context.Context, r gold.SourceStatusResult, src *config.SilverSource, configPath string, verbose bool, stdout io.Writer) {
	st, err := r.Status, r.Err
	if err != nil {
		fmt.Fprintf(stdout, "%-20s [%s] ERROR: %s\n", src.ID, src.Kind, err.Error())
		return
	}
	if st == nil {
		if line, ok := noDataYet(configPath, src); ok {
			fmt.Fprintf(stdout, "%-20s [%s] %s\n", src.ID, src.Kind, line)
			return
		}
		fmt.Fprintf(stdout, "%-20s [%s] not loaded yet\n", src.ID, src.Kind)
		return
	}

	if missing := missingFeeds(src); len(missing) > 0 {
		fmt.Fprintf(stdout, "%-20s [%s] ERROR: %s\n", src.ID, src.Kind, missingLine(missing))
		return
	}

	advanced := ""
	silverStatus, silverErr := probeSilverStatus(ctx, src)
	if silverErr == nil && silverAhead(st, silverStatus) {
		advanced = " *"
	}

	driftHint := ""
	if verbose && (st.OtherAssetClassCount > 0 || st.OtherTxKindCount > 0 ||
		st.GuessedTxKindCount > 0) {
		driftHint = fmt.Sprintf("  drift: %d pos/'other'+%d tx/'other'+%d tx/guessed",
			st.OtherAssetClassCount, st.OtherTxKindCount, st.GuessedTxKindCount)
	}
	if verbose && st.MissingVehicleCount > 0 {
		driftHint += fmt.Sprintf("  %d pos/no-vehicle", st.MissingVehicleCount)
	}
	if verbose && (st.UncategorizedSpendCount > 0 || st.ExcludedUnmappedCount > 0) {
		driftHint += fmt.Sprintf("  spend: %d uncategorised, %d excluded_unmapped",
			st.UncategorizedSpendCount, st.ExcludedUnmappedCount)
	}
	if verbose && st.UncategorizedIncomeCount > 0 {
		driftHint += fmt.Sprintf("  income: %d uncategorised", st.UncategorizedIncomeCount)
	}
	// The cash flow statement has no backlog to hint at — it reads the
	// two families' verdicts — so the hint carries the two row-level
	// ways it can be quietly wrong. The third, a pooled account with no
	// wrapper, is about accounts rather than rows and stays in the
	// per-source block with the rest.
	if verbose && (st.CashflowExcludedByKindCount > 0 || st.CashflowNoFarAccountCount > 0) {
		driftHint += fmt.Sprintf("  cashflow: %d excluded_by_kind, %d no_far_account",
			st.CashflowExcludedByKindCount, st.CashflowNoFarAccountCount)
	}

	fmt.Fprintf(stdout, "%-20s [%s] %d pos, %d tx, observed=%s, watermark=%s%s%s\n",
		src.ID, st.Kind, st.PositionsCount, st.TransactionsCount, formatOptionalDate(st.ObservedSnapshotsThrough),
		formatWatermark(st.HighWatermark), advanced, driftHint)
}

// noDataYet is the status of a source gold has not loaded, none of whose
// silver files exists yet, naming the command that collects it for the
// config at configPath.
func noDataYet(configPath string, src *config.SilverSource) (string, bool) {
	if len(missingFeeds(src)) < len(src.Feeds()) {
		return "", false
	}
	return noDataLine(configPath, src.Feeds()), true
}

// silverAhead says the silver holds data a load would bring: its latest
// change is past the watermark gold loaded through.
func silverAhead(st *gold.SourceStatus, silver canonical.Status) bool {
	return silver.LatestChangeNumber > st.HighWatermark
}

// probeSilverStatus opens the silver via the adapter and calls
// Status(). Used both by overview and detailed views to flag
// "silver has advanced past the stored watermark" without
// requiring a load. A silver whose collector writes it right now is not
// read (lockSilverForRead, with no wait): the callers show no flag for
// a probe that fails.
func probeSilverStatus(ctx context.Context, src *config.SilverSource) (canonical.Status, error) {
	adapter, err := silver.Get(src.Kind)
	if err != nil {
		return canonical.Status{}, err
	}
	var paths []string
	for _, f := range src.Feeds() {
		paths = append(paths, f.Path)
	}
	unlock, err := lockSilverForRead(ctx, paths, 0)
	if err != nil {
		return canonical.Status{}, err
	}
	defer unlock()
	conn, err := adapter.Open(ctx, src.ToSilverOpenSpec())
	if err != nil {
		return canonical.Status{}, err
	}
	defer conn.Close()
	return conn.Status(ctx)
}

// formatRange renders an [oldest, latest] timestamp pair as
// "YYYY-MM-DD..YYYY-MM-DD" or "(none)" when both are sentinels.
func formatRange(oldest, latest int64) string {
	if oldest < 0 || latest < 0 {
		return "(none)"
	}
	return formatDate(oldest) + ".." + formatDate(latest)
}

// formatOptionalDate renders a single timestamp that carries the -1
// "no rows of this grain" sentinel.
func formatOptionalDate(epoch int64) string {
	if epoch < 0 {
		return "(none)"
	}
	return formatDate(epoch)
}

// printStatusVerbose is `status -v`'s extra block: the taxonomy drift
// counters and one section per enrichment family.
//
// Its own function so the SEQUENCE is testable: both families'
// backlogs are reported here, and a test pins that each section
// prints.
func printStatusVerbose(stdout io.Writer, st *gold.SourceStatus) {
	fmt.Fprintln(stdout, "  taxonomy drift (what no adapter could map):")
	fmt.Fprintf(stdout, "    asset_class='other':     %d positions\n", st.OtherAssetClassCount)
	fmt.Fprintf(stdout, "    kind='other':            %d transactions\n", st.OtherTxKindCount)
	fmt.Fprintf(stdout, "    kind guessed:            %d transactions (raw value in payload.source_kind)\n",
		st.GuessedTxKindCount)
	fmt.Fprintf(stdout, "    vehicle missing (NULL):  %d positions\n", st.MissingVehicleCount)
	fmt.Fprintln(stdout, "  spending:")
	fmt.Fprintf(stdout, "    uncategorised:           %d spending lines\n", st.UncategorizedSpendCount)
	// The catch-all-kind counter is NOT duplicated per family, and not
	// because the two count the same rows: it joins spend_scoped_accounts(),
	// and where the two scopes agree — the default — one number answers
	// for both.
	fmt.Fprintf(stdout, "    excluded_unmapped:       %d transactions (catch-all kind on in-scope accounts)\n",
		st.ExcludedUnmappedCount)
	fmt.Fprintln(stdout, "  income:")
	fmt.Fprintf(stdout, "    uncategorised:           %d income lines\n", st.UncategorizedIncomeCount)
	// The cash flow statement has no backlog of its own — it reads the
	// two families' verdicts — so its block counts the three ways the
	// statement can be quietly wrong instead.
	fmt.Fprintln(stdout, "  cashflow:")
	fmt.Fprintf(stdout, "    excluded by kind:        %d transactions on pooled accounts\n",
		st.CashflowExcludedByKindCount)
	fmt.Fprintf(stdout, "    no tax wrapper:          %d pooled accounts (each reads as the household's)\n",
		st.CashflowPooledNoWrapperCount)
	fmt.Fprintf(stdout, "    no far account:          %d own-account moves (in 'Unpaired transfers')\n",
		st.CashflowNoFarAccountCount)
	if len(st.PerKindActivity) > 0 {
		fmt.Fprintln(stdout, "  per account kind (latest snapshot / latest transaction):")
		for _, a := range st.PerKindActivity {
			fmt.Fprintf(stdout, "    %-12s %3d acct  snap=%s  tx=%s\n",
				a.AccountKind, a.Accounts,
				formatOptionalDate(a.LatestSnapshotAt), formatOptionalDate(a.LatestTransactionAt))
		}
	}
}

// formatWatermark renders a stored change-number watermark as
// "<epoch> (<UTC datetime>)". Change numbers are Unix seconds by
// contract (canonical.Status — every adapter reports its newest
// dump/snapshot time), so the datetime form is always meaningful;
// non-positive values (sentinel / never loaded) stay bare.
func formatWatermark(w int64) string {
	if w <= 0 {
		return fmt.Sprintf("%d", w)
	}
	return fmt.Sprintf("%d (%s)", w, formatDateTime(w))
}
