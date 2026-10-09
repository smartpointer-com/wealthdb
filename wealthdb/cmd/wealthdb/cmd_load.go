package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
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
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "load: bad flags")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	ledger, err := loader.ParseTransferLedger(cfg.EquityTransfers)
	if err != nil {
		return err
	}
	// Before gold is opened read-write (parseEnrichmentLedgers).
	enrichment, err := parseEnrichmentLedgers(cfg)
	if err != nil {
		return err
	}

	// Resolve which sources to load.
	var specs []loader.SourceSpec
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "load: '-a' and a positional id are mutually exclusive")
	case *all:
		for _, s := range cfg.SilverSources {
			spec, err := buildSourceSpec(s, cfg, ledger)
			if err != nil {
				return err
			}
			specs = append(specs, spec)
		}
		if len(specs) == 0 {
			return fmt.Errorf("load: -a passed but no silver sources are configured")
		}
	case fs.NArg() == 1:
		id := fs.Arg(0)
		s, ok := cfg.Lookup(id)
		if !ok {
			return fmt.Errorf("load: silver source %q not found in config", id)
		}
		spec, err := buildSourceSpec(*s, cfg, ledger)
		if err != nil {
			return err
		}
		specs = []loader.SourceSpec{spec}
	default:
		fs.Usage()
		return errs.Newf(2, "load: expected one silver_source_id or -a")
	}

	// load mutates the live gold file: gate for write, then open RW.
	db, lock, err := openGoldForWrite(g, cfg, "load",
		"gold database %q does not exist. Run 'wealthdb init' first (requires write access).")
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
	// Persist the config-driven FX source precedence into gold so the
	// SQL FX layer (fx_daily) honours it. Re-stamps all sources, so a
	// single-source load keeps the column globally correct.
	if err := gold.SetFxPriorities(ctx, db, cfg.FxSourceOrder()); err != nil {
		fmt.Fprintf(stderr, "load: warning: could not stamp FX priorities: %s\n", err.Error())
	}
	if err := syncDeclaredAccounts(ctx, db, cfg, "load", stdout); err != nil {
		fmt.Fprintf(stderr, "load: %s\n", err.Error())
		firstErr = errors.Join(firstErr, err)
	}
	if err := runEnrichmentPass(ctx, db, cfg, enrichment, stdout); err != nil {
		fmt.Fprintf(stderr, "load: %s\n", err.Error())
		firstErr = errors.Join(firstErr, err)
	}
	return firstErr
}

// syncDeclaredAccounts stamps the config's declared accounts into the
// accounts dimension, after the per-source loads and before the
// enrichment pass that reads them. It is an error rather than a
// warning, like the pass itself: a rule naming a declaration the
// dimension does not hold would place its rows onto a far account the
// statement cannot resolve, which is the hole the declarations exist
// to close.
func syncDeclaredAccounts(ctx context.Context, db *sql.DB, cfg *config.Config, verb string, stdout io.Writer) error {
	n, err := gold.SyncDeclaredAccounts(ctx, db, cfg.DeclaredAccountChanges(time.Now().Unix()))
	if err != nil {
		return fmt.Errorf("declared accounts: %w", err)
	}
	if n > 0 {
		fmt.Fprintf(stdout, "%s: %d declared account(s) stamped\n", verb, n)
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
// either happened; with the two calls inline, dropping the second left
// the whole suite green.
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
		"reference and %d on a described counter leg, and paired %d a narrative named\n",
		res.WrapperRows, res.WrapperOverrides, res.ScopeRows, res.FarAccounts,
		res.StatedFarAccounts, res.ReferencePairs, res.StatedCounterPairs, res.NamedPairs)
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
// silver source: translates `path` / `subsources` / `relationships`
// to their silver-package counterparts and folds in every config
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
// a source that states none, so the line reads as it always has there.
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
