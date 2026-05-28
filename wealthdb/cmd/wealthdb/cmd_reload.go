package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ptu/wealthdb/internal/config"
	"github.com/ptu/wealthdb/internal/errs"
	"github.com/ptu/wealthdb/internal/gold"
	"github.com/ptu/wealthdb/internal/loader"
	"github.com/ptu/wealthdb/internal/pathmode"
)

func init() {
	register("reload", cmdReload)
}

// cmdReload is the reset-then-load shortcut. Useful after upgrading
// wealthdb when adapter logic changes affect already-loaded rows —
// the per-column upsert guard makes incremental load skip
// re-touching snapshots at-or-before the watermark, so values that
// the new adapter would now emit don't backfill into gold.
func cmdReload(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "reload every registered silver source")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb reload <silver_source_id> | -a

Reset then re-load one silver source (or every registered silver
source with -a). Equivalent to running 'wealthdb reset <id>'
followed by 'wealthdb load <id>'.

Use case: after upgrading wealthdb to a binary whose adapter logic
populates new columns or fixes a projection, the upsert guard
prevents the new values from backfilling onto snapshots already at
or below the high watermark. Reload forces a full re-projection.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "reload: bad flags")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Resolve targets to a list of (id, config.SilverSource) pairs so
	// the load step has the kind/path it needs without re-resolving.
	var targets []config.SilverSource
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "reload: '-a' and a positional id are mutually exclusive")
	case *all:
		targets = cfg.SilverSources
		if len(targets) == 0 {
			return fmt.Errorf("reload: -a passed but no silver sources are configured")
		}
	case fs.NArg() == 1:
		s, ok := cfg.Lookup(fs.Arg(0))
		if !ok {
			return fmt.Errorf("reload: silver source %q not found in config", fs.Arg(0))
		}
		targets = []config.SilverSource{*s}
	default:
		fs.Usage()
		return errs.Newf(2, "reload: expected one silver_source_id or -a")
	}

	// Reload is RW (combines reset + load).
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
			"'reload' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cfg.GoldDB, dec.Reason)
	}

	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	ld := loader.New(db)
	var firstErr error
	for _, s := range targets {
		// Reset is best-effort per source — surface failures but
		// keep going so '-a' isn't blocked by a single bad source.
		if err := ld.Reset(ctx, s.ID); err != nil {
			fmt.Fprintf(stderr, "reload: %s: reset: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		spec, err := buildSourceSpec(s, cfg.AccountOverrides)
		if err != nil {
			fmt.Fprintf(stderr, "reload: %s: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res, err := ld.Load(ctx, spec)
		if err != nil {
			fmt.Fprintf(stderr, "reload: %s: load: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch {
		case res.AlreadyUpToDate:
			// Vacuously true after a reset — silver has no
			// changes for an empty watermark, e.g. no dump_runs.
			fmt.Fprintf(stdout, "reload: %s: reset; nothing to load (watermark %d)\n",
				s.ID, res.ChangeNumberAfter)
		default:
			fmt.Fprintf(stdout, "reload: %s: reset + %d snapshot row(s) + %d transaction(s) (watermark → %d)\n",
				s.ID, res.SnapshotsLoaded, res.TransactionsLoaded, res.ChangeNumberAfter)
		}
	}
	return firstErr
}
