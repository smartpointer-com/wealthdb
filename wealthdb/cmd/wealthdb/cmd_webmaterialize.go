package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("web-materialize", cmdWebMaterialize)
}

// cmdWebMaterialize rewrites the report_returns table in the live gold DB:
// the full RunReturns matrix (4 grains × 4 periods × 3 currencies, plus the
// per-year windowed since-<year> summaries) with the CLI-default knobs and the
// config's inception_overrides / returns_exclude / returns_policy_overrides
// applied — each base partition is the verbatim output of a bare
// `wealthdb returns <grain> --period <granularity> --method both -x <CCY>`.
// The host-side `wealthdb web` wrapper runs it right before snapshotting so
// the Metabase Returns dashboards are as fresh as the holdings. Hidden from
// `wealthdb help`; it's plumbing for `wealthdb web`, not a user-facing
// command (diagnostic knobs stay on `wealthdb returns`). A concurrent
// `wealthdb load` holds DuckDB's single writer lock, so the open fails
// cleanly here and the wrapper aborts before the snapshot is touched.
func cmdWebMaterialize(ctx context.Context, g globalFlags, _ []string, _ io.Reader, _, stderr io.Writer) error {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Materialization mutates the live gold file: gate for write, then
	// open RW (mirrors cmd_load).
	db, err := openGoldForWrite(g, cfg, "web-materialize",
		"gold database %q does not exist. Run 'wealthdb init' first (requires write access).")
	if err != nil {
		return err
	}
	defer db.Close()

	inceptionOv, exclude, hide, policyOv := returnsCfgSettings(cfg)
	now := time.Now()
	n, err := gold.MaterializeReturns(ctx, db, gold.MaterializeParams{
		// The same end-of-today anchor a bare CLI run gets from
		// parseReturnsWindow's default window.
		ToEpoch:            anchorToDay(now.UTC(), true).Unix(),
		ComputedAt:         now.Unix(),
		InceptionOverrides: inceptionOv,
		ReturnsExclude:     exclude,
		ReturnsHide:        hide,
		PolicyOverrides:    policyOv,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "returns: materialized %d rows (4 grains x 4 periods x 3 currencies, plus per-year windows)\n", n)
	return nil
}
