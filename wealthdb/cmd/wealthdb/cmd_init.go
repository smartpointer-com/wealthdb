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
	"github.com/ptu/wealthdb/internal/pathmode"
)

func init() {
	register("init", cmdInit)
}

func cmdInit(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb init

Creates the gold DuckDB file at the path given by the gold_db
field in the config file, runs all migrations, and exits. Fails
if the file already exists.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "init: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "init: unexpected positional argument %q", fs.Arg(0))
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, true /* init */)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if dec.DBExists {
		return errs.Newf(errs.ExitInitExisting,
			"gold database %q already exists. Use 'wealthdb reset -a' to clear data, "+
				"or delete the file manually if you really want a fresh DB.", cfg.GoldDB)
	}
	if dec.Mode != pathmode.ModeReadWrite {
		return errs.Newf(errs.ExitRWNeeded,
			"'init' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cfg.GoldDB, dec.Reason)
	}

	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	if err := gold.Migrate(ctx, db); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("apply migrations: %w", err))
	}

	fmt.Fprintf(stdout, "wealthdb init: created gold database at %s\n", cfg.GoldDB)
	return nil
}
