package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
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
// in-place reset+load leaves behind). --in-place resets and loads on
// the live DB instead. Named sources stay in-place, and the class,
// enrichment, lot, exposure and yield passes run once after all of
// them have loaded.
func cmdReload(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "reload every registered silver source")
	inPlace := fs.Bool("in-place", false, "with -a: reset+load on the live DB instead of building a fresh file")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb reload <silver_source_id>... | -a [--in-place]

Reset then re-load one or more silver sources (or every registered
silver source with -a). Equivalent to 'wealthdb reset <id>' followed
by 'wealthdb load <id>' for each named source. The class, enrichment,
lot, exposure and yield passes then run once.

With -a, the default builds a fresh gold file from every source and
atomically swaps it over the live path — concurrent readers keep the
old file until they close, and the rebuilt file is compact. Pass
--in-place to reset+load on the live DB instead.

Use case: after upgrading wealthdb to a binary whose adapter logic
populates new columns or fixes a projection, the upsert guard
prevents the new values from backfilling onto snapshots already at
or below the high watermark. Reload forces a full re-projection.`)
	}
	if err := parseFlags(fs, subargs, "reload"); err != nil {
		return err
	}
	if *inPlace && !*all {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "reload: --in-place only applies to '-a'")
	}
	ids, err := reloadIDs(fs, *all)
	if err != nil {
		return err
	}

	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	run, specs, err := prepareLoad(cfg, "reload", ids, stdout, stderr)
	if err != nil {
		return err
	}

	// Reload is RW (combines reset + load). Gate here; the fresh-swap
	// and in-place paths each open the DB themselves. The write mutex
	// is held for the whole command, including the fresh-file rebuild:
	// nothing keeps the live file open between the verdict-store
	// carry-across and the swap, so a verdict written in that window
	// would land in the inode the rename unlinks.
	lock, err := gateGoldForWrite(g, cfg, "reload", defaultMissingGoldMsg)
	if err != nil {
		return err
	}
	defer lock.unlock()

	// Default '-a': build a fresh file from scratch and swap it in.
	if *all && !*inPlace {
		return reloadFreshAndSwap(ctx, run, specs)
	}

	return reloadInPlace(ctx, run, specs)
}

// reloadIDs is the sources a reload names: nil for -a, otherwise the
// positional ids, in the order given. -a and an id do not combine,
// and an id named twice is a usage error rather than two resets.
func reloadIDs(fs *flag.FlagSet, all bool) ([]string, error) {
	if all {
		_, err := sourceArg(fs, all, "reload")
		return nil, err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return nil, errs.Newf(errs.ExitUsage, "reload: expected one or more silver_source_id or -a")
	}
	ids := fs.Args()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			fs.Usage()
			return nil, errs.Newf(errs.ExitUsage, "reload: %q named more than once", id)
		}
		seen[id] = true
	}
	return ids, nil
}

// reloadFreshAndSwap builds a complete gold DB from every target
// source into a fresh temp file and atomically swaps it over the live
// path. The temp starts empty, so no per-source reset is needed; the
// load repopulates load_audit / silver_sources exactly as a live
// reset+load would. If any source fails to build, the error is
// surfaced and nothing is swapped: the live DB keeps its rows.
func reloadFreshAndSwap(ctx context.Context, run loadRun, specs []loader.SourceSpec) error {
	// Migrate the outgoing file first, as any load does, by opening it
	// read-write once. Both files then have one schema, and the carry
	// copies each store by name. A build that fails after this leaves
	// the live file migrated, as a failed load does.
	live, err := retryGoldLock(ctx, func() (*sql.DB, error) { return gold.Open(run.cfg.GoldDB, gold.ModeReadWrite) })
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if run.priorTransactions, err = goldHasTransactions(ctx, live); err != nil {
		_ = live.Close()
		return err
	}
	if err := live.Close(); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("close migrated gold: %w", err))
	}

	var buildErr error
	build := func(tmp string) error {
		// BuildFreshAndSwap has removed any file at tmp, so it starts as
		// a copy of the migrated empty image (gold.OpenFresh) rather
		// than a run of every migration.
		db, err := gold.OpenFresh(tmp)
		if err != nil {
			return err
		}
		// The temp DB is empty, so no per-source reset is needed —
		// each Load registers the source and populates its rows fresh.
		firstErr := run.sources(ctx, loader.New(db), specs, false)
		// Carry the paid-for stores over from the outgoing file, between
		// the config stamps and the passes. A hard error: those verdicts
		// were bought from a model and have no config backup to re-stamp
		// them from. Both spending families have one and so does symbol
		// resolution, and a store missing from the list is lost silently
		// — which is why the list is walked rather than one store named.
		//
		// The passes run AFTER the carry: the signature-version re-key
		// reads the stores, and a pass that ran before the carry would
		// see them empty and carry nothing forward. Before the
		// CHECKPOINT, so the swapped-in file has had every pass rather
		// than needing a follow-up load to become correct.
		var carryErr error
		finishErr := run.finish(ctx, db, func() error {
			carryErr = carryStores(ctx, db, run.cfg.GoldDB, run.stdout, run.stderr)
			return carryErr
		})
		if carryErr != nil {
			_ = db.Close()
			return carryErr
		}
		firstErr = errors.Join(firstErr, finishErr)
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

	reclaimed, err := gold.BuildFreshAndSwap(ctx, run.cfg.GoldDB, build)
	if err != nil {
		if buildErr != nil {
			// A source failed to build; nothing was swapped.
			return buildErr
		}
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("reload -a: %w", err))
	}
	fmt.Fprintf(run.stdout, "reload: rebuilt gold from %d source(s), reclaimed %s\n",
		len(specs), formatBytes(reclaimed))
	return nil
}

// carryStores carries every paid store, then the cash-flow boundary,
// from the outgoing gold file at livePath into the rebuilt one, and
// says what each carried.
func carryStores(ctx context.Context, db *sql.DB, livePath string, stdout, stderr io.Writer) error {
	for _, store := range paidStores {
		carried, err := carryVerdictStore(ctx, db, livePath, store)
		if err != nil {
			return err
		}
		if carried > 0 {
			fmt.Fprintf(stdout, "reload: carried %d %s verdict(s) across the rebuild\n", carried, store.noun)
		} else {
			fmt.Fprintf(stdout, "reload: outgoing gold's %s store is empty; nothing to carry\n", store.noun)
		}
	}
	// The cash-flow boundary comes across too. The enrichment pass
	// rewrites it whole, but it reads an empty one as the first pass
	// ever to stamp it and says every earlier report was wrong. The
	// outgoing gold's boundary makes that true only where it had
	// none. Not carrying it costs a false notice, never a figure, so
	// a failure here warns and the rebuild goes on.
	if _, err := carryVerdictStore(ctx, db, livePath, boundaryStamp); err != nil {
		fmt.Fprintf(stderr, "reload: the cash-flow boundary did not carry across (%s); "+
			"the load may call itself its first pass\n", err.Error())
	}
	return nil
}

// paidStore names one paid-for verdict store: the table and the word
// its rows are about.
type paidStore struct {
	table string
	noun  string
}

// paidStores is every store `reload -a` must carry. Adding a family
// means adding a row here; a store left out is lost on the next
// rebuild, silently and with nothing to restore it from.
var paidStores = []paidStore{
	{"spend_merchant_categories", "merchant"},
	{"income_payer_categories", "payer"},
	// Symbol resolutions are per-source rather than global, and they
	// resolve an instrument rather than categorise a counterparty, but
	// they are paid for by the same model and regenerated by nothing.
	{"symbol_resolutions", "symbol"},
}

// boundaryStamp is the cash-flow boundary (cashflow_wrapper_sides), carried
// the way a paid store is, so the rebuilt file starts with the outgoing
// file's rows. It is in no list of paid stores: the enrichment pass
// re-stamps it from the config.
var boundaryStamp = paidStore{"cashflow_wrapper_sides", "boundary"}

// carryVerdictStore copies one table out of the outgoing gold file into
// the freshly built one, and reports how many rows it carried.
//
// 'reload -a' builds an empty temp file and swaps it over the live
// path, and the verdict stores are the one thing in gold that a
// rebuild cannot regenerate: FX priorities and the account/instrument
// overrides are re-stamped from config, every fact table is re-derived
// from silver, but an LLM verdict exists only in its store and was
// paid for. Without this
// carry-across, routine compaction-by-reload would silently wipe it.
//
// The outgoing file was migrated before the build, so the two copies
// of the store have one schema and the rows copy by name.
//
// ATTACH is per-connection, so the sequence runs on one pinned conn.
func carryVerdictStore(ctx context.Context, db *sql.DB, livePath string, store paidStore) (int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("pin connection for the %s-store carry-across: %w", store.noun, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx,
		"ATTACH "+sqlLiteral(livePath)+" AS live_gold (READ_ONLY)"); err != nil {
		return 0, fmt.Errorf("attach live gold for the %s-store carry-across: %w", store.noun, err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH live_gold") }()

	res, err := conn.ExecContext(ctx,
		"INSERT INTO "+store.table+" BY NAME SELECT * FROM live_gold."+store.table)
	if err != nil {
		return 0, fmt.Errorf("carry the %s store across the rebuild: %w", store.noun, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the carried %s store: %w", store.noun, err)
	}
	return int(n), nil
}

// reloadInPlace resets and loads each source on the live DB, for
// named sources and for 'reload -a --in-place'. The live file is never
// replaced, so nothing is carried across. Reset keeps the two global
// verdict stores and drops the source's symbol_resolutions, as
// `reload <id>` always has (DESIGN.md §9). The gold passes run once,
// after every named source has loaded.
func reloadInPlace(ctx context.Context, run loadRun, specs []loader.SourceSpec) error {
	db, err := retryGoldLock(ctx, func() (*sql.DB, error) { return gold.Open(run.cfg.GoldDB, gold.ModeReadWrite) })
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	if run.priorTransactions, err = goldHasTransactions(ctx, db); err != nil {
		return err
	}
	firstErr := run.sources(ctx, loader.New(db), specs, true)
	return errors.Join(firstErr, run.finish(ctx, db, nil))
}
