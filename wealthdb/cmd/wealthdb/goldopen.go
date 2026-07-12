package main

import (
	"database/sql"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
)

// defaultMissingGoldMsg is the ExitMissingDB message the read and
// write helpers use when the gold DB doesn't exist yet. A few
// commands vary it (see openGoldForRead's optional argument and the
// missingDBMsg parameter of the write helpers).
const defaultMissingGoldMsg = "gold database %q does not exist. Run 'wealthdb init' first (requires write access)."

// openGoldForRead opens the gold DB for a read-side subcommand
// (positions / accounts / portfolios / transactions). It picks
// read-write when the filesystem allows it — so an outstanding
// migration is applied on open — and read-only when -r is set or
// the path isn't writable. Returns a clear ExitMissingDB error
// when the DB doesn't exist yet. missingDBMsg optionally overrides
// the ExitMissingDB message (a format string taking the DB path)
// for commands whose wording differs. The caller owns db.Close().
func openGoldForRead(g globalFlags, cfg *config.Config, missingDBMsg ...string) (*sql.DB, error) {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		msg := defaultMissingGoldMsg
		if len(missingDBMsg) > 0 {
			msg = missingDBMsg[0]
		}
		return nil, errs.Newf(errs.ExitMissingDB, msg, cfg.GoldDB)
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

// gateGoldForWrite verifies the gold DB exists and is writable for a
// write-side subcommand, returning a typed error (ExitMissingDB or
// ExitRWNeeded) when not. cmdName names the command in the read-only
// rejection; missingDBMsg is the ExitMissingDB message (a format
// string taking the DB path) — commands vary it ("Run 'wealthdb init'
// first" vs "Nothing to reset" vs "Nothing to compact"). It does not
// open the DB: commands that rewrite the file via swap machinery
// (compact, reload) gate without holding a handle.
func gateGoldForWrite(g globalFlags, cfg *config.Config, cmdName, missingDBMsg string) error {
	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB, missingDBMsg, cfg.GoldDB)
	}
	if dec.Mode != pathmode.ModeReadWrite {
		return errs.Newf(errs.ExitRWNeeded,
			"'%s' requires write access to the gold database, but '%s' is read-only (detected: %s).",
			cmdName, cfg.GoldDB, dec.Reason)
	}
	return nil
}

// openGoldForWrite gates as gateGoldForWrite does, then opens the gold
// DB read-write (applying any outstanding migration on open). The
// caller owns db.Close(). Used by the write commands that mutate the
// live file directly (load, reset, web-materialize); compact and
// reload rewrite via a swap and only need gateGoldForWrite.
func openGoldForWrite(g globalFlags, cfg *config.Config, cmdName, missingDBMsg string) (*sql.DB, error) {
	if err := gateGoldForWrite(g, cfg, cmdName, missingDBMsg); err != nil {
		return nil, err
	}
	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return nil, errs.Wrap(errs.ExitOpenFailed, err)
	}
	return db, nil
}
