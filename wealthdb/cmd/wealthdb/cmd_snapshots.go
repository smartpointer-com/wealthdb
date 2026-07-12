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
)

func init() {
	register("snapshots", cmdSnapshots)
}

func cmdSnapshots(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb snapshots", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "list snapshots for every registered silver source")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb snapshots <silver_source_id> | -a

List every distinct snapshot_at that gold has data for under the
given silver source (or under every configured silver with -a).
Oldest first, formatted YYYY-MM-DD.

Useful for "I expected to see a snapshot from yesterday and I
don't" debugging.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "snapshots: bad flags")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	var ids []string
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "snapshots: '-a' and a positional id are mutually exclusive")
	case *all:
		ids, err = gold.ListSilverSources(ctx, db)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Fprintln(stdout, "snapshots: no silver sources registered in gold yet")
			return nil
		}
	case fs.NArg() == 1:
		ids = []string{fs.Arg(0)}
	default:
		fs.Usage()
		return errs.Newf(2, "snapshots: expected one silver_source_id or -a")
	}

	// Column width for the source id — pad to the longest id so
	// dates line up visually. Single-id mode ends up with no
	// padding (width == len(only id)).
	idWidth := 0
	for _, id := range ids {
		if len(id) > idWidth {
			idWidth = len(id)
		}
	}

	for _, id := range ids {
		times, err := gold.ListSnapshotTimes(ctx, db, id)
		if err != nil {
			return err
		}
		if len(times) == 0 {
			fmt.Fprintf(stdout, "%-*s  (no snapshots)\n", idWidth, id)
			continue
		}
		// Group by calendar date — multiple intra-day dumps
		// (common when reloading bronze) compress to a single
		// line with a (×N) suffix. For sub-day forensics, use
		// `wealthdb status <id>` instead.
		var prevDate string
		var count int
		emit := func() {
			if prevDate == "" {
				return
			}
			if count > 1 {
				fmt.Fprintf(stdout, "%-*s  %s (×%d)\n", idWidth, id, prevDate, count)
			} else {
				fmt.Fprintf(stdout, "%-*s  %s\n", idWidth, id, prevDate)
			}
		}
		for _, t := range times {
			d := formatDate(t)
			if d == prevDate {
				count++
				continue
			}
			emit()
			prevDate, count = d, 1
		}
		emit()
	}
	return nil
}
