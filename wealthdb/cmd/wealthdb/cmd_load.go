package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/loader"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
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

	// Resolve which sources to load.
	var specs []loader.SourceSpec
	switch {
	case *all && fs.NArg() > 0:
		fs.Usage()
		return errs.Newf(2, "load: '-a' and a positional id are mutually exclusive")
	case *all:
		for _, s := range cfg.SilverSources {
			spec, err := buildSourceSpec(s, cfg.AccountOverrides, cfg.PortfolioOverrides, cfg.InstrumentOverrides, ledger)
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
		spec, err := buildSourceSpec(*s, cfg.AccountOverrides, cfg.PortfolioOverrides, cfg.InstrumentOverrides, ledger)
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
	if err := runSpendingPass(ctx, db, cfg, stdout); err != nil {
		fmt.Fprintf(stderr, "load: %s\n", err.Error())
		firstErr = errors.Join(firstErr, err)
	}
	return firstErr
}

// runSpendingPass re-asserts every deterministic spend verdict in gold
// — the pins ledger included, re-read from config on every call — and
// reports what it did.
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
func runSpendingPass(ctx context.Context, db *sql.DB, cfg *config.Config, stdout io.Writer) error {
	include, exclude := cfg.SpendAccountScope()
	m := cfg.SpendMatching()
	pins, err := spending.ParsePinLedger(cfg.SpendPins())
	if err != nil {
		return err
	}
	res, err := spending.RunDeterministicPass(ctx, db, spending.Options{
		Include:           include,
		Exclude:           exclude,
		MatchWindowDays:   m.Window(),
		MatchTolerancePct: m.Tolerance(),
		Rules:             spendRules(cfg),
		Pins:              pins,
	})
	if err != nil {
		return fmt.Errorf("spending enrichment: %w", err)
	}
	fmt.Fprintf(stdout, "spending: %d row(s) enriched — %d matcher, %d rule, %d provider, %d pinned, %d unplaced\n",
		res.Enriched, res.MatcherRows, res.RuleRows, res.ProviderRows, res.PinRows, res.SignatureOnlyRows)
	if res.UnmatchedPins > 0 {
		fmt.Fprintf(stdout, "spending: %d pin(s) matched no transaction — not loaded yet, or the ledger row describes none\n",
			res.UnmatchedPins)
	}
	if res.UnresolvedScopeAccounts > 0 {
		fmt.Fprintf(stdout, "spending: %d account scope id(s) matched no account — "+
			"`spending.accounts` keys on the account id, and such an entry scopes nothing\n",
			res.UnresolvedScopeAccounts)
	}
	if res.UnmappedProviderCategories > 0 {
		fmt.Fprintf(stdout, "spending: %d row(s) carried a provider category this build does not map\n",
			res.UnmappedProviderCategories)
	}
	if res.RekeyedMerchants > 0 {
		fmt.Fprintf(stdout, "spending: %d merchant verdict(s) carried forward to signature version %d\n",
			res.RekeyedMerchants, spending.SignatureVersion)
	}
	if res.SplitMerchants > 0 {
		fmt.Fprintf(stdout, "spending: %d merchant verdict(s) left behind by the signature version %d re-key — "+
			"their rows split across several new signatures, so those merchants are re-asked on the next 'categorize'\n",
			res.SplitMerchants, spending.SignatureVersion)
	}
	return nil
}

// spendRules translates the config's compiled `spending.rules` to the
// enrichment pass's type, the way buildSourceSpec translates the
// account overrides: config carries the JSON shape and the validation,
// spending stays config-free.
func spendRules(cfg *config.Config) []spending.Rule {
	compiled := cfg.SpendRules()
	if len(compiled) == 0 {
		return nil
	}
	rules := make([]spending.Rule, 0, len(compiled))
	for _, r := range compiled {
		rules = append(rules, spending.Rule{Match: r.Match, Category: r.Category})
	}
	return rules
}

// buildSourceSpec assembles a loader.SourceSpec for one configured
// silver source: translates `path` / `subsources` / `relationships`
// to their silver-package counterparts and folds in the per-source
// account_overrides + portfolio_overrides + instrument_overrides
// slices. Returns an error when the config can't be translated
// (e.g. invalid psn_start_override date).
func buildSourceSpec(
	s config.SilverSource,
	accountOverrides map[string]map[string]config.AccountOverride,
	portfolioOverrides map[string]map[string]config.PortfolioOverride,
	instrumentOverrides map[string]map[string]config.InstrumentOverride,
	transferLedger map[string][]loader.TransferEntry,
) (loader.SourceSpec, error) {
	openSpec, err := s.ToSilverOpenSpec()
	if err != nil {
		return loader.SourceSpec{}, err
	}
	spec := loader.SourceSpec{
		ID:             s.ID,
		Kind:           s.Kind,
		Path:           openSpec.Path,
		Subsources:     openSpec.Subsources,
		Relationships:  openSpec.Relationships,
		TransferLedger: transferLedger[s.ID],
	}
	if cfgOvr := accountOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.Overrides = make(map[string]loader.AccountOverride, len(cfgOvr))
		for acctID, ov := range cfgOvr {
			spec.Overrides[acctID] = loader.AccountOverride{
				Nickname:        ov.Nickname,
				Category:        ov.Category,
				TaxWrapper:      ov.TaxWrapper,
				ManagementStyle: ov.ManagementStyle,
			}
		}
	}
	if cfgOvr := portfolioOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.PortfolioOverrides = make(map[string]loader.PortfolioOverride, len(cfgOvr))
		for portfolioID, ov := range cfgOvr {
			spec.PortfolioOverrides[portfolioID] = loader.PortfolioOverride{
				TaxWrapper: ov.TaxWrapper,
			}
		}
	}
	if cfgOvr := instrumentOverrides[s.ID]; len(cfgOvr) > 0 {
		spec.InstrumentOverrides = make(map[string]loader.InstrumentOverride, len(cfgOvr))
		for instrID, ov := range cfgOvr {
			spec.InstrumentOverrides[instrID] = loader.InstrumentOverride{
				AssetClass: ov.AssetClass,
				Vehicle:    ov.Vehicle,
			}
		}
	}
	return spec, nil
}

func printLoadResult(w io.Writer, r *loader.LoadResult) {
	switch {
	case r.AlreadyUpToDate:
		fmt.Fprintf(w, "load: %s: up-to-date (watermark %d)\n", r.SourceID, r.ChangeNumberAfter)
	default:
		fmt.Fprintf(w, "load: %s: %d snapshot row(s) + %d transaction(s) (watermark %d → %d)\n",
			r.SourceID, r.SnapshotsLoaded, r.TransactionsLoaded,
			r.ChangeNumberBefore, r.ChangeNumberAfter)
	}
}
