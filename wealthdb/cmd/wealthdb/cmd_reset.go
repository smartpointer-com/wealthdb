package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

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
	db, err := openGoldForWrite(g, cfg, "reset",
		"gold database %q does not exist. Nothing to reset.")
	if err != nil {
		return err
	}
	defer db.Close()

	ld := loader.New(db)

	// Build the target list. Order: explicit id wins; then -a;
	// then complain.
	var ids []string
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
	return firstErr
}
