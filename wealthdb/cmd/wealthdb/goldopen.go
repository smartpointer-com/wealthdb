package main

import (
	"database/sql"

	"github.com/ptu/wealthdb/internal/config"
	"github.com/ptu/wealthdb/internal/errs"
	"github.com/ptu/wealthdb/internal/gold"
	"github.com/ptu/wealthdb/internal/pathmode"
)

// openGoldForRead opens the gold DB for a read-side subcommand
// (positions / accounts / portfolios / transactions). It picks
// read-write when the filesystem allows it — so an outstanding
// migration is applied on open — and read-only when -r is set or
// the path isn't writable. Returns a clear ExitMissingDB error
// when the DB doesn't exist yet. The caller owns db.Close().
func openGoldForRead(g globalFlags, cfg *config.Config) (*sql.DB, error) {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return nil, errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}
	openMode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		openMode = gold.ModeReadOnly
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	return db, nil
}
