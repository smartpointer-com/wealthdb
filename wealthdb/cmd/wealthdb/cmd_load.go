package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
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
for all configured sources (-a). See docs/DESIGN.md §8 for the
load semantics.`)
	}
	if err := parseFlags(fs, subargs, "load"); err != nil {
		return err
	}
	id, err := sourceArg(fs, *all, "load")
	if err != nil {
		return err
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	ledger, err := loader.ParseTransferLedger(cfg.EquityTransfers)
	if err != nil {
		return err
	}
	in, err := readLoadInputs(cfg)
	if err != nil {
		return err
	}
	targets, err := selectSources(cfg, id, "load")
	if err != nil {
		return err
	}
	specs := make([]loader.SourceSpec, 0, len(targets))
	for _, s := range targets {
		spec, err := buildSourceSpec(s, cfg, ledger)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}

	// load mutates the live gold file: gate for write, then open RW.
	db, lock, err := openGoldForWrite(g, cfg, "load", defaultMissingGoldMsg)
	if err != nil {
		return err
	}
	defer lock.unlock()
	defer db.Close()

	ld := loader.New(db)
	var firstErr error
	for _, spec := range specs {
		res, err := ld.Load(ctx, spec)
		if err != nil {
			fmt.Fprintf(stderr, "load: %s: %s\n", spec.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		printLoadResult(stdout, res)
	}
	firstErr = errors.Join(firstErr, stampConfig(ctx, db, cfg, in, "load", stdout, stderr))
	return errors.Join(firstErr, runGoldPasses(ctx, db, cfg, in.enrichment, "load", stdout, stderr))
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

// selectSources is the configured sources a load or reload covers:
// every one for id "" (-a), else the one id names.
func selectSources(cfg *config.Config, id, cmd string) ([]config.SilverSource, error) {
	if id == "" {
		if len(cfg.SilverSources) == 0 {
			return nil, fmt.Errorf("%s: -a passed but no silver sources are configured", cmd)
		}
		return cfg.SilverSources, nil
	}
	s, ok := cfg.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("%s: silver source %q not found in config", cmd, id)
	}
	return []config.SilverSource{*s}, nil
}

// stampConfig writes what gold takes from the config once the sources
// are in: the FX source precedence, which the SQL FX layer (fx_daily)
// reads, the declared accounts, the holding aliases, the exposure rows
// (read, with the declarations ledger, before gold opened) and the yield
// rows. The precedence re-stamps every source, so a single-source load
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
// exposure block into the exposure tables and its yield block into the
// yield tables, after the per-source loads and before the passes that
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
// before gold is opened read-write: the enrichment ledgers, the exposure
// rows with the declarations ledger, and the yield rows.
type loadInputs struct {
	enrichment enrichmentLedgers
	exposure   canonical.ExposureRows
	yield      canonical.YieldRows
}

// readLoadInputs reads the loadInputs (parseEnrichmentLedgers,
// config.ExposureRows, config.YieldRows).
func readLoadInputs(cfg *config.Config) (loadInputs, error) {
	enrichment, err := parseEnrichmentLedgers(cfg)
	if err != nil {
		return loadInputs{}, err
	}
	exposure, err := cfg.ExposureRows()
	if err != nil {
		return loadInputs{}, err
	}
	return loadInputs{enrichment: enrichment, exposure: exposure, yield: cfg.YieldRows()}, nil
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
	if res.ReimbursementPairs > 0 || res.UnmatchedReimbursements > 0 || res.UnlinkedReimbursements > 0 {
		fmt.Fprintf(stdout, "spending: %d reimbursement pair(s) from the ledger, %d ledger line(s) naming none; "+
			"%d repayment(s) linked to no outflow (a `reimburse` line links one)\n",
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
		"reference and %d on a described counter leg, and paired %d a narrative named "+
		"and %d reversal(s) on one account\n",
		res.WrapperRows, res.WrapperOverrides, res.ScopeRows, res.FarAccounts,
		res.StatedFarAccounts, res.ReferencePairs, res.StatedCounterPairs, res.NamedPairs,
		res.ReversalPairs)
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
// silver source: translates `path` / `subsources` / `relationships` /
// `schwab` to their silver-package counterparts and folds in every config
// statement scoped to this source — the override families and
// `transaction_instruments`. Returns an error when the config can't
// be translated (e.g. invalid psn_start_override date).
func buildSourceSpec(
	s config.SilverSource,
	cfg *config.Config,
	transferLedger map[string][]loader.TransferEntry,
) (loader.SourceSpec, error) {
	openSpec, err := s.ToSilverOpenSpec()
	if err != nil {
		return loader.SourceSpec{}, err
	}
	spec := loader.SourceSpec{
		ID:             s.ID,
		Kind:           s.Kind,
		TaxableWrapper: s.TaxableWrapper,
		Path:           openSpec.Path,
		Subsources:     openSpec.Subsources,
		Relationships:  openSpec.Relationships,
		MarkGuard:      openSpec.MarkGuard,
		TransferLedger: transferLedger[s.ID],
	}
	if cfgOvr := cfg.AccountOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.Overrides = make(map[string]loader.AccountOverride, len(cfgOvr))
		for acctID, ov := range cfgOvr {
			spec.Overrides[acctID] = loader.AccountOverride{
				Nickname:        ov.Nickname,
				Category:        ov.Category,
				TaxWrapper:      ov.TaxWrapper,
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
	if cfgOvr := cfg.InstrumentOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.InstrumentOverrides = make(map[string]loader.InstrumentOverride, len(cfgOvr))
		for instrID, ov := range cfgOvr {
			spec.InstrumentOverrides[instrID] = loader.InstrumentOverride{
				AssetClass: ov.AssetClass,
				Vehicle:    ov.Vehicle,
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
	return spec, nil
}

// realizedLotsNote is the realized-lot count for a load line, empty for
// a source that states none.
func realizedLotsNote(r *loader.LoadResult) string {
	if r.RealizedLotsLoaded == 0 {
		return ""
	}
	return fmt.Sprintf(" + %d realized lot(s)", r.RealizedLotsLoaded)
}

func printLoadResult(w io.Writer, r *loader.LoadResult) {
	switch {
	case r.AlreadyUpToDate:
		fmt.Fprintf(w, "load: %s: up-to-date (watermark %d)\n", r.SourceID, r.ChangeNumberAfter)
	default:
		fmt.Fprintf(w, "load: %s: %d snapshot row(s) + %d transaction(s)%s (watermark %d → %d)\n",
			r.SourceID, r.SnapshotsLoaded, r.TransactionsLoaded, realizedLotsNote(r),
			r.ChangeNumberBefore, r.ChangeNumberAfter)
	}
}

// runClassPass reads OpenFIGI's security types over the classes of the
// instruments whose adapter takes them (gold.RefineClasses). The
// instruments instrument_overrides pins keep their pair. It prints
// nothing: every load writes the adapter's pairs back, so the pass
// rewrites the same instruments each time.
func runClassPass(ctx context.Context, db *sql.DB, cfg *config.Config) error {
	_, err := gold.RefineClasses(ctx, db, silver.ClassRefiners(), pinnedInstruments(cfg))
	return err
}

// pinnedInstruments names, per source, the instruments the config pins
// with instrument_overrides.
func pinnedInstruments(cfg *config.Config) map[string]map[string]bool {
	pinned := map[string]map[string]bool{}
	for src, overrides := range cfg.InstrumentOverrides {
		pinned[src] = map[string]bool{}
		for iid := range overrides {
			pinned[src][iid] = true
		}
	}
	return pinned
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
