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
	"sync"
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
		AsOf: anchorToDay(time.Now().UTC(), true).Unix()}
	same, err := materializeInputsSame(ctx, cfg, key)
	if err != nil {
		return err
	}
	if same {
		fmt.Fprintf(stderr, "web-materialize: inputs unchanged\n")
		return nil
	}

	db, err := retryGoldLock(ctx, func() (*sql.DB, error) {
		return gold.Open(cfg.GoldDB, gold.ModeReadWrite)
	})
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()
	// The load key of the migrated gold. No phase writes a table the key
	// stamps, so it still holds once the phases are done.
	if key.LoadKey, err = gold.LoadKey(ctx, db); err != nil {
		return err
	}
	// Each phase commits on its own. A run that stops partway must leave
	// no key, or a later run with the earlier inputs would match tables
	// that are half rewritten.
	if err := gold.ClearMaterializeKey(ctx, db, gold.WebMaterializeScope); err != nil {
		return err
	}

	toEpoch := key.AsOf
	// One computed_at for every phase that stamps it, read as the first
	// of them starts.
	computedAt := sync.OnceValue(func() int64 { return time.Now().Unix() })
	ccys := len(gold.MaterializedCurrencies())
	if err := runMaterializePhases(stderr, []materializePhase{
		{"serving", func() (int, error) {
			return gold.MaterializeServingTables(ctx, db)
		}, func(n int) string {
			return fmt.Sprintf("serving tables: materialized %d rows (histories, latest holdings, transaction lines)", n)
		}},
		{"returns", func() (int, error) {
			inceptionOv, exclude, hide, policyOv, matching := returnsCfgSettings(cfg)
			return gold.MaterializeReturns(ctx, db, gold.MaterializeParams{
				ToEpoch:            toEpoch,
				ComputedAt:         computedAt(),
				InceptionOverrides: inceptionOv,
				ReturnsExclude:     exclude,
				ReturnsHide:        hide,
				PolicyOverrides:    policyOv,
				TransferMatching:   matching,
			})
		}, func(n int) string {
			return fmt.Sprintf("returns: materialized %d rows (4 grains x 4 periods x %d currencies, plus per-year windows)", n, ccys)
		}},
		{"gains", func() (int, error) {
			return gold.MaterializeGains(ctx, db, toEpoch, computedAt())
		}, func(n int) string {
			return fmt.Sprintf("gains: materialized %d rows (monthly x %d currencies x %d readings of a missing cost basis)",
				n, ccys, len(lots.MissingBasisReadings))
		}},
		{"exposure", func() (int, error) {
			return gold.MaterializeExposure(ctx, db, toEpoch, computedAt())
		}, func(n int) string {
			return fmt.Sprintf("exposure: materialized %d rows (month ends x %d currencies)", n, ccys)
		}},
		{"yield", func() (int, error) {
			return gold.MaterializeYield(ctx, db, toEpoch, computedAt(), cfg.CashflowLookback())
		}, func(n int) string {
			return fmt.Sprintf("yield: materialized %d rows (holdings at today and %d month ends x %d currencies, today's projection)",
				n, gold.YieldHistoryMonths, ccys)
		}},
	}); err != nil {
		return err
	}
	return gold.WriteMaterializeKey(ctx, db, gold.WebMaterializeScope, key)
}

// materializePhase is one phase of web-materialize: its name on the
// timing line, the run that returns its row count, and the summary line
// of that count.
type materializePhase struct {
	name    string
	run     func() (int, error)
	summary func(n int) string
}

// runMaterializePhases runs each phase in order under timedPass and
// prints its summary once it succeeds. It stops at the first failure,
// which prints its timing line and no summary.
func runMaterializePhases(stderr io.Writer, phases []materializePhase) error {
	for _, p := range phases {
		var n int
		if err := timedPass(stderr, p.name, func() (err error) {
			n, err = p.run()
			return err
		}); err != nil {
			return err
		}
		fmt.Fprintln(stderr, p.summary(n))
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
