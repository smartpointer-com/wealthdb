package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
)

func init() {
	register("web-materialize", cmdWebMaterialize)
}

// cmdWebMaterialize rewrites the report_returns table in the live gold DB:
// the full RunReturns matrix (4 grains × 4 periods × 3 currencies, plus the
// per-year windowed since-<year> summaries) with the CLI-default knobs and the
// config's inception_overrides / returns_exclude applied — each base partition
// is the verbatim output of a bare
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

	// Mode detection mirrors cmd_load: materialization is a write.
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}
	if dec.Mode != pathmode.ModeReadWrite {
		return errs.Newf(errs.ExitRWNeeded,
			"'web-materialize' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cfg.GoldDB, dec.Reason)
	}

	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	inceptionOv, exclude := returnsCfgSettings(cfg)
	now := time.Now()
	n, err := gold.MaterializeReturns(ctx, db, gold.MaterializeParams{
		// The same end-of-today anchor a bare CLI run gets from
		// parseReturnsWindow's default window.
		ToEpoch:            anchorToDay(now.UTC(), true).Unix(),
		ComputedAt:         now.Unix(),
		InceptionOverrides: inceptionOv,
		ReturnsExclude:     exclude,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "returns: materialized %d rows (4 grains x 4 periods x 3 currencies, plus per-year windows)\n", n)
	return nil
}
