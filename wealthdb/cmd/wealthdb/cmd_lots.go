package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// `wealthdb lots rebuild` runs the lot engine's pass by hand
// (docs/LOTS.md). `load` and `reload` run the same pass after the
// enrichment pass, and it replays whenever the gold rows, the config or
// the engine changed; this verb applies such a change without a load.
func init() {
	register("lots", cmdLots)
}

const lotsUsage = `usage: wealthdb lots rebuild [--force]

Replay the trades of every source whose lot policy is not off into
lots, and write the cost basis no source states: the open lots, the
cost basis of each position, and a realized lot for each sale
(docs/LOTS.md). Stated figures are never replaced. The method comes
from the "lots" block of wealthdb.cfg, else the source kind's own.

The pass replays nothing when no input changed since the last one;
--force replays anyway.`

func cmdLots(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(subargs) == 0 || subargs[0] != "rebuild" {
		fmt.Fprintln(stderr, lotsUsage)
		if len(subargs) > 0 && (subargs[0] == "-h" || subargs[0] == "--help" || subargs[0] == "help") {
			return nil
		}
		return errs.Newf(2, "lots: the verb is 'rebuild'")
	}
	fs := flag.NewFlagSet("wealthdb lots rebuild", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "replay even when no input changed")
	fs.Usage = func() { fmt.Fprintln(stderr, lotsUsage) }
	if err := fs.Parse(subargs[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "lots: bad flags")
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return errs.Newf(2, "lots: unexpected argument %q", fs.Arg(0))
	}
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	db, lock, err := openGoldForWrite(g, cfg, "lots",
		"gold database %q does not exist. Run 'wealthdb init' first (requires write access).")
	if err != nil {
		return err
	}
	defer lock.unlock()
	defer db.Close()
	return runLotPass(ctx, db, cfg, *force, stdout)
}

// runLotPass runs the lot engine's pass and reports it. load and
// reload run it after the enrichment pass. Like that pass, a failure is
// an error: the positions a load rewrote would otherwise carry no
// rebuilt basis without saying why.
func runLotPass(ctx context.Context, db *sql.DB, cfg *config.Config, force bool, stdout io.Writer) error {
	lc, err := cfg.LotsEngine()
	if err != nil {
		return err
	}
	sum, err := gold.RebuildLots(ctx, db, gold.LotsOptions{Config: lc, Force: force})
	if err != nil {
		return fmt.Errorf("lots: %w", err)
	}
	if sum.Unchanged {
		fmt.Fprintln(stdout, "lots: no input changed since the last pass")
		return nil
	}
	for _, s := range sum.Sources {
		if s.Events == 0 {
			// A source of a replayed kind that holds no securities.
			continue
		}
		fmt.Fprintf(stdout, "lots: %s (%s, %s): %d lots, %d realized, %d positions filled, %d seeds, %d implied, %d blips, %d anchors\n",
			s.SourceID, s.Mode, s.Methods, s.Lots, s.RealizedLots, s.PositionsFilled, s.Seeds, s.ImpliedDisposals, s.Blips, s.Anchors)
	}
	fmt.Fprintf(stdout, "lots: build %d in %s\n", sum.BuildID, sum.Took.Round(1e6))
	return nil
}
