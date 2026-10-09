package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("compact", cmdCompact)
}

// compactParityTables are the core fact/dimension tables whose row
// counts must survive a compaction rewrite unchanged. A mismatch
// between the live DB and the freshly rewritten temp aborts the swap.
var compactParityTables = []string{
	"positions", "transactions", "fx_rates", "cash_balances", "accounts",
	"position_lots", "realized_lots",
}

// cmdCompact rewrites the live gold DB into a fresh, compact file and
// atomically swaps it into place. Repeated in-place DELETE+INSERT (the
// load / reset path) leaves dead row-group versions that DuckDB never
// truncates from the file; a clean rewrite reclaims that space without
// re-merging silver. Concurrent readers keep the old file until they
// close.
func cmdCompact(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "build the compacted file, report the reclaim, then discard it (no swap)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb compact [--dry-run]

Rewrite the gold DB into a fresh file to reclaim dead space left by
repeated in-place loads, then atomically swap it over the live path.
Data is copied faithfully (all tables, views, indexes) — no silver
re-merge. Concurrent readers keep the old file until they close.

--dry-run builds the compacted file, reports the reclaimable bytes,
then discards it without touching the live DB.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "compact: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "compact: unexpected positional argument %q", fs.Arg(0))
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	// Compact rewrites the gold file in place (via swap); it needs the
	// same RW + existence gating as reset / reload, but opens nothing
	// here — the rewrite ATTACHes the live DB read-only. The write
	// mutex is held across the whole rebuild-and-swap for that reason:
	// the build detaches the live file long before the rename, and a
	// concurrent writer's rows would land in the inode it unlinks.
	lock, err := gateGoldForWrite(g, cfg, "compact",
		"gold database %q does not exist. Nothing to compact.")
	if err != nil {
		return err
	}
	defer lock.unlock()

	build := compactBuild(ctx, cfg.GoldDB)

	if *dryRun {
		// Build into a private temp, measure, then discard without
		// swapping. Honest number, live DB untouched.
		tmp := cfg.GoldDB + ".compact-dryrun-" + strconv.Itoa(os.Getpid())
		defer func() {
			_ = os.Remove(tmp)
			_ = os.Remove(tmp + ".wal")
		}()
		if err := build(tmp); err != nil {
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("compact --dry-run: %w", err))
		}
		oldSize, err := fileBytes(cfg.GoldDB)
		if err != nil {
			return err
		}
		newSize, err := fileBytes(tmp)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "compact --dry-run: current %s → compacted %s (reclaim %s)\n",
			formatBytes(oldSize), formatBytes(newSize), formatBytes(oldSize-newSize))
		return nil
	}

	reclaimed, err := gold.BuildFreshAndSwap(ctx, cfg.GoldDB, build)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("compact: %w", err))
	}
	fmt.Fprintf(stdout, "compact: rewrote gold, reclaimed %s\n", formatBytes(reclaimed))
	return nil
}

// compactBuild returns a build function for BuildFreshAndSwap that
// rewrites the whole gold DB at goldPath into tmpPath via DuckDB's
// COPY FROM DATABASE — preserving every table, view and index — then
// asserts row-count parity on the core tables before returning. A
// parity mismatch fails the build, so BuildFreshAndSwap never swaps a
// lossy rewrite over the live DB.
func compactBuild(ctx context.Context, goldPath string) func(string) error {
	return func(tmp string) error {
		// ATTACH is per-connection, so the multi-statement sequence
		// must run on one pinned *sql.Conn — the pool would otherwise
		// scatter the statements across connections.
		db, err := sql.Open("duckdb", ":memory:")
		if err != nil {
			return fmt.Errorf("open compaction driver: %w", err)
		}
		defer db.Close()
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("pin compaction connection: %w", err)
		}
		defer conn.Close()

		for _, stmt := range []string{
			"ATTACH " + sqlLiteral(goldPath) + " AS src (READ_ONLY)",
			"ATTACH " + sqlLiteral(tmp) + " AS dst",
			"COPY FROM DATABASE src TO dst",
			// Flush dst to disk and truncate its WAL so the file is
			// complete and clean before the pinned conn closes.
			"CHECKPOINT dst",
		} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("compact rewrite (%s): %w", stmt, err)
			}
		}

		// Strong correctness gate: the core tables must have identical
		// row counts in the source and the freshly written copy.
		for _, tbl := range compactParityTables {
			var srcN, dstN int64
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM src."+tbl).Scan(&srcN); err != nil {
				return fmt.Errorf("compact parity read src.%s: %w", tbl, err)
			}
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM dst."+tbl).Scan(&dstN); err != nil {
				return fmt.Errorf("compact parity read dst.%s: %w", tbl, err)
			}
			if srcN != dstN {
				return fmt.Errorf("compact parity check failed for %s: src=%d dst=%d rows", tbl, srcN, dstN)
			}
		}
		return nil
	}
}

// sqlLiteral renders s as a single-quoted DuckDB string literal,
// doubling any embedded single quotes.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// fileBytes returns the byte size of the file at path.
func fileBytes(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("stat %q: %w", path, err))
	}
	return fi.Size(), nil
}

// formatBytes renders a byte count in the largest unit that keeps the
// value at or above 1, to two decimals (e.g. "1.50 GB", "512 B").
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
