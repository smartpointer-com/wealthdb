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
	register("load", cmdLoad)
}

func cmdLoad(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "load all configured silver sources")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb load <silver_source_id> | -a

Merge new silver snapshots into gold for one silver source, or
for all configured sources (-a). See docs/DESIGN.md §8 for the
load semantics.`)
	}
	if err := fs.Parse(subargs); err != nil {
		return errs.Newf(2, "load: bad flags")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Resolve which sources to load.
	var specs []loader.SourceSpec
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "load: '-a' and a positional id are mutually exclusive")
	case *all:
		for _, s := range cfg.SilverSources {
			specs = append(specs, loader.SourceSpec{ID: s.ID, Kind: s.Kind, Path: s.Path})
		}
		if len(specs) == 0 {
			return fmt.Errorf("load: -a passed but no silver sources are configured")
		}
	case fs.NArg() == 1:
		id := fs.Arg(0)
		s, ok := cfg.Lookup(id)
		if !ok {
			return fmt.Errorf("load: silver source %q not found in config", id)
		}
		specs = []loader.SourceSpec{{ID: s.ID, Kind: s.Kind, Path: s.Path}}
	default:
		fs.Usage()
		return errs.Newf(2, "load: expected one silver_source_id or -a")
	}

	// Mode detection. load is RW.
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
			"'load' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cfg.GoldDB, dec.Reason)
	}

	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	ld := loader.New(db)
	var firstErr error
	for _, spec := range specs {
		res, err := ld.Load(ctx, spec)
		if err != nil {
			if errors.Is(err, loader.ErrSilverWentBackwards) {
				fmt.Fprintf(stderr, "load: %s: %s\n", spec.ID, err.Error())
			} else {
				fmt.Fprintf(stderr, "load: %s: %s\n", spec.ID, err.Error())
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		printLoadResult(stdout, res)
	}
	return firstErr
}

func printLoadResult(w io.Writer, r *loader.LoadResult) {
	switch {
	case r.AlreadyUpToDate:
		fmt.Fprintf(w, "load: %s: up-to-date (watermark %d)\n", r.SourceID, r.ChangeNumberAfter)
	default:
		fmt.Fprintf(w, "load: %s: %d snapshot row(s) + %d transaction(s) (watermark %d → %d)\n",
			r.SourceID, r.SnapshotsLoaded, r.TransactionsLoaded,
			r.ChangeNumberBefore, r.ChangeNumberAfter)
	}
}
