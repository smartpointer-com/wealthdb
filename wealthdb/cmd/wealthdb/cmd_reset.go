package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/loader"
)

func init() {
	register("reset", cmdReset)
}

func cmdReset(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "reset every registered silver source")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb reset <silver_source_id> | -a

Purge all gold rows belonging to one silver source (or every
registered silver source with -a). The silver SQLite file itself
is untouched. A follow-up 'wealthdb load <id>' starts from an
empty watermark.

A single-source reset clears only that source's spend enrichment.
The matcher writes its internal_transfer verdict onto BOTH legs of
a cross-source pair, so the surviving leg on another source keeps a
verdict whose partner is gone and stays out of every spending
report until the next load re-asserts the pass.

Use case: a silver was rebuilt from bronze (re-parse, new
migration, data correction) and you want gold to re-sync from
scratch for that source.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "reset: bad flags")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Reset mutates the live gold file: gate for write, then open RW.
	db, lock, err := openGoldForWrite(g, cfg, "reset",
		"gold database %q does not exist. Nothing to reset.")
	if err != nil {
		return err
	}
	defer lock.unlock()
	defer db.Close()

	ld := loader.New(db)

	// Build the target list. Order: explicit id wins; then -a;
	// then complain.
	// registered is what gold knows about; it decides whether the
	// cross-source caveat below applies. Only the single-id path needs
	// it — '-a' resets exactly the registered set and prints no caveat.
	var ids, registered []string
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "reset: '-a' and a positional id are mutually exclusive")
	case *all:
		ids, err = ld.ListSourceIDs(ctx)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Fprintln(stdout, "reset: nothing to do (no silver sources registered in gold)")
			return nil
		}
	case fs.NArg() == 1:
		ids = []string{fs.Arg(0)}
		registered, err = ld.ListSourceIDs(ctx)
		if err != nil {
			return err
		}
	default:
		fs.Usage()
		return errs.Newf(2, "reset: expected one silver_source_id or -a")
	}

	var firstErr error
	for _, id := range ids {
		if err := ld.Reset(ctx, id); err != nil {
			fmt.Fprintf(stderr, "reset: %s: %s\n", id, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Fprintf(stdout, "reset: %s: cleared\n", id)
	}
	// Reset purges the two enrichment overlays for the named source
	// only, but a matcher verdict is written onto both legs of a
	// cross-source pair — and the two legs can be in different
	// families. After a partial reset the surviving leg still reads
	// internal_transfer and stays out of every spending and income
	// report, so the caveat is printed where it can be acted on.
	//
	// Three states make it untrue rather than useful, and each is
	// silent instead: after -a there is no surviving leg; after a
	// failed purge nothing was cleared to go stale; and an id gold
	// never registered had no rows to pair against in the first place.
	if !*all && firstErr == nil && slices.Contains(registered, ids[0]) {
		fmt.Fprintf(stdout, "reset: spend and income verdicts on other sources that paired against %s "+
			"are now stale — run 'wealthdb load -a' to re-assert them\n", ids[0])
	}
	return firstErr
}
