package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/pathmode"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/wizard"
)

func init() {
	register("init", cmdInit)
}

func cmdInit(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb init

Create the database at the path the config names (gold_db), and name
the next step. A database that exists already is left as it is.`)
	}
	if err := parseFlags(fs, subargs, "init"); err != nil {
		return err
	}
	if err := noPositionals(fs, "init"); err != nil {
		return err
	}

	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	if err := createGold(ctx, g, cfg, stdout); err != nil {
		return err
	}
	fmt.Fprint(stdout, initNextStep(colors(stdout), g.ConfigPath, cfg))
	return nil
}

// createGold creates the database the config names and runs every
// migration, or says where it is when it exists.
func createGold(ctx context.Context, g globalFlags, cfg *config.Config, stdout io.Writer) error {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, true /* init */)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if dec.DBExists {
		fmt.Fprintf(stdout, "wealthdb init: the database exists already at %s\n", cfg.GoldDB)
		return nil
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
	if err := claimGoldForEngine(cfg.GoldDB, "init", true); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "wealthdb init: created the database at %s\n", cfg.GoldDB)
	return nil
}

// initNextStep is what follows a database: the setup when no source is
// configured, else the collect commands of the sources without data,
// then the load. The engine commands it names read the config at
// configPath (commandFor).
func initNextStep(p wizard.Palette, configPath string, cfg *config.Config) string {
	if len(cfg.SilverSources) == 0 {
		return wizard.ToDo(p, []wizard.Step{{Command: setupCommand(configPath), Does: "Adds a bank or a broker to read."}})
	}
	feeds := make([][]config.Feed, len(cfg.SilverSources))
	for i := range cfg.SilverSources {
		feeds[i] = cfg.SilverSources[i].Feeds()
	}
	return wizard.NextSteps(p, commandFor(configPath, "load -a"), feeds)
}
