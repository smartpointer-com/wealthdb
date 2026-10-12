package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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
	if err := parseFlags(fs, subargs, "snapshots"); err != nil {
		return err
	}
	id, err := sourceArg(fs, *all, "snapshots")
	if err != nil {
		return err
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

	ids := []string{id}
	if *all {
		if ids, err = gold.ListSilverSources(ctx, db); err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Fprintln(stdout, "snapshots: no silver sources registered in gold yet")
			return nil
		}
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
// source. times is oldest-first (gold.ListSnapshotTimes order);
// multiple intra-day dumps (common when reloading bronze) compress to a
// single line with a (×N) suffix. For sub-day forensics, use `wealthdb
// status <id>` instead. With latestOnly, only the newest day's line is
// printed.
func printSnapshotDates(stdout io.Writer, idWidth int, id string, times []int64, latestOnly bool) {
	days := snapshotDays(id, times, latestOnly)
	if len(days) == 0 {
		fmt.Fprintf(stdout, "%-*s  (no snapshots)\n", idWidth, id)
		return
	}
	for _, d := range days {
		if d.count > 1 {
			fmt.Fprintf(stdout, "%-*s  %s (×%d)\n", idWidth, id, d.date, d.count)
		} else {
			fmt.Fprintf(stdout, "%-*s  %s\n", idWidth, id, d.date)
		}
	}
}

// snapshotRow is one day a source has data for.
type snapshotRow struct {
	source, date string
	count        int // snapshots taken that day
}

// snapshotDays groups a source's oldest-first snapshot times by
// calendar date, one row per day; latestOnly keeps the newest day.
func snapshotDays(id string, times []int64, latestOnly bool) []snapshotRow {
	var days []snapshotRow
	for _, t := range times {
		d := formatDate(t)
		if n := len(days); n > 0 && days[n-1].date == d {
			days[n-1].count++
			continue
		}
		days = append(days, snapshotRow{source: id, date: d, count: 1})
	}
	if latestOnly && len(days) > 0 {
		days = days[len(days)-1:]
	}
	return days
}
