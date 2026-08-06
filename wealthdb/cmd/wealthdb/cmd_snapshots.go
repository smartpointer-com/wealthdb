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
	latest := fs.Bool("latest", false, "print only the newest snapshot per source")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb snapshots <silver_source_id> | -a  [--latest]

List every distinct snapshot_at that gold has data for under the
given silver source (or under every configured silver with -a).
Oldest first, formatted YYYY-MM-DD. With --latest, only the newest
snapshot line per source is printed.

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
		printSnapshotDates(stdout, idWidth, id, times, *latest)
	}
	return nil
}

// printSnapshotDates emits the per-day snapshot lines for one
// source. times is oldest-first (gold.ListSnapshotTimes order) and
// grouped by calendar date — multiple intra-day dumps (common when
// reloading bronze) compress to a single line with a (×N) suffix.
// For sub-day forensics, use `wealthdb status <id>` instead. With
// latestOnly, only the newest day's line is printed.
func printSnapshotDates(stdout io.Writer, idWidth int, id string, times []int64, latestOnly bool) {
	if len(times) == 0 {
		fmt.Fprintf(stdout, "%-*s  (no snapshots)\n", idWidth, id)
		return
	}
	type day struct {
		date  string
		count int
	}
	var days []day
	for _, t := range times {
		d := formatDate(t)
		if n := len(days); n > 0 && days[n-1].date == d {
			days[n-1].count++
			continue
		}
		days = append(days, day{date: d, count: 1})
	}
	if latestOnly {
		days = days[len(days)-1:]
	}
	for _, d := range days {
		if d.count > 1 {
			fmt.Fprintf(stdout, "%-*s  %s (×%d)\n", idWidth, id, d.date, d.count)
		} else {
			fmt.Fprintf(stdout, "%-*s  %s\n", idWidth, id, d.date)
		}
	}
}
