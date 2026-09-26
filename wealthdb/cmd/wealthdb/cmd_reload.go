package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/loader"
)

func init() {
	register("reload", cmdReload)
}

// cmdReload is the reset-then-load shortcut. Useful after upgrading
// wealthdb when adapter logic changes affect already-loaded rows —
// the per-column upsert guard makes incremental load skip
// re-touching snapshots at-or-before the watermark, so values that
// the new adapter would now emit don't backfill into gold.
//
// 'reload -a' rebuilds every source into a FRESH gold file and
// atomically swaps it over the live path, which also compacts the
// file (a fresh build carries none of the dead row-group versions an
// in-place reset+load leaves behind). --in-place keeps the older
// reset-then-load-on-the-live-DB behaviour. Single-source
// 'reload <id>' always stays in-place.
func cmdReload(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "reload every registered silver source")
	inPlace := fs.Bool("in-place", false, "with -a: reset+load on the live DB instead of building a fresh file")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb reload <silver_source_id> | -a [--in-place]

Reset then re-load one silver source (or every registered silver
source with -a). Equivalent to running 'wealthdb reset <id>'
followed by 'wealthdb load <id>'.

With -a, the default builds a fresh gold file from every source and
atomically swaps it over the live path — concurrent readers keep the
old file until they close, and the rebuilt file is compact. Pass
--in-place to reset+load on the live DB instead.

Use case: after upgrading wealthdb to a binary whose adapter logic
populates new columns or fixes a projection, the upsert guard
prevents the new values from backfilling onto snapshots already at
or below the high watermark. Reload forces a full re-projection.`)
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "reload: bad flags")
	}
	if *inPlace && !*all {
		fs.Usage()
		return errs.Newf(2, "reload: --in-place only applies to '-a'")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	ledger, err := loader.ParseTransferLedger(cfg.EquityTransfers)
	if err != nil {
		return err
	}

	// Resolve targets to a list of (id, config.SilverSource) pairs so
	// the load step has the kind/path it needs without re-resolving.
	var targets []config.SilverSource
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "reload: '-a' and a positional id are mutually exclusive")
	case *all:
		targets = cfg.SilverSources
		if len(targets) == 0 {
			return fmt.Errorf("reload: -a passed but no silver sources are configured")
		}
	case fs.NArg() == 1:
		s, ok := cfg.Lookup(fs.Arg(0))
		if !ok {
			return fmt.Errorf("reload: silver source %q not found in config", fs.Arg(0))
		}
		targets = []config.SilverSource{*s}
	default:
		fs.Usage()
		return errs.Newf(2, "reload: expected one silver_source_id or -a")
	}

	// Reload is RW (combines reset + load). Gate here; the fresh-swap
	// and in-place paths each open the DB themselves. The write mutex
	// is held for the whole command, including the fresh-file rebuild:
	// nothing keeps the live file open between the verdict-store
	// carry-across and the swap, so a verdict written in that window
	// would land in the inode the rename unlinks.
	lock, err := gateGoldForWrite(g, cfg, "reload",
		"gold database %q does not exist. Run 'wealthdb init' first (requires write access).")
	if err != nil {
		return err
	}
	defer lock.unlock()

	// Default '-a': build a fresh file from scratch and swap it in.
	if *all && !*inPlace {
		return reloadFreshAndSwap(ctx, cfg, targets, ledger, stdout, stderr)
	}

	return reloadInPlace(ctx, cfg, targets, ledger, stdout, stderr)
}

// reloadFreshAndSwap builds a complete gold DB from every target
// source into a fresh temp file and atomically swaps it over the live
// path. The temp starts empty, so no per-source reset is needed; the
// load repopulates load_audit / silver_sources exactly as a live
// reset+load would. If any source fails to build, the error is
// surfaced and the live DB is left untouched (no swap).
func reloadFreshAndSwap(
	ctx context.Context,
	cfg *config.Config,
	targets []config.SilverSource,
	ledger map[string][]loader.TransferEntry,
	stdout, stderr io.Writer,
) error {
	var buildErr error
	build := func(tmp string) error {
		db, err := gold.Open(tmp, gold.ModeReadWrite)
		if err != nil {
			return err
		}
		// The temp DB is empty, so no per-source reset is needed —
		// each Load registers the source and populates its rows fresh.
		ld := loader.New(db)
		var firstErr error
		for _, s := range targets {
			spec, err := buildSourceSpec(s, cfg, ledger)
			if err != nil {
				fmt.Fprintf(stderr, "reload: %s: %s\n", s.ID, err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			res, err := ld.Load(ctx, spec)
			if err != nil {
				fmt.Fprintf(stderr, "reload: %s: load: %s\n", s.ID, err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			fmt.Fprintf(stdout, "reload: %s: %d snapshot row(s) + %d transaction(s) (watermark → %d)\n",
				s.ID, res.SnapshotsLoaded, res.TransactionsLoaded, res.ChangeNumberAfter)
		}
		if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
			fmt.Fprintf(stderr, "reload: warning: could not stamp FX priorities: %s\n", err.Error())
		}
		if err := syncDeclaredAccounts(ctx, db, cfg, "reload", stdout); err != nil {
			fmt.Fprintf(stderr, "reload: %s\n", err.Error())
			firstErr = errors.Join(firstErr, err)
		}
		// Carry the paid-for stores over from the outgoing file. A hard
		// error: those verdicts were bought from a model and have no
		// config backup to re-stamp them from. Both spending families
		// have one and so does symbol resolution, and a store missing
		// from the list is lost silently — which is why the list is
		// walked rather than one store named.
		var present bool
		var carried int
		for _, store := range paidStores {
			present, carried, err = carryVerdictStore(ctx, db, cfg.GoldDB, store)
			if err != nil {
				_ = db.Close()
				return err
			}
			switch {
			case carried > 0:
				fmt.Fprintf(stdout, "reload: carried %d %s verdict(s) across the rebuild\n", carried, store.noun)
			case present:
				fmt.Fprintf(stdout, "reload: outgoing gold's %s store is empty; nothing to carry\n", store.noun)
			default:
				fmt.Fprintf(stdout, "reload: outgoing gold has no %s store; nothing to carry\n", store.noun)
			}
		}
		// Re-assert the deterministic verdicts of both families, AFTER the
		// stores have been carried across: the signature-version re-key
		// reads that store, and a pass that ran before the carry would
		// see it empty and carry nothing forward. Before the CHECKPOINT,
		// so the swapped-in file is enriched rather than needing a
		// follow-up load to become correct.
		if err := runEnrichmentPass(ctx, db, cfg, stdout); err != nil {
			fmt.Fprintf(stderr, "reload: %s\n", err.Error())
			firstErr = errors.Join(firstErr, err)
		}
		// Checkpoint then close so the temp file is complete and clean
		// (no leftover WAL) before it is verified and swapped.
		if _, err := db.ExecContext(ctx, "CHECKPOINT"); err != nil {
			_ = db.Close()
			return fmt.Errorf("checkpoint rebuilt gold: %w", err)
		}
		if err := db.Close(); err != nil {
			return fmt.Errorf("close rebuilt gold: %w", err)
		}
		// Surface a per-source failure so BuildFreshAndSwap does NOT
		// swap a partial rebuild over the live DB.
		buildErr = firstErr
		return firstErr
	}

	reclaimed, err := gold.BuildFreshAndSwap(ctx, cfg.GoldDB, build)
	if err != nil {
		if buildErr != nil {
			// A source failed to build; the live DB is untouched.
			return buildErr
		}
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("reload -a: %w", err))
	}
	fmt.Fprintf(stdout, "reload: rebuilt gold from %d source(s), reclaimed %s\n",
		len(targets), formatBytes(reclaimed))
	return nil
}

// paidStore names one paid-for verdict store: the table, the word its
// rows are about, and its key column — which the carry-across checks
// for by name, since a store whose key did not survive into the
// rebuilt schema cannot be carried at all.
type paidStore struct {
	table     string
	noun      string
	signature string
}

// paidStores is every store `reload -a` must carry. Adding a family
// means adding a row here; a store left out is lost on the next
// rebuild, silently and with nothing to restore it from.
var paidStores = []paidStore{
	{"spend_merchant_categories", "merchant", "merchant_signature"},
	{"income_payer_categories", "payer", "payer_signature"},
	// The oldest of the three and the one that reads least like a
	// "verdict store", which is how it sat outside this list: symbol
	// resolutions are per-source rather than global, and they resolve an
	// instrument rather than categorise a counterparty. They are paid for
	// by the same model and regenerated by nothing, so they belong here.
	{"symbol_resolutions", "symbol", "lookup_value"},
}

// carryVerdictStore copies one global verdict store out of the
// outgoing gold file into the freshly built one, and reports whether
// the outgoing file had the table at all and how many rows it carried.
//
// 'reload -a' builds an empty temp file and swaps it over the live
// path, and the verdict stores are the one thing in gold that a
// rebuild cannot regenerate: FX priorities and the account/instrument
// overrides are re-stamped from config, every fact table is re-derived
// from silver, but an LLM verdict on a merchant or payer signature
// exists only in its store and was paid for. Without this
// carry-across, routine compaction-by-reload would silently wipe it.
//
// The copied column list is derived — the intersection of the two
// files' columns, read from duckdb_columns() — rather than named in
// Go, so neither drift direction needs a Go-side edit: a column a
// later migration adds is carried as soon as both files have it, and
// a column only the outgoing file has is left behind instead of
// raising a binder error.
//
// The intersection alone does not make the live-behind direction
// SAFE, only silent: a column the rebuilt schema requires and the
// outgoing file does not have yet is dropped from the SELECT and then
// fails the target table's NOT NULL on INSERT. Every column of these
// tables is NOT NULL, so that is the normal shape of the next additive
// migration met by a live file no load has touched since. Such
// columns are therefore named in an error before the INSERT runs,
// rather than surfacing as a constraint violation with nothing
// actionable in it.
//
// ATTACH is per-connection, so the sequence runs on one pinned conn.
// A live file predating the store's own migration — 0041 for the
// merchant store, 0070 for the payer store — has no such table, which
// carries nothing rather than erroring.
func carryVerdictStore(ctx context.Context, db *sql.DB, livePath string, store paidStore) (bool, int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("pin connection for the %s-store carry-across: %w", store.noun, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx,
		"ATTACH "+sqlLiteral(livePath)+" AS live_gold (READ_ONLY)"); err != nil {
		return false, 0, fmt.Errorf("attach live gold for the %s-store carry-across: %w", store.noun, err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH live_gold") }()

	var tables int
	if err := conn.QueryRowContext(ctx, `
        SELECT count(*) FROM duckdb_tables()
         WHERE database_name = 'live_gold'
           AND table_name = ?`, store.table).Scan(&tables); err != nil {
		return false, 0, fmt.Errorf("probe live gold for the %s store: %w", store.noun, err)
	}
	if tables == 0 {
		return false, 0, nil
	}

	// An outgoing store with no rows carries nothing, so it also
	// cannot lose anything — it takes the column check with it, and a
	// routine rebuild is not blocked over rows that do not exist.
	var live int
	if err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM live_gold."+store.table).Scan(&live); err != nil {
		return true, 0, fmt.Errorf("count the outgoing %s store: %w", store.noun, err)
	}
	if live == 0 {
		return true, 0, nil
	}

	if err := checkStoreColumnsSatisfiable(ctx, conn, store); err != nil {
		return true, 0, err
	}
	columns, err := carriedStoreColumns(ctx, conn, store)
	if err != nil {
		return true, 0, err
	}

	res, err := conn.ExecContext(ctx,
		"INSERT INTO "+store.table+" ("+columns+") "+
			"SELECT "+columns+" FROM live_gold."+store.table)
	if err != nil {
		return true, 0, fmt.Errorf("carry the %s store across the rebuild: %w", store.noun, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return true, 0, fmt.Errorf("count the carried %s store: %w", store.noun, err)
	}
	return true, int(n), nil
}

// checkStoreColumnsSatisfiable refuses the carry-across when the
// rebuilt store requires a column the outgoing one cannot
// supply: NOT NULL, no default, and absent from the outgoing table.
// The intersection drops such a column from both sides of the INSERT,
// which leaves the target table to reject the row — a constraint
// error naming nothing actionable, in the middle of a rebuild. Named
// here instead, together with the command that fixes it: a read-write
// open migrates the live file, and 'reload -a' never opens it that
// way.
func checkStoreColumnsSatisfiable(ctx context.Context, conn *sql.Conn, store paidStore) error {
	rows, err := conn.QueryContext(ctx, `
        SELECT f.column_name
          FROM duckdb_columns() f
         WHERE f.database_name  = current_database()
           AND f.table_name     = ?
           AND NOT f.is_nullable
           AND f.column_default IS NULL
           AND f.column_name NOT IN (
                 SELECT l.column_name
                   FROM duckdb_columns() l
                  WHERE l.database_name = 'live_gold'
                    AND l.table_name    = ?)
         ORDER BY f.column_index`, store.table, store.table)
	if err != nil {
		return fmt.Errorf("compare the %s store's required columns: %w", store.noun, err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan the %s store's required columns: %w", store.noun, err)
		}
		missing = append(missing, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("the outgoing gold's %s store (%s) has no %s column(s), which the rebuilt schema "+
		"requires (NOT NULL, no default); run 'wealthdb load <silver_source_id>' first — it opens the live "+
		"file read-write and migrates it — then re-run 'reload -a'",
		store.noun, store.table, strings.Join(missing, ", "))
}

// carriedStoreColumns returns the quoted, comma-joined column list the
// two copies of ONE verdict store have in common — the freshly built
// one (current_database(), which resolves to the rebuild temp file even
// with live_gold attached) intersected with the outgoing one, in the
// new table's own column order.
func carriedStoreColumns(ctx context.Context, conn *sql.Conn, store paidStore) (string, error) {
	rows, err := conn.QueryContext(ctx, `
        SELECT f.column_name
          FROM duckdb_columns() f
          JOIN duckdb_columns() l
            ON l.database_name = 'live_gold'
           AND l.table_name    = ?
           AND l.column_name   = f.column_name
         WHERE f.database_name = current_database()
           AND f.table_name    = ?
         ORDER BY f.column_index`, store.table, store.table)
	if err != nil {
		return "", fmt.Errorf("read the %s store's columns: %w", store.noun, err)
	}
	defer rows.Close()

	var quoted []string
	keyed := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", fmt.Errorf("scan the %s store's columns: %w", store.noun, err)
		}
		if name == store.signature {
			keyed = true
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !keyed {
		return "", fmt.Errorf("the outgoing %s store shares no key column with the rebuilt one; refusing to carry it",
			store.noun)
	}
	return strings.Join(quoted, ", "), nil
}

// reloadInPlace is the original reset-then-load-on-the-live-DB path,
// used for single-source reloads and for 'reload -a --in-place'. The
// verdict stores need no carry-across here: the live file is never
// replaced, and Reset leaves both of them alone.
func reloadInPlace(
	ctx context.Context,
	cfg *config.Config,
	targets []config.SilverSource,
	ledger map[string][]loader.TransferEntry,
	stdout, stderr io.Writer,
) error {
	db, err := gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	ld := loader.New(db)
	var firstErr error
	for _, s := range targets {
		// Reset is best-effort per source — surface failures but
		// keep going so '-a' isn't blocked by a single bad source.
		if err := ld.Reset(ctx, s.ID); err != nil {
			fmt.Fprintf(stderr, "reload: %s: reset: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		spec, err := buildSourceSpec(s, cfg, ledger)
		if err != nil {
			fmt.Fprintf(stderr, "reload: %s: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		res, err := ld.Load(ctx, spec)
		if err != nil {
			fmt.Fprintf(stderr, "reload: %s: load: %s\n", s.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch {
		case res.AlreadyUpToDate:
			// Vacuously true after a reset — silver has no
			// changes for an empty watermark, e.g. no dump_runs.
			fmt.Fprintf(stdout, "reload: %s: reset; nothing to load (watermark %d)\n",
				s.ID, res.ChangeNumberAfter)
		default:
			fmt.Fprintf(stdout, "reload: %s: reset + %d snapshot row(s) + %d transaction(s) (watermark → %d)\n",
				s.ID, res.SnapshotsLoaded, res.TransactionsLoaded, res.ChangeNumberAfter)
		}
	}
	if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
		fmt.Fprintf(stderr, "reload: warning: could not stamp FX priorities: %s\n", err.Error())
	}
	if err := syncDeclaredAccounts(ctx, db, cfg, "reload", stdout); err != nil {
		fmt.Fprintf(stderr, "reload: %s\n", err.Error())
		firstErr = errors.Join(firstErr, err)
	}
	if err := runEnrichmentPass(ctx, db, cfg, stdout); err != nil {
		fmt.Fprintf(stderr, "reload: %s\n", err.Error())
		firstErr = errors.Join(firstErr, err)
	}
	return firstErr
}
