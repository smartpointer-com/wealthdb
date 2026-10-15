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

// cmdWebMaterialize rewrites the three derived tables in the live gold DB.
// report_returns holds the full RunReturns matrix (every grain × period ×
// reporting currency, plus the per-year windowed since-<year> summaries)
// with the CLI-default knobs and the same wealthdb.cfg returns settings a
// CLI run applies (returnsCfgSettings) — each base partition is the
// verbatim output of a bare
// `wealthdb returns <grain> --period <granularity> --method both -x <CCY>`.
// report_gains holds the monthly gains rows per reporting currency, the
// rows `wealthdb gains <grain> --period monthly -x <CCY>` sums.
// report_exposure holds the look-through at every month end and today
// per reporting currency; an instant's class totals are those of
// `wealthdb exposure classes -d <day> -x <CCY>`. The host-side
// `wealthdb web` wrapper runs it right before snapshotting so the
// Metabase Returns, Gains and Exposure dashboards are as fresh as the
// holdings. Hidden from `wealthdb help`; it's plumbing
// for `wealthdb web`, not a user-facing command (diagnostic knobs stay
// on `wealthdb returns` and `wealthdb gains`). A concurrent write command
// holds the gold write mutex, so the gate fails cleanly here and the
// wrapper aborts before the snapshot is touched.
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

	inceptionOv, exclude, hide, policyOv, matching := returnsCfgSettings(cfg)
	now := time.Now()
	// The same end-of-today anchor a bare CLI run gets from its default
	// window.
	toEpoch := anchorToDay(now.UTC(), true).Unix()
	n, err := gold.MaterializeReturns(ctx, db, gold.MaterializeParams{
		ToEpoch:            toEpoch,
		ComputedAt:         now.Unix(),
		InceptionOverrides: inceptionOv,
		ReturnsExclude:     exclude,
		ReturnsHide:        hide,
		PolicyOverrides:    policyOv,
		TransferMatching:   matching,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "returns: materialized %d rows (4 grains x 4 periods x %d currencies, plus per-year windows)\n", n, len(gold.MaterializedCurrencies()))
	n, err = gold.MaterializeGains(ctx, db, toEpoch, now.Unix())
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "gains: materialized %d rows (monthly x %d currencies x %d readings of a missing cost basis)\n",
		n, len(gold.MaterializedCurrencies()), len(lots.MissingBasisReadings))
	n, err = gold.MaterializeExposure(ctx, db, toEpoch, now.Unix())
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "exposure: materialized %d rows (month ends x %d currencies)\n", n, len(gold.MaterializedCurrencies()))
	return nil
}
