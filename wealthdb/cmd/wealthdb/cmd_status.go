package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

func init() {
	register("status", cmdStatus)
}

func cmdStatus(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	verbose := fs.Bool("v", false, "verbose: include taxonomy-drift counts (asset_class='other' and kind='other')")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb status [<silver_source_id>] [-v]

Without <id>: one line per silver source in the config — kind,
path, gold-side counts, watermark, silver-side change number, and
a * if a `+"`wealthdb load`"+` would bring in new data.

With <id>: a detailed per-source block — counts per gold table,
silver Status() extrema, the stored watermark, and the most
recent load_audit rows.

-v additionally counts 'other'-bucketed rows per source (asset_
class='other' positions, kind='other' transactions) so taxonomy
drift in the adapters is visible.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "status: bad flags")
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return errs.Newf(2, "status: at most one silver_source_id allowed")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}

	openMode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		openMode = gold.ModeReadOnly
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	if fs.NArg() == 1 {
		return runStatusDetailed(ctx, db, cfg, fs.Arg(0), *verbose, stdout)
	}
	return runStatusOverview(ctx, db, cfg, *verbose, stdout)
}

func runStatusOverview(ctx context.Context, db *sql.DB, cfg *config.Config, verbose bool, stdout io.Writer) error {
	if len(cfg.SilverSources) == 0 {
		fmt.Fprintln(stdout, "status: no silver sources configured")
		return nil
	}
	for i := range cfg.SilverSources {
		printOneLineStatus(ctx, db, &cfg.SilverSources[i], verbose, stdout)
	}
	return nil
}

func runStatusDetailed(ctx context.Context, db *sql.DB, cfg *config.Config, id string, verbose bool, stdout io.Writer) error {
	src, ok := cfg.Lookup(id)
	if !ok {
		return fmt.Errorf("status: silver source %q not found in config", id)
	}

	st, err := gold.StatusForSource(ctx, db, id, verbose)
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Fprintf(stdout, "%s (%s, %s)\n  not registered in gold; run `wealthdb load %s` first\n",
			id, src.Kind, src.Path, id)
		return nil
	}

	silverStatus, silverErr := probeSilverStatus(ctx, src)
	advancedHint := ""
	if silverErr == nil && silverStatus.LatestChangeNumber > st.HighWatermark {
		advancedHint = "  (silver has new data — run `wealthdb load " + id + "`)"
	}

	fmt.Fprintf(stdout, "%s [%s] %s\n", id, st.Kind, st.Path)
	fmt.Fprintf(stdout, "  high_watermark:  %d%s\n", st.HighWatermark, advancedHint)
	fmt.Fprintf(stdout, "  first_loaded:    %s\n", formatDateTime(st.FirstLoadedAt))
	fmt.Fprintf(stdout, "  last_loaded:     %s\n", formatDateTime(st.LastLoadedAt))
	fmt.Fprintln(stdout, "  gold-side counts:")
	fmt.Fprintf(stdout, "    positions:     %d\n", st.PositionsCount)
	fmt.Fprintf(stdout, "    cash_balances: %d\n", st.CashBalancesCount)
	fmt.Fprintf(stdout, "    fx_rates:      %d\n", st.FxRatesCount)
	fmt.Fprintf(stdout, "    transactions:  %d\n", st.TransactionsCount)
	fmt.Fprintf(stdout, "  snapshot range:  %s\n", formatRange(st.OldestSnapshotAt, st.LatestSnapshotAt))
	fmt.Fprintf(stdout, "  tx range:        %s\n", formatRange(st.OldestTransactionAt, st.LatestTransactionAt))

	if verbose {
		fmt.Fprintln(stdout, "  taxonomy drift ('other' bucket):")
		fmt.Fprintf(stdout, "    asset_class='other': %d positions\n", st.OtherAssetClassCount)
		fmt.Fprintf(stdout, "    kind='other':        %d transactions\n", st.OtherTxKindCount)
	}

	if silverErr != nil {
		fmt.Fprintf(stdout, "  silver-side:     unreachable (%s)\n", silverErr.Error())
	} else {
		fmt.Fprintln(stdout, "  silver-side:")
		fmt.Fprintf(stdout, "    LatestChangeNumber: %d\n", silverStatus.LatestChangeNumber)
		fmt.Fprintf(stdout, "    snapshot range:     %s\n", formatRange(silverStatus.OldestSnapshotAt, silverStatus.LatestSnapshotAt))
		fmt.Fprintf(stdout, "    tx range:           %s\n", formatRange(silverStatus.OldestTransactionAt, silverStatus.LatestTransactionAt))
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
			fmt.Fprintf(stdout, "    %s  %s → %d  window=[%s..%s]  +%d snap +%d tx\n",
				formatDateTime(r.LoadedAt/1_000_000_000), before, r.ChangeNumberAfter,
				formatDate(r.WindowStart), formatDate(r.WindowEnd),
				r.SnapshotsLoaded, r.TransactionsLoaded)
		}
	}
	return nil
}

// printOneLineStatus emits a single-line summary for the overview
// view. Tolerates per-source errors — prints a "?" / message
// rather than failing the whole report.
func printOneLineStatus(ctx context.Context, db *sql.DB, src *config.SilverSource, verbose bool, stdout io.Writer) {
	st, err := gold.StatusForSource(ctx, db, src.ID, verbose)
	if err != nil {
		fmt.Fprintf(stdout, "%-20s [%s] ERROR: %s\n", src.ID, src.Kind, err.Error())
		return
	}
	if st == nil {
		fmt.Fprintf(stdout, "%-20s [%s] not loaded yet\n", src.ID, src.Kind)
		return
	}

	advanced := ""
	silverStatus, silverErr := probeSilverStatus(ctx, src)
	if silverErr == nil && silverStatus.LatestChangeNumber > st.HighWatermark {
		advanced = " *"
	}

	driftHint := ""
	if verbose && (st.OtherAssetClassCount > 0 || st.OtherTxKindCount > 0) {
		driftHint = fmt.Sprintf("  drift: %d pos/'other'+%d tx/'other'",
			st.OtherAssetClassCount, st.OtherTxKindCount)
	}

	fmt.Fprintf(stdout, "%-20s [%s] %d pos, %d tx, watermark=%d%s%s\n",
		src.ID, st.Kind, st.PositionsCount, st.TransactionsCount, st.HighWatermark,
		advanced, driftHint)
}

// probeSilverStatus opens the silver via the adapter and calls
// Status(). Used both by overview and detailed views to flag
// "silver has advanced past the stored watermark" without
// requiring a load.
func probeSilverStatus(ctx context.Context, src *config.SilverSource) (canonical_status, error) {
	adapter, err := silver.Get(src.Kind)
	if err != nil {
		return canonical_status{}, err
	}
	spec, err := src.ToSilverOpenSpec()
	if err != nil {
		return canonical_status{}, err
	}
	conn, err := adapter.Open(ctx, spec)
	if err != nil {
		return canonical_status{}, err
	}
	defer conn.Close()
	s, err := conn.Status(ctx)
	if err != nil {
		return canonical_status{}, err
	}
	return canonical_status{
		OldestSnapshotAt:    s.OldestSnapshotAt,
		LatestSnapshotAt:    s.LatestSnapshotAt,
		OldestTransactionAt: s.OldestTransactionAt,
		LatestTransactionAt: s.LatestTransactionAt,
		LatestChangeNumber:  s.LatestChangeNumber,
	}, nil
}

// canonical_status is a local mirror of canonical.Status so this
// file doesn't have to import canonical just for one type. (The
// silver.Adapter API still depends on canonical, but the cmd
// layer doesn't need to surface canonical types in its own
// helpers.)
type canonical_status struct {
	OldestSnapshotAt    int64
	LatestSnapshotAt    int64
	OldestTransactionAt int64
	LatestTransactionAt int64
	LatestChangeNumber  int64
}

// formatRange renders an [oldest, latest] timestamp pair as
// "YYYY-MM-DD..YYYY-MM-DD" or "(none)" when both are sentinels.
func formatRange(oldest, latest int64) string {
	if oldest < 0 || latest < 0 {
		return "(none)"
	}
	return formatDate(oldest) + ".." + formatDate(latest)
}

// formatDateTime is the human-readable form for second-grain
// audit/registration timestamps.
func formatDateTime(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02 15:04:05Z")
}
