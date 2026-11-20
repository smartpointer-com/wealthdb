package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

func init() {
	register("load", cmdLoad)
}

func cmdLoad(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("a", false, "load all configured silver sources")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: wealthdb load <silver_source_id> | -a

Merge new silver snapshots into gold for one silver source, or
for all configured sources (-a). A silver older than its adapter
still loads, and the load prints one line naming the parts it lacks.
See docs/DESIGN.md §8 for the load semantics.`)
	}
	if err := parseFlags(fs, subargs, "load"); err != nil {
		return err
	}
	id, err := sourceArg(fs, *all, "load")
	if err != nil {
		return err
	}

	cfg, err := g.loadConfig()
	if err != nil {
		return err
	}
	var ids []string
	if id != "" {
		ids = []string{id}
	}
	run, specs, err := prepareLoad(cfg, "load", ids, stdout, stderr)
	if err != nil {
		return err
	}

	// load mutates the live gold file: gate for write, then open RW.
	db, lock, err := openGoldForWrite(g, cfg, "load", defaultMissingGoldMsg)
	if err != nil {
		return err
	}
	defer lock.unlock()
	defer db.Close()

	firstErr := run.sources(ctx, loader.New(db), specs, false)
	return errors.Join(firstErr, run.finish(ctx, db, nil))
}

// loadRun is one load, reload or rebuild: the config, what it applies
// beside the sources, the verb its lines start with and where they go.
type loadRun struct {
	verb           string
	cfg            *config.Config
	in             loadInputs
	stdout, stderr io.Writer
}

// prepareLoad reads what a load applies beside the sources and builds
// the spec of each source it covers: every configured one for ids nil,
// else the named ones in that order. It runs before gold is opened
// read-write, so a ledger or a source this build refuses fails the
// command before anything is migrated or loaded.
func prepareLoad(cfg *config.Config, verb string, ids []string, stdout, stderr io.Writer) (loadRun, []loader.SourceSpec, error) {
	in, err := readLoadInputs(cfg)
	if err != nil {
		return loadRun{}, nil, err
	}
	targets, err := loadTargets(cfg, verb, ids)
	if err != nil {
		return loadRun{}, nil, err
	}
	specs := make([]loader.SourceSpec, 0, len(targets))
	for _, s := range targets {
		specs = append(specs, buildSourceSpec(s, cfg, in.transfers))
	}
	return loadRun{verb: verb, cfg: cfg, in: in, stdout: stdout, stderr: stderr}, specs, nil
}

// loadTargets is every configured source for ids nil (-a), or the
// named sources in that order. A missing id fails before any source
// is touched.
func loadTargets(cfg *config.Config, verb string, ids []string) ([]config.SilverSource, error) {
	if ids == nil {
		if len(cfg.SilverSources) == 0 {
			return nil, fmt.Errorf("%s: -a passed but no silver sources are configured", verb)
		}
		return cfg.SilverSources, nil
	}
	out := make([]config.SilverSource, 0, len(ids))
	for _, id := range ids {
		s, ok := cfg.Lookup(id)
		if !ok {
			return nil, fmt.Errorf("%s: silver source %q not found in config", verb, id)
		}
		out = append(out, *s)
	}
	return out, nil
}

// sources loads each source into db, resetting it first when reset is
// set, and prints a line per source. A source that fails is reported
// and does not stop the next; the first failure comes back.
func (r loadRun) sources(ctx context.Context, ld *loader.Loader, specs []loader.SourceSpec, reset bool) error {
	var firstErr error
	failed := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, spec := range specs {
		if reset {
			// Reset is best-effort per source: surface failures but
			// keep going so '-a' isn't blocked by a single bad source.
			if err := ld.Reset(ctx, spec.ID); err != nil {
				fmt.Fprintf(r.stderr, "%s: %s: reset: %s\n", r.verb, spec.ID, err.Error())
				failed(err)
				continue
			}
		}
		res, err := ld.Load(ctx, spec)
		if err != nil {
			if r.verb == "load" {
				fmt.Fprintf(r.stderr, "load: %s: %s\n", spec.ID, err.Error())
			} else {
				fmt.Fprintf(r.stderr, "%s: %s: load: %s\n", r.verb, spec.ID, err.Error())
			}
			failed(err)
			continue
		}
		r.printResult(res, reset)
	}
	return firstErr
}

// printResult prints one source's line: a load's, an in-place reload's
// after its reset, or a rebuild's into a fresh file.
func (r loadRun) printResult(res *loader.LoadResult, reset bool) {
	switch {
	case r.verb == "load" && res.AlreadyUpToDate:
		fmt.Fprintf(r.stdout, "load: %s: up-to-date (watermark %d)\n", res.SourceID, res.ChangeNumberAfter)
	case r.verb == "load":
		fmt.Fprintf(r.stdout, "load: %s: %d snapshot row(s) + %d transaction(s)%s (watermark %d → %d)\n",
			res.SourceID, res.SnapshotsLoaded, res.TransactionsLoaded, realizedLotsNote(res),
			res.ChangeNumberBefore, res.ChangeNumberAfter)
	case reset && res.AlreadyUpToDate:
		// Vacuously true after a reset — silver has no
		// changes for an empty watermark, e.g. no dump_runs.
		fmt.Fprintf(r.stdout, "%s: %s: reset; nothing to load (watermark %d)\n",
			r.verb, res.SourceID, res.ChangeNumberAfter)
	case reset:
		fmt.Fprintf(r.stdout, "%s: %s: reset + %d snapshot row(s) + %d transaction(s)%s (watermark → %d)\n",
			r.verb, res.SourceID, res.SnapshotsLoaded, res.TransactionsLoaded, realizedLotsNote(res),
			res.ChangeNumberAfter)
	default:
		fmt.Fprintf(r.stdout, "%s: %s: %d snapshot row(s) + %d transaction(s)%s (watermark → %d)\n",
			r.verb, res.SourceID, res.SnapshotsLoaded, res.TransactionsLoaded, realizedLotsNote(res),
			res.ChangeNumberAfter)
	}
}

// realizedLotsNote is the realized-lot count for a load line, empty for
// a source that states none.
func realizedLotsNote(r *loader.LoadResult) string {
	if r.RealizedLotsLoaded == 0 {
		return ""
	}
	return fmt.Sprintf(" + %d realized lot(s)", r.RealizedLotsLoaded)
}

// finish ends a load once its sources are in: the config stamps, then
// carry when it is set, then the gold passes. A failed stamp or pass is
// reported and does not stop the rest; the failures come back joined.
// A failed carry stops the run and comes back alone.
func (r loadRun) finish(ctx context.Context, db *sql.DB, carry func() error) error {
	stampErr := stampConfig(ctx, db, r.cfg, r.in, r.verb, r.stdout, r.stderr)
	if carry != nil {
		if err := carry(); err != nil {
			return err
		}
	}
	return errors.Join(stampErr, runGoldPasses(ctx, db, r.cfg, r.in.enrichment, r.verb, r.stdout, r.stderr))
}

// sourceArg reads the `<silver_source_id> | -a` argument the
// per-source commands share: the one id named, or "" for -a.
func sourceArg(fs *flag.FlagSet, all bool, cmd string) (string, error) {
	switch {
	case all && fs.NArg() > 0:
		fs.Usage()
		return "", errs.Newf(errs.ExitUsage, "%s: '-a' and a positional id are mutually exclusive", cmd)
	case all:
		return "", nil
	case fs.NArg() == 1:
		return fs.Arg(0), nil
	}
	fs.Usage()
	return "", errs.Newf(errs.ExitUsage, "%s: expected one silver_source_id or -a", cmd)
}

// stampConfig writes what gold takes from the config once the sources
// are in: the FX source precedence, which the SQL FX layer (fx_daily)
// reads, the declared accounts, the holding aliases, the exposure rows
// (read, with the declarations ledger, before gold opened), the yield
// rows and the benchmarks. The precedence re-stamps every source, so a single-source load
// keeps it globally correct; failing to stamp it degrades a conversion
// and is a warning.
// A failure to sync the rows is an error (syncConfigRows).
func stampConfig(ctx context.Context, db *sql.DB, cfg *config.Config, in loadInputs, verb string,
	stdout, stderr io.Writer) error {
	if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
		fmt.Fprintf(stderr, "%s: warning: could not stamp FX priorities: %s\n", verb, err.Error())
	}
	if err := syncConfigRows(ctx, db, cfg, in, verb, stdout); err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", verb, err.Error())
		return err
	}
	warnUnmatchedDeclarations(ctx, db, verb, stderr)
	return nil
}

// warnUnmatchedDeclarations names the declarations whose holding, or
// whose account for a rate, no source states. Such a record declares
// nothing, and the holding it was meant for stays undeclared, so the
// warning is what shows a misspelt instrument or account.
func warnUnmatchedDeclarations(ctx context.Context, db *sql.DB, verb string, stderr io.Writer) {
	lines, err := gold.DeclarationsNamingNoHolding(ctx, db)
	if err != nil {
		fmt.Fprintf(stderr, "%s: warning: could not check the declarations: %s\n", verb, err.Error())
		return
	}
	if len(lines) == 0 {
		return
	}
	const shown = 10
	names := make([]string, 0, shown)
	for _, n := range lines[:min(len(lines), shown)] {
		names = append(names, strconv.Itoa(n))
	}
	if len(lines) > shown {
		names = append(names, "…")
	}
	fmt.Fprintf(stderr, "%s: warning: %d declaration(s) name a holding or an account no source states (exposure.declarations line %s)\n",
		verb, len(lines), strings.Join(names, ", "))
}

// syncConfigRows stamps the config's declared accounts into the
// accounts dimension, its holding aliases into holding_aliases, its
// exposure block into the exposure tables, its yield block into the
// yield tables and the benchmarks into theirs, after the per-source loads and before the passes that
// read them. A failure is an error rather than a warning: a rule naming
// a declared account the dimension does not hold would place its rows
// onto a far account the statement cannot resolve, which is the hole
// declared accounts exist to close.
func syncConfigRows(ctx context.Context, db *sql.DB, cfg *config.Config, in loadInputs, verb string,
	stdout io.Writer) error {
	n, err := gold.SyncDeclaredAccounts(ctx, db, cfg.DeclaredAccountChanges(time.Now().Unix()))
	if err != nil {
		return fmt.Errorf("declared accounts: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(stdout, "%s: %d declared account(s) stamped\n", verb, n)
	}
	if n, err = gold.SyncHoldingAliases(ctx, db, cfg.HoldingAliasRows()); err != nil {
		return fmt.Errorf("holding aliases: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(stdout, "%s: %d holding alias(es) stamped\n", verb, n)
	}
	if n, err = gold.SyncExposure(ctx, db, in.exposure); err != nil {
		return fmt.Errorf("exposure: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(stdout, "%s: %d exposure entr(ies) stamped\n", verb, n)
	}
	if n, err = gold.SyncYield(ctx, db, in.yield); err != nil {
		return fmt.Errorf("yield: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(stdout, "%s: %d yield entr(ies) stamped\n", verb, n)
	}
	if _, err = gold.SyncBenchmarks(ctx, db, cfg.BenchmarkDefs()); err != nil {
		return fmt.Errorf("benchmarks: %w", err)
	}
	return nil
}

// enrichmentLedgers are the files the enrichment pass applies beside
// the config: both families' pins and the transfer-override ledger.
type enrichmentLedgers struct {
	spendPins  []spending.Pin
	incomePins []spending.Pin
	overrides  []gold.TransferOverrideRule
}

// loadInputs is what a load or reload applies beside the sources, read
// before gold is opened read-write: the equity-transfer ledger by
// source, the enrichment ledgers, the exposure rows with the
// declarations ledger, and the yield rows.
type loadInputs struct {
	transfers  map[string][]loader.TransferEntry
	enrichment enrichmentLedgers
	exposure   canonical.ExposureRows
	yield      canonical.YieldRows
}

// readLoadInputs reads the loadInputs (loader.ParseTransferLedger,
// checkTransferSources, parseEnrichmentLedgers, config.ExposureRows,
// config.YieldRows).
func readLoadInputs(cfg *config.Config) (loadInputs, error) {
	transfers, err := loader.ParseTransferLedger(cfg.EquityTransfers)
	if err != nil {
		return loadInputs{}, err
	}
	if err := checkTransferSources(transfers, cfg.SilverSources); err != nil {
		return loadInputs{}, err
	}
	enrichment, err := parseEnrichmentLedgers(cfg)
	if err != nil {
		return loadInputs{}, err
	}
	exposure, err := cfg.ExposureRows()
	if err != nil {
		return loadInputs{}, err
	}
	return loadInputs{transfers: transfers, enrichment: enrichment, exposure: exposure, yield: cfg.YieldRows()}, nil
}

// checkTransferSources refuses an equity-transfer leg whose source is no
// configured source: a load applies a source's legs only, so the leg
// would book nothing, and a one-row move would book its sending half
// alone. Every configured source counts, not only the ones a load
// covers, so a load of one source still reads a ledger that names the
// others. The error names the row by its line, account and date, and
// the column the id came from.
func checkTransferSources(transfers map[string][]loader.TransferEntry, sources []config.SilverSource) error {
	known := make(map[string]bool, len(sources))
	for _, s := range sources {
		known[s.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(transfers)) {
		if known[id] {
			continue
		}
		e := transfers[id][0]
		source, account := "silver_source_id", "account"
		if e.PairSource != "" && e.Direction == "in" {
			source, account = "to_silver_source_id", "to_account"
		}
		return fmt.Errorf("equity_transfers: line %d: the row of %s %q on %s: %s %q is no configured source",
			e.Line, account, e.Account, time.Unix(e.OccurredAt, 0).UTC().Format(time.DateOnly), source, id)
	}
	return nil
}

// parseEnrichmentLedgers reads the ledgers the pass applies. Every
// command that runs the pass reads them before it opens gold
// read-write, because the open applies any outstanding migration. A
// ledger this build refuses, such as a pin naming a retired value,
// then fails the command before anything is migrated or loaded, not
// after the sources have landed.
func parseEnrichmentLedgers(cfg *config.Config) (enrichmentLedgers, error) {
	var l enrichmentLedgers
	var err error
	if l.spendPins, err = spending.ParsePinLedger(cfg.SpendPins()); err != nil {
		return enrichmentLedgers{}, err
	}
	if l.incomePins, err = spending.ParsePinLedgerAs(cfg.IncomePins(), "income",
		"income_detailed", canonical.IncomeDetailedCapitalReturn, canonical.ValidIncomeDetailed); err != nil {
		return enrichmentLedgers{}, err
	}
	if l.overrides, err = gold.ParseTransferOverrideLedger(cfg.SpendTransferOverrides()); err != nil {
		return enrichmentLedgers{}, err
	}
	return l, nil
}

// runGoldPasses runs the passes a load or reload ends with, over all of
// gold: the class pass, whose classes every later pass reads, the
// enrichment pass, the lot pass, the exposure pass, then the yield pass,
// which reads the exposure pass's keys. A pass that fails is reported
// under verb and does not stop the next; the failures come back joined.
func runGoldPasses(ctx context.Context, db *sql.DB, cfg *config.Config, enrichment enrichmentLedgers, verb string, stdout, stderr io.Writer) error {
	var failed error
	for _, pass := range []struct {
		name string
		fn   func() error
	}{
		{"class", func() error { return runClassPass(ctx, db, cfg) }},
		{"enrichment", func() error { return runEnrichmentPass(ctx, db, cfg, enrichment, stdout) }},
		{"lots", func() error { return runLotPass(ctx, db, cfg, false, stdout) }},
		{"exposure", func() error { return runExposurePass(ctx, db, stdout) }},
		{"yield", func() error { return runYieldPass(ctx, db, stdout) }},
	} {
		err := timedPass(stdout, pass.name, pass.fn)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", verb, err.Error())
			failed = errors.Join(failed, err)
		}
	}
	return failed
}

// timedPass runs one gold pass and prints its wall time. The pass's own
// lines say what it wrote; this line says how long it took, on a load,
// a reload and each web-materialize phase.
func timedPass(w io.Writer, name string, fn func() error) error {
	start := time.Now()
	err := fn()
	printTiming(w, name, start)
	return err
}

// printTiming prints the wall time since start of the step name.
func printTiming(w io.Writer, name string, start time.Time) {
	fmt.Fprintf(w, "timing: %s %s\n", name, time.Since(start).Round(time.Millisecond))
}

// runEnrichmentPass re-asserts every deterministic verdict in gold,
// the ledgers' among them, and reports what it did. The caller reads
// the ledgers first (parseEnrichmentLedgers).
//
// It runs after the per-source loop rather than per source, because
// the verdicts it reaches are not per-source facts: the withdrawal
// that funds a card payment and the card payment itself routinely
// arrive from two different sources, and neither leg can be recognised
// as an own-account move until both are in gold. One pass over the
// whole file after every source has landed is the only ordering in
// which that is always true.
//
// UNLIKE the FX-priority stamp beside it, a failure here is an error
// rather than a warning. A missing FX rank degrades a conversion; a
// missing enrichment pass leaves own-account moves counted as
// spending, which is not a degraded answer but a wrong one.
func runEnrichmentPass(ctx context.Context, db *sql.DB, cfg *config.Config, ledgers enrichmentLedgers, stdout io.Writer) error {
	include, exclude := cfg.SpendAccountScope()
	m := cfg.SpendMatching()
	incomeInclude, incomeExclude := cfg.IncomeAccountScope()
	// The pool inherits neither family's scope, so it reads its own
	// block. The include half is always nil: every account is pooled
	// until an entry takes one out.
	_, cashflowExclude := cfg.CashflowAccountScope()
	res, err := spending.RunDeterministicPass(ctx, db, spending.Options{
		Include:           include,
		Exclude:           exclude,
		MatchWindowDays:   m.Window(),
		MatchTolerancePct: m.Tolerance(),
		MatchNames:        matchNames(m),
		Rules:             compiledRules(cfg.SpendRules()),
		TransferOverrides: ledgers.overrides,
		Pins:              ledgers.spendPins,
		Income: spending.IncomeOptions{
			Include: incomeInclude,
			Exclude: incomeExclude,
			Rules:   compiledRules(cfg.IncomeRules()),
			Pins:    ledgers.incomePins,
		},
		Cashflow: spending.CashflowOptions{
			Exclude:  cashflowExclude,
			Wrappers: cfg.CashflowWrappers(),
		},
	})
	if err != nil {
		return fmt.Errorf("enrichment: %w", err)
	}
	printPassSummary(stdout, res)
	return nil
}

// printPassSummary writes everything `load` says about the enrichment
// pass: one block per family, spending first, with the override
// ledger's line under spending's.
//
// It is its own function so the SEQUENCE is testable. The pass writes
// two overlays in one transaction and this is how a reader learns
// either happened, so a test pins that both blocks print.
//
// The override ledger's line sits under spending because there is ONE
// matcher and one ledger, and both families read its verdicts — printed
// after income's block it read as a remark about the family it is not
// about.
func printPassSummary(stdout io.Writer, res *spending.Result) {
	printFamilySummary(stdout, "spending", "merchant", res.FamilyResult)
	if res.UnmatchedTransferOverrides > 0 {
		fmt.Fprintf(stdout, "spending: %d transfer override(s) matched no leg — "+
			"not loaded yet, or the ledger row describes none\n",
			res.UnmatchedTransferOverrides)
	}
	if res.IdleTransferMatches > 0 {
		fmt.Fprintf(stdout, "spending: %d transfer override `match` line(s) named both legs but paired none — "+
			"the legs read as one direction, one is zero, or another line took one\n",
			res.IdleTransferMatches)
	}
	if res.ReimbursementPairs > 0 || res.UnmatchedReimbursements > 0 || res.UnlinkedReimbursements > 0 {
		fmt.Fprintf(stdout, "spending: %d reimbursement pair(s) from the ledger, %d ledger line(s) naming none; "+
			"%d repayment(s) no road links (a `reimburse` line links one)\n",
			res.ReimbursementPairs, res.UnmatchedReimbursements, res.UnlinkedReimbursements)
	}
	printFamilySummary(stdout, "income", "payer", res.Income)
	printCashflowSummary(stdout, res.Cashflow)
}

// printCashflowSummary is the boundary's block. It is not a family
// block: cashflow enriches nothing, so there is no population, no tier
// breakdown and no backlog to print.
//
// The wrapper-coverage line is the one that has to be loud. An unset
// wrapper reads as household, which keeps the statement complete but
// puts a retirement or health account INSIDE the cash pool, where its
// own trades become household investing and its contributions become
// invisible. The crossing is then absent rather than wrong, so no
// reconciliation downstream can find it: the load saying so is the
// only place it surfaces at load time.
func printCashflowSummary(stdout io.Writer, res spending.CashflowResult) {
	// One line when the deployment is covered, and the far-account
	// count rides on it rather than taking a line of its own: the two
	// halves the resolution needs are the boundary and the far
	// accounts, and a reader checking that a load did its work should
	// find both in one place.
	// The two figures count different things and are worded to say so:
	// far accounts are enrichment ROWS, one per leg, while the matcher
	// reports PAIRS, each of which writes two of those rows. Read as
	// subsets of one another they would never add up.
	fmt.Fprintf(stdout, "cashflow: household boundary stamped — %d wrapper(s), %d overridden, "+
		"%d account(s) out of the pool; %d own-account move(s) carry a far account "+
		"(%d stated by the source itself); the matcher asserted %d pair(s) on a shared "+
		"reference and %d on a described counter leg, and paired %d a narrative named, "+
		"%d across two currencies at the day's rate and %d reversal(s) on one account\n",
		res.WrapperRows, res.WrapperOverrides, res.ScopeRows, res.FarAccounts,
		res.StatedFarAccounts, res.ReferencePairs, res.StatedCounterPairs, res.NamedPairs,
		res.ConvertedPairs, res.ReversalPairs)
	// Said only when there is something to say. A handful of refused
	// references is the ordinary shape of a bank that books a charge under
	// the reference of the payment it belongs to; a number that grows with
	// the archive says the source mints references per day or per batch, and
	// that the road should not be trusted on it.
	if res.AmbiguousReferences > 0 {
		fmt.Fprintf(stdout, "cashflow: %d source reference(s) named more than one movement "+
			"and paired nothing\n", res.AmbiguousReferences)
	}
	// Said once, on the pass that ends the state, because the state
	// itself is unreadable from the outside: with no boundary stamped
	// and no far account written, every matched own-account move
	// resolves to `vehicles · Unpaired transfers` and the dashboard
	// shows one enormous node that looks exactly like a finding.
	if res.FirstPass {
		fmt.Fprintf(stdout, "cashflow: this was the FIRST pass to stamp the boundary — "+
			"until now the statement drew every own-account move as `Unpaired transfers`, "+
			"and any cash flow report read before this run was wrong\n")
	}
	if res.PooledAccountsWithoutWrapper > 0 {
		fmt.Fprintf(stdout, "cashflow: %d pooled account(s) have no tax wrapper — each reads as the household's, "+
			"so a vehicle among them contributes no crossing at all\n",
			res.PooledAccountsWithoutWrapper)
	}
	// A declaration is unfalsifiable by anything the product measures,
	// so the load is where the count is printed, and the pooled half is
	// the one that matters: those are the accounts a move to which
	// draws nothing, on the holder's word alone.
	if res.DeclaredAccounts > 0 {
		fmt.Fprintf(stdout, "cashflow: %d declared account(s), %d inside the household pool",
			res.DeclaredAccounts, res.DeclaredPooled)
		if res.UnusedDeclarations > 0 {
			fmt.Fprintf(stdout, "; %d named by no rule-placed row", res.UnusedDeclarations)
		}
		fmt.Fprintln(stdout)
	}
	if res.UnresolvedScopeAccounts > 0 {
		fmt.Fprintf(stdout, "cashflow: %d account scope id(s) matched no account — "+
			"`cashflow.accounts` keys on the account id, and such an entry scopes nothing\n",
			res.UnresolvedScopeAccounts)
	}
}

// printFamilySummary is the per-family block `load` prints. One shape
// for both, so a reader who has learned to read one has learned the
// other, and the counters that are zero on a quiet run stay silent.
func printFamilySummary(stdout io.Writer, family, counterparty string, res spending.FamilyResult) {
	fmt.Fprintf(stdout, "%s: %d row(s) enriched — %d matcher, %d rule, %d provider, %d pinned, %d unplaced\n",
		family, res.Enriched, res.MatcherRows, res.RuleRows, res.ProviderRows,
		res.PinRows, res.SignatureOnlyRows)
	if res.UnmatchedPins > 0 {
		fmt.Fprintf(stdout, "%s: %d pin(s) matched no transaction — not loaded yet, or the ledger row describes none\n",
			family, res.UnmatchedPins)
	}
	if res.UnresolvedScopeAccounts > 0 {
		fmt.Fprintf(stdout, "%s: %d account scope id(s) matched no account — "+
			"`%s.accounts` keys on the account id, and such an entry scopes nothing\n",
			family, res.UnresolvedScopeAccounts, family)
	}
	// The backlog, not the tally: a success count rising from zero says
	// nothing about whether the node is honest, and the tiers this
	// surface cannot reach keep pushing the remainder back up. Printed
	// whenever either side is non-zero, because "0 of N stated" is the
	// reading that matters on the first run after the column lands.
	if res.StatedExposures > 0 || res.UnstatedInvesting > 0 {
		fmt.Fprintf(stdout, "%s: %d investing row(s) say what the capital went into, %d still do not\n",
			family, res.StatedExposures, res.UnstatedInvesting)
	}
	if res.UnmappedProviderCategories > 0 {
		fmt.Fprintf(stdout, "%s: %d row(s) carried a provider category this build does not map\n",
			family, res.UnmappedProviderCategories)
	}
	if res.RekeyedVerdicts > 0 {
		fmt.Fprintf(stdout, "%s: %d %s verdict(s) carried forward to signature version %d\n",
			family, res.RekeyedVerdicts, counterparty, spending.SignatureVersion)
	}
	if res.SplitVerdicts > 0 {
		fmt.Fprintf(stdout, "%s: %d %s verdict(s) left behind by the signature version %d re-key — "+
			"their rows split across several new signatures, so those %ss are re-asked on the next 'categorize'\n",
			family, res.SplitVerdicts, counterparty, spending.SignatureVersion, counterparty)
	}
}

// compiledRules translates a family's compiled config rules to the
// enrichment pass's type, the way buildSourceSpec translates the
// account overrides: config carries the JSON shape and the validation,
// spending stays config-free.
//
// It takes an already-chosen list rather than the whole config, so one
// function serves both families and neither call site reads as the
// special case.
func compiledRules(compiled []config.CompiledSpendRule) []spending.Rule {
	if len(compiled) == 0 {
		return nil
	}
	rules := make([]spending.Rule, 0, len(compiled))
	for _, r := range compiled {
		rules = append(rules, spending.Rule{
			Match: r.Match, Category: r.Category, AssetClass: r.AssetClass, Far: r.Far,
			Scope: spending.RuleScope{
				Source:    r.Scope.Source,
				Portfolio: r.Scope.Portfolio,
				Account:   r.Scope.Account,
				From:      r.Scope.From,
				To:        r.Scope.To,
			},
		})
	}
	return rules
}

// matchNames converts the config's matcher names to the pass's.
func matchNames(m *config.SpendingTransferMatching) []spending.CounterpartyName {
	if m == nil {
		return nil
	}
	out := make([]spending.CounterpartyName, 0, len(m.Names))
	for _, n := range m.Names {
		out = append(out, spending.CounterpartyName{Source: n.Source, Account: n.Account, Pattern: n.Pattern()})
	}
	return out
}

// buildSourceSpec assembles a loader.SourceSpec for one configured
// silver source: carries `path`, `subsources` and the settings its kind
// parsed (`relationships`, `schwab`) and folds in every config
// statement scoped to this source — the override families and
// `transaction_instruments`.
func buildSourceSpec(
	s config.SilverSource,
	cfg *config.Config,
	transferLedger map[string][]loader.TransferEntry,
) loader.SourceSpec {
	openSpec := s.ToSilverOpenSpec()
	spec := loader.SourceSpec{
		ID:             s.ID,
		Kind:           s.Kind,
		TaxableWrapper: s.TaxableWrapper,
		Path:           openSpec.Path,
		Subsources:     openSpec.Subsources,
		Settings:       openSpec.Settings,
		TransferLedger: transferLedger[s.ID],
	}
	if cfgOvr := cfg.AccountOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.Overrides = make(map[string]loader.AccountOverride, len(cfgOvr))
		for acctID, ov := range cfgOvr {
			spec.Overrides[acctID] = loader.AccountOverride{
				Nickname:        ov.Nickname,
				Category:        ov.Category,
				AccountKind:     ov.AccountKind,
				TaxWrapper:      ov.TaxWrapper,
				TaxWrapperFrom:  ov.TaxWrapperFromEpoch(),
				ManagementStyle: ov.ManagementStyle,
				Exclude:         ov.Exclude,
			}
		}
	}
	if cfgOvr := cfg.PortfolioOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.PortfolioOverrides = make(map[string]loader.PortfolioOverride, len(cfgOvr))
		for portfolioID, ov := range cfgOvr {
			spec.PortfolioOverrides[portfolioID] = loader.PortfolioOverride{
				TaxWrapper: ov.TaxWrapper,
				Exclude:    ov.Exclude,
			}
		}
	}
	if links := cfg.TransactionInstruments[s.ID]; len(links) > 0 {
		spec.TransactionInstruments = maps.Clone(links)
	}
	// No clone, unlike the line above: Epochs() parses into a fresh map
	// each call, so nothing else holds this one.
	if ends := cfg.Supersession.Epochs()[s.ID]; len(ends) > 0 {
		spec.Supersession = ends
	}
	return spec
}

// runClassPass sets the pairs the config pins with instrument_overrides
// and reads OpenFIGI's security types over the classes of every other
// instrument (gold.RefineClasses, silver.RefineClass). It prints
// nothing: every load writes the adapter's pairs back, so the pass
// rewrites the same instruments each time.
func runClassPass(ctx context.Context, db *sql.DB, cfg *config.Config) error {
	_, err := gold.RefineClasses(ctx, db, silver.RefineClass, instrumentPins(cfg))
	return err
}

// instrumentPins is, per source, the pair instrument_overrides pins on
// each instrument it names.
func instrumentPins(cfg *config.Config) map[string]map[string]gold.ClassPin {
	pins := map[string]map[string]gold.ClassPin{}
	for src, overrides := range cfg.InstrumentOverrides {
		pins[src] = map[string]gold.ClassPin{}
		for iid, ov := range overrides {
			pins[src][iid] = gold.ClassPin{AssetClass: canonical.AssetClass(ov.AssetClass), Vehicle: canonical.Vehicle(ov.Vehicle)}
		}
	}
	return pins
}

// runExposurePass rebuilds the exposure report's derived tables
// (gold.RebuildExposure) and says what it built, once there is reference
// data to build from.
func runExposurePass(ctx context.Context, db *sql.DB, stdout io.Writer) error {
	n, err := gold.RebuildExposure(ctx, db)
	if err != nil {
		return err
	}
	if n.Securities > 0 {
		fmt.Fprintf(stdout, "exposure: %d securities, %d aliases, %d unmapped label(s)\n",
			n.Securities, n.Aliases, n.UnmappedLabels)
	}
	return nil
}

// runYieldPass rebuilds the yield report's derived tables
// (gold.RebuildYield) and says what it built, once there is something to
// say.
func runYieldPass(ctx context.Context, db *sql.DB, stdout io.Writer) error {
	n, err := gold.RebuildYield(ctx, db)
	if err != nil {
		return err
	}
	if n.Instruments+n.RatedAccounts > 0 {
		fmt.Fprintf(stdout, "yield: %d instrument(s) with terms, %d with a maturity; %d account(s) with a stated rate\n",
			n.Instruments, n.WithMaturity, n.RatedAccounts)
	}
	return nil
}
