package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func init() {
	register("web-materialize", cmdWebMaterialize)
}

// cmdWebMaterialize rewrites the derived tables in the live gold DB.
// The serving tables hold the daily histories, the latest holdings and
// every transaction line the serving views read, each its report
// macro's rows verbatim (gold.MaterializeServingTables). report_returns holds the
// full RunReturns matrix (every grain × period × reporting currency,
// plus the per-year windowed since-<year> summaries) with the
// CLI-default knobs and the same wealthdb.cfg returns settings a CLI
// run applies (returnsCfgSettings) — each base partition is the
// verbatim output of a bare
// `wealthdb returns <grain> --period <granularity> --method both -x <CCY>`.
// report_gains holds the monthly gains rows per reporting currency, the
// rows `wealthdb gains <grain> --period monthly -x <CCY>` sums.
// report_exposure holds the look-through at every month end and today
// per reporting currency; an instant's class totals are those of
// `wealthdb exposure classes -d <day> -x <CCY>`. The yield tables hold
// every holding's yield figures at today and the month ends before, and
// today's income projection, those of `wealthdb yield positions` and
// `wealthdb income`; report_cashflow_plan holds the look-back parts of
// `wealthdb cashflow projection` (both gold.MaterializeYield). The
// host-side `wealthdb web` wrapper runs it right before snapshotting so
// the Metabase dashboards are as fresh as the holdings. Hidden from
// `wealthdb help`; it's plumbing for `wealthdb web`, not a user-facing
// command (diagnostic knobs stay on `wealthdb returns` and `wealthdb
// gains`). A concurrent write command holds the gold write mutex, so
// the gate fails cleanly here and the wrapper aborts before the
// snapshot is touched.
func cmdWebMaterialize(ctx context.Context, g globalFlags, _ []string, _ io.Reader, _, stderr io.Writer) error {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Materialization mutates the live gold file: gate for write, then
	// open RW.
	db, lock, err := openGoldForWrite(g, cfg, "web-materialize", defaultMissingGoldMsg)
	if err != nil {
		return err
	}
	defer lock.unlock()
	defer db.Close()

	if _, err := timedPhase(stderr, "serving", func() (int, error) {
		return gold.MaterializeServingTables(ctx, db)
	}); err != nil {
		return err
	}
	inceptionOv, exclude, hide, policyOv, matching := returnsCfgSettings(cfg)
	now := time.Now()
	// The same end-of-today anchor a bare CLI run gets from its default
	// window.
	toEpoch := anchorToDay(now.UTC(), true).Unix()
	if _, err := timedPhase(stderr, "returns", func() (int, error) {
		return gold.MaterializeReturns(ctx, db, gold.MaterializeParams{
			ToEpoch:            toEpoch,
			ComputedAt:         now.Unix(),
			InceptionOverrides: inceptionOv,
			ReturnsExclude:     exclude,
			ReturnsHide:        hide,
			PolicyOverrides:    policyOv,
			TransferMatching:   matching,
		})
	}); err != nil {
		return err
	}
	if _, err := timedPhase(stderr, "gains", func() (int, error) {
		return gold.MaterializeGains(ctx, db, toEpoch, now.Unix())
	}); err != nil {
		return err
	}
	if _, err := timedPhase(stderr, "exposure", func() (int, error) {
		return gold.MaterializeExposure(ctx, db, toEpoch, now.Unix())
	}); err != nil {
		return err
	}
	if _, err := timedPhase(stderr, "yield", func() (int, error) {
		return gold.MaterializeYield(ctx, db, toEpoch, now.Unix(), cfg.CashflowLookback())
	}); err != nil {
		return err
	}
	return nil
}

// timedPhase runs one materialize phase and prints its wall time on
// stderr. The row-count line stays the summary a reader sees; the
// timing line is what a run compares across builds.
func timedPhase(stderr io.Writer, name string, fn func() (int, error)) (int, error) {
	start := time.Now()
	n, err := fn()
	fmt.Fprintf(stderr, "timing: %s %s\n", name, time.Since(start).Round(time.Millisecond))
	if err != nil {
		return 0, err
	}
	switch name {
	case "serving":
		fmt.Fprintf(stderr, "serving tables: materialized %d rows (histories, latest holdings, transaction lines)\n", n)
	case "returns":
		fmt.Fprintf(stderr, "returns: materialized %d rows (4 grains x 4 periods x %d currencies, plus per-year windows)\n", n, len(gold.MaterializedCurrencies()))
	case "gains":
		fmt.Fprintf(stderr, "gains: materialized %d rows (monthly x %d currencies x %d readings of a missing cost basis)\n",
			n, len(gold.MaterializedCurrencies()), len(lots.MissingBasisReadings))
	case "exposure":
		fmt.Fprintf(stderr, "exposure: materialized %d rows (month ends x %d currencies)\n", n, len(gold.MaterializedCurrencies()))
	case "yield":
		fmt.Fprintf(stderr, "yield: materialized %d rows (holdings at today and %d month ends x %d currencies, today's projection)\n",
			n, gold.YieldHistoryMonths, len(gold.MaterializedCurrencies()))
	}
	return n, nil
}
