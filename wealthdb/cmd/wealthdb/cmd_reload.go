package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/loader"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
)

func init() {
	register("reload", cmdReload)
}

// cmdReload is the reset-then-load shortcut. Useful after upgrading
// wealthdb when adapter logic changes affect already-loaded rows —
// the per-column upsert guard makes incremental load skip
// re-touching snapshots at-or-before the watermark, so values that
// the new adapter would now emit don't backfill into gold.
//
// 'reload -a' rebuilds every source into a FRESH gold file and
// atomically swaps it over the live path, which also compacts the
// file (a fresh build carries none of the dead row-group versions an
// in-place reset+load leaves behind). --in-place keeps the older
// reset-then-load-on-the-live-DB behaviour. Single-source
// 'reload <id>' always stays in-place.
func cmdReload(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "reload every registered silver source")
	inPlace := fs.Bool("in-place", false, "with -a: reset+load on the live DB instead of building a fresh file")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb reload <silver_source_id> | -a [--in-place]

Reset then re-load one silver source (or every registered silver
source with -a). Equivalent to running 'wealthdb reset <id>'
followed by 'wealthdb load <id>'.

With -a, the default builds a fresh gold file from every source and
atomically swaps it over the live path — concurrent readers keep the
old file until they close, and the rebuilt file is compact. Pass
--in-place to reset+load on the live DB instead.

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
	if *inPlace && !*all {
		fs.Usage()
		return errs.Newf(2, "reload: --in-place only applies to '-a'")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	ledger, err := loader.ParseTransferLedger(cfg.EquityTransfers)
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

	// Default '-a': build a fresh file from scratch and swap it in.
	if *all && !*inPlace {
		return reloadFreshAndSwap(ctx, cfg, targets, ledger, stdout, stderr)
	}

	return reloadInPlace(ctx, cfg, targets, ledger, stdout, stderr)
}

// reloadFreshAndSwap builds a complete gold DB from every target
// source into a fresh temp file and atomically swaps it over the live
// path. The temp starts empty, so no per-source reset is needed; the
// load repopulates load_audit / silver_sources exactly as a live
// reset+load would. If any source fails to build, the error is
// surfaced and the live DB is left untouched (no swap).
func reloadFreshAndSwap(
	ctx context.Context,
	cfg *config.Config,
	targets []config.SilverSource,
	ledger map[string][]loader.TransferEntry,
	stdout, stderr io.Writer,
) error {
	var buildErr error
	build := func(tmp string) error {
		db, err := gold.Open(tmp, gold.ModeReadWrite)
		if err != nil {
			return err
		}
		// The temp DB is empty, so no per-source reset is needed —
		// each Load registers the source and populates its rows fresh.
		ld := loader.New(db)
		var firstErr error
		for _, s := range targets {
			spec, err := buildSourceSpec(s, cfg.AccountOverrides, cfg.PortfolioOverrides, cfg.InstrumentOverrides, ledger)
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
			fmt.Fprintf(stdout, "reload: %s: %d snapshot row(s) + %d transaction(s) (watermark → %d)\n",
				s.ID, res.SnapshotsLoaded, res.TransactionsLoaded, res.ChangeNumberAfter)
		}
		if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
			fmt.Fprintf(stderr, "reload: warning: could not stamp FX priorities: %s\n", err.Error())
		}
		// Checkpoint then close so the temp file is complete and clean
		// (no leftover WAL) before it is verified and swapped.
		if _, err := db.ExecContext(ctx, "CHECKPOINT"); err != nil {
			_ = db.Close()
			return fmt.Errorf("checkpoint rebuilt gold: %w", err)
		}
		if err := db.Close(); err != nil {
			return fmt.Errorf("close rebuilt gold: %w", err)
		}
		// Surface a per-source failure so BuildFreshAndSwap does NOT
		// swap a partial rebuild over the live DB.
		buildErr = firstErr
		return firstErr
	}

	reclaimed, err := gold.BuildFreshAndSwap(ctx, cfg.GoldDB, build)
	if err != nil {
		if buildErr != nil {
			// A source failed to build; the live DB is untouched.
			return buildErr
		}
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("reload -a: %w", err))
	}
	fmt.Fprintf(stdout, "reload: rebuilt gold from %d source(s), reclaimed %s\n",
		len(targets), formatBytes(reclaimed))
	return nil
}

// reloadInPlace is the original reset-then-load-on-the-live-DB path,
// used for single-source reloads and for 'reload -a --in-place'.
func reloadInPlace(
	ctx context.Context,
	cfg *config.Config,
	targets []config.SilverSource,
	ledger map[string][]loader.TransferEntry,
	stdout, stderr io.Writer,
) error {
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
		spec, err := buildSourceSpec(s, cfg.AccountOverrides, cfg.PortfolioOverrides, cfg.InstrumentOverrides, ledger)
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
	if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
		fmt.Fprintf(stderr, "reload: warning: could not stamp FX priorities: %s\n", err.Error())
	}
	return firstErr
}
