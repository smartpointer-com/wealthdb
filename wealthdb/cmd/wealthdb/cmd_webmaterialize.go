package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/version"
)

func init() {
	register("web-materialize", cmdWebMaterialize)
}

// cmdWebMaterialize rewrites the derived tables in the live gold DB.
// When the stored inputs match the current loads, config, engine and
// day, it returns without writing.
// The serving tables hold the daily histories, the latest holdings and
// every transaction line the serving views read, each its report
// macro's rows verbatim (gold.MaterializeServingTables). report_returns holds the
// full RunReturns matrix (every grain × period × reporting currency,
// plus the per-year windowed since-<year> summaries) with the
// CLI-default knobs and the same wealthdb.cfg returns settings a CLI
// run applies (returnsCfgSettings) — each base partition is the
// verbatim output of a bare
// `wealthdb returns <grain> --period <granularity> --method both -x <CCY>`.
// report_gains holds the monthly gains rows per reporting currency, the
// rows `wealthdb gains <grain> --period monthly -x <CCY>` sums.
// report_exposure holds the look-through at every month end and today
// per reporting currency; an instant's class totals are those of
// `wealthdb exposure classes -d <day> -x <CCY>`. The yield tables hold
// every holding's yield figures at today and the month ends before, and
// today's income projection, those of `wealthdb yield positions` and
// `wealthdb income`; report_cashflow_plan holds the look-back parts of
// `wealthdb cashflow projection` (both gold.MaterializeYield). The
// host-side `wealthdb web` wrapper runs it right before snapshotting so
// the Metabase dashboards are as fresh as the holdings. Hidden from
// `wealthdb help`; it's plumbing for `wealthdb web`, not a user-facing
// command (diagnostic knobs stay on `wealthdb returns` and `wealthdb
// gains`). A concurrent write command holds the gold write mutex, so
// the gate fails cleanly here and the wrapper aborts before the
// snapshot is touched.
func cmdWebMaterialize(ctx context.Context, g globalFlags, _ []string, _ io.Reader, _, stderr io.Writer) error {
	// "key" times the config read and the read-only look that decides
	// whether to rebuild, "open" the read-write open and its migrations,
	// and "close" the key's write and the close, which checkpoints what
	// the phases wrote.
	start := time.Now()
	// The bytes the key hashes are read before Load parses the file. An
	// edit between the two reads leaves a key older than the tables, so
	// the next run rebuilds rather than skip over them.
	raw, readErr := os.ReadFile(g.ConfigPath)
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	if readErr != nil {
		return fmt.Errorf("hash config: %w", readErr)
	}

	// A read-only look first. The read-write open migrates and stamps
	// the file, which would make a gold that did not need rewriting
	// look newer than its snapshot.
	lock, err := gateGoldForWrite(g, cfg, "web-materialize", defaultMissingGoldMsg)
	if err != nil {
		return err
	}
	defer lock.unlock()
	hash, err := gold.HashInputs(raw, returnsCfgFiles(cfg)...)
	if err != nil {
		return err
	}
	key := gold.MaterializeKey{ConfigHash: hash, EngineVersion: engineVersion(),
		AsOf: anchorToDay(g.clock().UTC(), true).Unix()}
	same, err := materializeInputsSame(ctx, cfg, key)
	if err != nil {
		return err
	}
	printTiming(stderr, "key", start)
	if same {
		fmt.Fprintf(stderr, "web-materialize: inputs unchanged\n")
		return nil
	}

	opened := time.Now()
	db, err := retryGoldLock(ctx, func() (*sql.DB, error) {
		return gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	})
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()
	printTiming(stderr, "open", opened)
	// The load key of the migrated gold. No phase writes a table the key
	// stamps (gold's TestNoPhaseWritesALoadKeyTable), so it still holds
	// once the phases are done.
	if key.LoadKey, err = gold.LoadKey(ctx, db); err != nil {
		return err
	}
	// Each phase commits on its own. A run that stops partway must leave
	// no key, or a later run with the earlier inputs would match tables
	// that are half rewritten.
	if err := gold.ClearMaterializeKey(ctx, db, gold.WebMaterializeScope); err != nil {
		return err
	}

	inceptionOv, exclude, hide, policyOv, matching := returnsCfgSettings(cfg)
	// One computed_at for every phase that stamps it.
	p := gold.MaterializeParams{
		ToEpoch:            key.AsOf,
		ComputedAt:         time.Now().Unix(),
		InceptionOverrides: inceptionOv,
		ReturnsExclude:     exclude,
		ReturnsHide:        hide,
		PolicyOverrides:    policyOv,
		TransferMatching:   matching,
		CashflowLookback:   cfg.CashflowLookback(),
	}
	if err := runMaterializePhases(ctx, stderr, db, p, webMaterializePhases); err != nil {
		return err
	}
	closing := time.Now()
	if err := gold.WriteMaterializeKey(ctx, db, gold.WebMaterializeScope, key); err != nil {
		return err
	}
	err = db.Close()
	printTiming(stderr, "close", closing)
	return err
}

// materializePhase is one phase of web-materialize: its name on the
// timing line, the run that rewrites its tables and returns their row
// count, and the summary line of that count.
type materializePhase struct {
	name    string
	run     func(context.Context, *sql.DB, gold.MaterializeParams) (int, error)
	summary func(n int) string
}

// webMaterializePhases are web-materialize's phases, in the order they
// run.
var webMaterializePhases = []materializePhase{
	{"serving", gold.MaterializeServingTables, func(n int) string {
		return fmt.Sprintf("serving tables: materialized %d rows (histories, latest holdings, transaction lines)", n)
	}},
	{"returns", gold.MaterializeReturns, func(n int) string {
		return fmt.Sprintf("returns: materialized %d rows (4 grains x 4 periods x %d currencies, plus per-year windows)",
			n, len(gold.MaterializedCurrencies()))
	}},
	{"gains", gold.MaterializeGains, func(n int) string {
		return fmt.Sprintf("gains: materialized %d rows (monthly x %d currencies x %d readings of a missing cost basis)",
			n, len(gold.MaterializedCurrencies()), len(lots.MissingBasisReadings))
	}},
	{"exposure", gold.MaterializeExposure, func(n int) string {
		return fmt.Sprintf("exposure: materialized %d rows (month ends x %d currencies)", n, len(gold.MaterializedCurrencies()))
	}},
	{"yield", gold.MaterializeYield, func(n int) string {
		return fmt.Sprintf("yield: materialized %d rows (holdings at today and %d month ends x %d currencies, today's projection)",
			n, gold.YieldHistoryMonths, len(gold.MaterializedCurrencies()))
	}},
}

// runMaterializePhases runs each phase in order over db with p, under
// timedPass, and prints its summary once it succeeds. It stops at the
// first failure, which prints its timing line and no summary.
func runMaterializePhases(ctx context.Context, stderr io.Writer, db *sql.DB, p gold.MaterializeParams, phases []materializePhase) error {
	for _, ph := range phases {
		var n int
		if err := timedPass(stderr, ph.name, func() (err error) {
			n, err = ph.run(ctx, db, p)
			return err
		}); err != nil {
			return err
		}
		fmt.Fprintln(stderr, ph.summary(n))
	}
	return nil
}

// engineVersion is the version string plus a hash of this executable.
// Two builds of one dirty commit share the version string. The hash
// does not match until the bytes do, and one process still matches itself.
func engineVersion() string {
	sum, err := executableHash()
	if err != nil {
		// A later read of the same binary must not share this string,
		// or two unreadable executables would skip the rebuild.
		return version.String() + " unread " + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return version.String() + " " + sum
}

func executableHash() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// materializeInputsSame reports whether gold was already materialized
// from key's config, engine and end of day and from the loads gold
// holds now. The look is read-only, so a gold that matches is left
// untouched. A gold with a migration still to apply is not the same:
// the read-write open applies it, and the tables it adds are read.
func materializeInputsSame(ctx context.Context, cfg *config.Config, key gold.MaterializeKey) (bool, error) {
	db, err := retryGoldLock(ctx, func() (*sql.DB, error) {
		return gold.Open(cfg.GoldDB, gold.ModeReadOnly)
	})
	if err != nil {
		return false, errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()
	if pending, err := gold.MigrationsPending(ctx, db); err != nil || pending {
		return false, err
	}
	load, err := gold.LoadKey(ctx, db)
	if err != nil {
		return false, err
	}
	stored, ok, err := gold.ReadMaterializeKey(ctx, db, gold.WebMaterializeScope)
	if err != nil {
		return false, err
	}
	return ok && stored.EngineVersion == key.EngineVersion &&
		stored.LoadKey == load && stored.ConfigHash == key.ConfigHash && stored.AsOf == key.AsOf, nil
}
