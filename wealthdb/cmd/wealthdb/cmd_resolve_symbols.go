package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
)

func init() {
	register("resolve-symbols", cmdResolveSymbols)
}

// defaultResolveBatch is how many candidates one model call carries
// (see splitBatches). A symbol answer is a short row, and each batch
// sends only its own sources' anchors, so a local model answers a
// batch this size well inside the per-call ceiling.
const defaultResolveBatch = 100

// candidate is one row presented to the LLM for resolution.
// LookupKind / LookupValue together identify the row in
// symbol_resolutions's PK space. HintName is the instrument label
// the model sees as context for instrument_external_id-keyed rows
// (where the lookup_value itself is opaque like an ISIN); for
// name-keyed rows the lookup_value IS the descriptive label and
// HintName is empty. Currency biases ticker selection between US,
// European, and Asian listings.
type candidate struct {
	SilverSourceID string
	LookupKind     string // "instrument_external_id" or "name"
	LookupValue    string
	HintName       string
	Currency       string
}

// anchor is one already-resolved (silver_source_id, ..., symbol)
// tuple drawn from the existing instruments rows. Passed to
// the LLM as in-context examples of correct mappings so it doesn't
// have to guess at the broker-specific ticker conventions in the
// portfolio. Always instrument_external_id-keyed (anchors
// always have a real instrument row).
type anchor struct {
	SilverSourceID       string
	InstrumentExternalID string
	Name                 string
	Symbol               string
	Currency             string
}

// resolution is one validated (lookup → symbol) mapping, ready to
// upsert into symbol_resolutions.
type resolution struct {
	SilverSourceID string
	LookupKind     string
	LookupValue    string
	Symbol         string
}

// tickerShapeRe is the strict ticker-shape pattern: 1-12 chars of
// uppercase ASCII letters, digits, dots, or hyphens. Captures the
// surface forms brokers emit (BRK.B,
// XDEW, TFLO, EUNH.DE, etc.) and rejects obvious garbage like
// "BANK INT 07/30".
var tickerShapeRe = regexp.MustCompile(`^[A-Z0-9.\-]{1,12}$`)

// isinShapeRe matches the ISO 6166 ISIN surface form: two-letter
// country code + nine alphanumerics + one Luhn check digit, total
// 12 chars. Used to reject the "model echoed the lookup_value"
// degenerate case where the LLM gives up and emits the ISIN
// itself as the ticker (e.g. US0000000030 → US0000000030). Real
// tickers don't look like this.
var isinShapeRe = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{9}[0-9]$`)

// cusipShapeRe matches the nine plain alphanumerics of a CUSIP. A
// narrative that prints one ("DIVIDEND RECEIVED 000000AA0") invites the
// model to copy it out in place of a ticker, and no listed ticker has
// that shape.
var cusipShapeRe = regexp.MustCompile(`^[A-Z0-9]{9}$`)

// cmdResolveSymbols collects rows from gold that the silver
// adapters couldn't ticker-resolve, calls the configured LLM
// endpoint with a CSV-shaped prompt, validates the response, and
// upserts the verified resolutions into symbol_resolutions. The
// read path in gold/positions.go and gold/transactions.go picks
// the new tickers up via LEFT JOIN + COALESCE.
func cmdResolveSymbols(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb resolve-symbols", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("n", false, "show resolution plan without writing to gold")
	fs.BoolVar(dryRun, "dry-run", false, "show resolution plan without writing to gold")
	maxAttempts := fs.Int("max-attempts", 3, "max LLM round-trips when responses include hallucinated rows")
	noCurrency := fs.Bool("no-currency", false, "drop the currency hint from the prompt (experiment / ablation)")
	maxAnchors := fs.Int("max-anchors", 30, "max anchor examples per silver source to include in the prompt")
	batch := fs.Int("batch", defaultResolveBatch, "candidates per model call")
	showPrompt := fs.Bool("show-prompt", false, "print the LLM prompt to stderr before sending (debugging)")
	overridesOnly := fs.Bool("overrides-only", false, "apply cfg.symbol_resolution.overrides and exit; skip the LLM round-trip entirely")
	fs.Usage = func() {
		fmt.Fprintln(stderr, resolveSymbolsUsage())
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "resolve-symbols: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "resolve-symbols: unexpected positional argument %q", fs.Arg(0))
	}
	if *batch < 1 {
		fs.Usage()
		return errs.Newf(2, "resolve-symbols: --batch must be at least 1, got %d", *batch)
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	// LLM config is only needed when we'll actually call the LLM.
	// --overrides-only is a fast cfg→DB sync path with no model
	// dependency.
	const modelKey = "symbol_resolution.model"
	var modelCfg *config.ModelConfig
	if cfg.SymbolResolution != nil {
		modelCfg = cfg.SymbolResolution.Model
	}
	if !*overridesOnly {
		if modelCfg == nil {
			return errs.Newf(2, "resolve-symbols: %s is not set; add a `%s` block to %s (or use --overrides-only)",
				modelKey, modelKey, g.ConfigPath)
		}
		if err := validateModelConfig(modelKey, modelCfg); err != nil {
			return errs.Newf(2, "resolve-symbols: %s", err.Error())
		}
	}

	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first (requires write access).", cfg.GoldDB)
	}
	if !*dryRun && dec.Mode != pathmode.ModeReadWrite {
		return errs.Newf(errs.ExitRWNeeded,
			"'resolve-symbols' requires write access to the gold database, but '%s' is read-only (detected: %s). "+
				"Pass --dry-run if you only want to see the plan.", cfg.GoldDB, dec.Reason)
	}
	// Dry-run takes no write locks so a parallel `wealthdb
	// transactions` / `positions` call can read the DB while the
	// LLM is responding. --overrides-only writes (the sync step),
	// so it always opens RW.
	openMode := gold.ModeReadWrite
	if *dryRun && !*overridesOnly {
		openMode = gold.ModeReadOnly
	}
	if openMode == gold.ModeReadWrite {
		// Every write path takes the gold write mutex for the whole
		// command, so a rebuild-and-swap cannot land between the
		// override sync and the resolutions this run persists.
		lock, err := lockGoldForWrite(cfg.GoldDB, "resolve-symbols")
		if err != nil {
			return err
		}
		defer lock.unlock()
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	// Released before the model round-trips (see below) and re-taken
	// to persist. dbOpen keeps the deferred close correct on the
	// paths that return early.
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	configuredSources := make(map[string]bool, len(cfg.SilverSources))
	for _, s := range cfg.SilverSources {
		configuredSources[s.ID] = true
	}

	// Always sync cfg.symbol_resolution.overrides first — manual
	// overrides are the source of truth and must win over any
	// LLM-derived row for the same key, whether we're about to run
	// the LLM or just doing --overrides-only.
	var overrides []config.SymbolOverride
	if cfg.SymbolResolution != nil {
		overrides = cfg.SymbolResolution.Overrides
	}
	purged, upserted, suppressed, err := syncSymbolOverrides(ctx, db, overrides)
	if err != nil {
		return fmt.Errorf("resolve-symbols: sync overrides: %w", err)
	}
	if purged+upserted+suppressed > 0 {
		fmt.Fprintf(stdout, "resolve-symbols: synced cfg overrides — %d upserted, %d suppressed (delete:true), %d stale manual-override rows purged\n",
			upserted, suppressed, purged)
	}

	if *overridesOnly {
		var total int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_resolutions`).Scan(&total); err == nil {
			fmt.Fprintf(stdout, "resolve-symbols: overrides-only mode; total symbol_resolutions rows now %d\n", total)
		}
		return nil
	}

	candidates, err := collectCandidates(ctx, db)
	if err != nil {
		return err
	}
	candidates = withoutOverridden(candidates, overrides)
	if len(candidates) == 0 {
		fmt.Fprintln(stdout, "resolve-symbols: nothing to resolve")
		return nil
	}
	anchors, err := collectAnchors(ctx, db, *maxAnchors)
	if err != nil {
		return err
	}

	stats := summariseStats(candidates)
	batches := splitBatches(candidates, *batch)

	fmt.Fprintf(stdout, "resolve-symbols: %d candidates (%s; by-kind %s), %d anchors, %d batch(es) of up to %d, model %s\n",
		stats.Total, formatPerSource(stats.PerSource),
		formatPerKind(stats.PerKind), len(anchors), len(batches), *batch, modelCfg.Name)

	// Everything the run reads is read; release the handle before the
	// model round-trips. DuckDB is one read-write handle OR many
	// read-only ones, so holding it across the whole LLM pass would
	// lock every reader out of gold for its duration. The write mutex
	// above still excludes other writers.
	if err := db.Close(); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("resolve-symbols: close gold before the model pass: %w", err))
	}
	dbOpen = false

	// Each batch is stored as it completes, so a later batch failing —
	// a wedged serve, the per-call ceiling — never costs the answers
	// already paid for.
	var valid []resolution
	var attempts, totalInvalid int
	stored := map[string]int{}
	persisted, total := 0, 0
	for i, b := range batches {
		got, n, invalid, err := resolveWithLLM(ctx, modelCfg, b, anchorsFor(anchors, b),
			candidateKeyset(b), configuredSources, *maxAttempts, *noCurrency, *showPrompt, stdout, stderr)
		attempts += n
		totalInvalid += invalid
		valid = append(valid, got...)
		if !*dryRun && len(got) > 0 {
			perSource, t, serr := storeResolutions(ctx, cfg.GoldDB, got, modelCfg.Name, stdout)
			if serr != nil {
				return serr
			}
			for src, c := range perSource {
				stored[src] += c
			}
			persisted += len(got)
			total = t
		}
		if err != nil {
			if !*dryRun {
				fmt.Fprintf(stdout, "resolve-symbols: stopped in batch %d of %d; the %d resolution(s) answered so far are stored\n",
					i+1, len(batches), persisted)
			}
			return err
		}
		fmt.Fprintf(stdout, "resolve-symbols: batch %d/%d: %d of %d resolved\n", i+1, len(batches), len(got), len(b))
	}

	// Sort for stable output.
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].SilverSourceID != valid[j].SilverSourceID {
			return valid[i].SilverSourceID < valid[j].SilverSourceID
		}
		if valid[i].LookupKind != valid[j].LookupKind {
			return valid[i].LookupKind < valid[j].LookupKind
		}
		return valid[i].LookupValue < valid[j].LookupValue
	})

	unresolved := unresolvedCandidates(candidates, valid)
	printSummary(stdout, stats, valid, unresolved, attempts, totalInvalid)

	if *dryRun {
		printResolutionPlan(stdout, "--- dry-run plan (no rows written) ---", valid)
		return nil
	}

	if len(valid) == 0 {
		fmt.Fprintln(stdout, "resolve-symbols: no valid resolutions to persist")
		return nil
	}
	fmt.Fprintln(stdout, "resolve-symbols: persisted to gold:")
	for _, s := range sortedKeys(stored) {
		fmt.Fprintf(stdout, "  %s: %d rows upserted\n", s, stored[s])
	}
	fmt.Fprintf(stdout, "resolve-symbols: total symbol_resolutions rows in gold now %d\n", total)
	return nil
}

// withoutOverridden drops the candidates whose key a config override
// decides, as a pinned symbol or as a suppression. That word is final:
// the model's answer is stored after the overrides are synced, so asking
// about such a key would overwrite the pin or undo the suppression.
func withoutOverridden(cands []candidate, overrides []config.SymbolOverride) []candidate {
	decided := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		decided[candKey(o.SilverSourceID, o.LookupKind, o.LookupValue)] = true
	}
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if !decided[candKey(c.SilverSourceID, c.LookupKind, c.LookupValue)] {
			out = append(out, c)
		}
	}
	return out
}

// anchorsFor keeps the anchors of the silver sources a batch asks
// about. Anchors teach the model a broker's own ticker conventions, so
// another broker's are no help to a batch, only prompt it pays for.
func anchorsFor(anchors []anchor, batch []candidate) []anchor {
	sources := map[string]bool{}
	for _, c := range batch {
		sources[c.SilverSourceID] = true
	}
	var out []anchor
	for _, a := range anchors {
		if sources[a.SilverSourceID] {
			out = append(out, a)
		}
	}
	return out
}

// storeResolutions re-takes the gold handle and upserts what the model
// answered for one batch. It returns the rows upserted per source and
// the table's row count after the upsert.
//
// The re-open is retried on the bounded backoff, because it can lose a
// race it did not have to run before the handle was released for the
// length of the model pass: DuckDB refuses a read-write attach while
// any other handle is open on the file, and a single concurrent read
// command is enough. These rows have already been paid for.
//
// When even the retries fail, the rows are printed in the plan format
// before the error is returned: text on stdout is the last place a
// paid-for answer can survive a persist that cannot happen, and it is
// the same shape --dry-run prints, so it can be re-applied.
//
// The re-open does not stamp binary_versions: this is the same command
// continuing, and its first open already recorded which binary wrote
// the file.
func storeResolutions(ctx context.Context, goldPath string, valid []resolution, modelName string, stdout io.Writer) (map[string]int, int, error) {
	var perSource map[string]int
	var total int
	err := retryFlush(ctx, func() error {
		wdb, err := gold.ReopenReadWrite(goldPath)
		if err != nil {
			return err
		}
		defer wdb.Close()
		ps, n, err := persistResolutions(ctx, wdb, valid, time.Now().Unix(), modelName)
		if err != nil {
			return err
		}
		perSource, total = ps, n
		return nil
	})
	if err != nil {
		printResolutionPlan(stdout,
			fmt.Sprintf("--- %d resolution(s) the model answered but gold would not accept ---", len(valid)),
			valid)
		return nil, 0, errs.Wrap(errs.ExitOpenFailed,
			fmt.Errorf("resolve-symbols: could not store the resolutions above; re-run once nothing else "+
				"holds %q open, or carry them into cfg.symbol_resolution.overrides: %w", goldPath, err))
	}
	return perSource, total, nil
}

// printResolutionPlan renders resolutions one per line under a
// heading. One renderer for both the dry-run plan and the
// could-not-store fallback, so what a failed persist leaves on stdout
// is exactly what a plan looks like.
func printResolutionPlan(w io.Writer, heading string, rows []resolution) {
	fmt.Fprintln(w, heading)
	for _, r := range rows {
		fmt.Fprintf(w, "  %s [%s] %s → %s\n", r.SilverSourceID, r.LookupKind, r.LookupValue, r.Symbol)
	}
}

// collectCandidates emits two disjoint streams of unresolved rows:
//
//  1. by-id: every instruments row with name set but symbol NULL,
//     keyed by instrument_external_id (typically an ISIN for UBS or
//     CUSIP for Schwab). The LLM gets the name as a hint.
//
//  2. by-name: every transaction with no instrument_external_id but
//     a non-null description (Schwab DIVIDEND_OR_INTEREST payloads
//     where transferItems only has the cash leg; UBS PSN cash_movement
//     rows that didn't extract an ISIN). Deduped by description.
//
// Currency tags both — Schwab tickets are mostly USD, European ETFs
// USD/EUR/CHF; the model uses this to bias listing choice.
func collectCandidates(ctx context.Context, db *sql.DB) ([]candidate, error) {
	var out []candidate

	const byIDQ = `
SELECT silver_source_id, instrument_external_id, name, currency
  FROM instruments
 WHERE symbol IS NULL
   AND name   IS NOT NULL
 ORDER BY silver_source_id, instrument_external_id`
	rows, err := db.QueryContext(ctx, byIDQ)
	if err != nil {
		return nil, fmt.Errorf("collectCandidates (by-id): %w", err)
	}
	for rows.Next() {
		var c candidate
		var hint, ccy sql.NullString
		if err := rows.Scan(&c.SilverSourceID, &c.LookupValue, &hint, &ccy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan by-id candidate: %w", err)
		}
		c.LookupKind = "instrument_external_id"
		c.HintName = hint.String
		c.Currency = ccy.String
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// by-name: transactions with no instrument link but a
	// description. DISTINCT (silver_source_id, description) so we
	// query once per unique label rather than once per dividend
	// payment. The currency MIN keeps the most common one when a
	// description spans accounts/currencies (very rare in practice).
	//
	// Filter on instrument-related kinds only. Cash-only kinds
	// (deposit, withdrawal, fee, tax, fx_*, journal, other) don't
	// name a security — emitting them just bloats the prompt and
	// increases hallucination surface area. The LLM would skip
	// them anyway per the prompt instructions, but filtering at
	// SQL is cheaper and more honest. NB: Schwab's adapter buckets
	// cash-interest as 'dividend' (known gap), so 'dividend' here
	// still includes some non-security rows — we lean on the LLM
	// to skip those.
	const byNameQ = `
SELECT silver_source_id, description, MIN(currency) AS currency
  FROM transactions
 WHERE instrument_external_id IS NULL
   AND description IS NOT NULL
   AND kind IN ('dividend', 'interest', 'coupon', 'capital_gain',
                'buy', 'sell', 'corporate_action',
                'transfer_in', 'transfer_out')
 GROUP BY silver_source_id, description
 ORDER BY silver_source_id, description`
	rows, err = db.QueryContext(ctx, byNameQ)
	if err != nil {
		return nil, fmt.Errorf("collectCandidates (by-name): %w", err)
	}
	for rows.Next() {
		var c candidate
		var ccy sql.NullString
		if err := rows.Scan(&c.SilverSourceID, &c.LookupValue, &ccy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan by-name candidate: %w", err)
		}
		c.LookupKind = "name"
		c.Currency = ccy.String
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// collectAnchors samples up to maxAnchors already-resolved
// instruments per silver source, earliest first so the choice is
// deterministic. Used as in-context examples in the prompt so the
// model learns the broker-specific ticker conventions in this
// portfolio (UBS Xetra tickers, Schwab US tickers, Swissquote SIX
// tickers) rather than guessing globally. Only an instrument whose
// symbol has a ticker's shape (symbolProblem) can serve as one.
func collectAnchors(ctx context.Context, db *sql.DB, maxAnchors int) ([]anchor, error) {
	if maxAnchors <= 0 {
		return nil, nil
	}
	const q = `
SELECT silver_source_id, instrument_external_id, name, symbol, currency
  FROM instruments
 WHERE symbol IS NOT NULL
   AND name   IS NOT NULL
 ORDER BY silver_source_id, first_seen_at, instrument_external_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("collectAnchors: %w", err)
	}
	defer rows.Close()
	var out []anchor
	perSource := map[string]int{}
	for rows.Next() {
		var a anchor
		var ccy sql.NullString
		if err := rows.Scan(&a.SilverSourceID, &a.InstrumentExternalID, &a.Name, &a.Symbol, &ccy); err != nil {
			return nil, fmt.Errorf("scan anchor: %w", err)
		}
		if perSource[a.SilverSourceID] >= maxAnchors || symbolProblem(a.Symbol) != "" {
			continue
		}
		a.Currency = ccy.String
		perSource[a.SilverSourceID]++
		out = append(out, a)
	}
	return out, rows.Err()
}

// candidateKeyset returns the set of valid input keys, used to
// reject hallucinated lookup_values during response validation.
// Keyed on (silver_source_id, lookup_kind, lookup_value).
func candidateKeyset(cands []candidate) map[string]bool {
	m := make(map[string]bool, len(cands))
	for _, c := range cands {
		m[candKey(c.SilverSourceID, c.LookupKind, c.LookupValue)] = true
	}
	return m
}

func candKey(source, kind, value string) string {
	return source + "\x00" + kind + "\x00" + value
}

// candidateStats summarises the candidate set across the two
// dimensions the run summary reports: per silver_source (how many
// to attempt per broker) and per
// lookup_kind within each source (id-keyed vs name-keyed — those
// have different failure modes).
type candidateStats struct {
	Total     int
	PerSource map[string]int
	PerKind   map[string]int
}

func summariseStats(cands []candidate) candidateStats {
	s := candidateStats{
		PerSource: map[string]int{},
		PerKind:   map[string]int{},
	}
	for _, c := range cands {
		s.Total++
		s.PerSource[c.SilverSourceID]++
		s.PerKind[c.LookupKind]++
	}
	return s
}

func formatPerSource(m map[string]int) string {
	keys := sortedKeys(m)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func formatPerKind(m map[string]int) string {
	// Stable order: by-id first (shorter), by-name second.
	parts := []string{}
	if n, ok := m["instrument_external_id"]; ok {
		parts = append(parts, fmt.Sprintf("by-id=%d", n))
	}
	if n, ok := m["name"]; ok {
		parts = append(parts, fmt.Sprintf("by-name=%d", n))
	}
	return strings.Join(parts, ", ")
}

// unresolvedCandidates returns the subset of the input candidate
// set that didn't get a resolution in `valid`. This list is
// inspected to decide whether to re-run (e.g. with more anchors,
// a different model) or to fix upstream silver data (e.g. tag
// cash-interest rows so they aren't bucketed as 'dividend' in the
// first place).
func unresolvedCandidates(cands []candidate, valid []resolution) []candidate {
	resolved := make(map[string]bool, len(valid))
	for _, r := range valid {
		resolved[candKey(r.SilverSourceID, r.LookupKind, r.LookupValue)] = true
	}
	out := make([]candidate, 0, len(cands)-len(valid))
	for _, c := range cands {
		if !resolved[candKey(c.SilverSourceID, c.LookupKind, c.LookupValue)] {
			out = append(out, c)
		}
	}
	return out
}

// printSummary writes a multi-line resolution report covering the
// numbers needed to assess success and decide on follow-up.
// Always run; both dry-run and write paths print it before the
// per-row plan / persist block.
//
// Per-source resolution rate is calculated against the candidate
// total, not against the model's response — i.e. unresolved
// includes both rows the model skipped and rows the model attempted
// but failed validation on. That's the right denominator: a
// candidate counts as "resolved" only if it ended up in
// symbol_resolutions.
func printSummary(w io.Writer, stats candidateStats, valid []resolution, unresolved []candidate, attempts, totalInvalid int) {
	perSourceResolved := map[string]int{}
	perKindResolved := map[string]int{}
	for _, r := range valid {
		perSourceResolved[r.SilverSourceID]++
		perKindResolved[r.LookupKind]++
	}
	perSourceUnresolved := map[string]int{}
	for _, c := range unresolved {
		perSourceUnresolved[c.SilverSourceID]++
	}

	fmt.Fprintln(w, "resolve-symbols: summary")
	fmt.Fprintf(w, "  candidates sent:    %d (%s)\n", stats.Total, formatPerSource(stats.PerSource))
	fmt.Fprintf(w, "  LLM attempts:       %d\n", attempts)
	fmt.Fprintf(w, "  hallucinated rows:  %d (rejected by validation across all attempts)\n", totalInvalid)
	fmt.Fprintf(w, "  resolved:           %d (%s; by-kind %s)\n",
		len(valid), formatPerSource(perSourceResolved), formatPerKind(perKindResolved))
	fmt.Fprintf(w, "  unresolved:         %d (%s)\n",
		len(unresolved), formatPerSource(perSourceUnresolved))

	if stats.Total > 0 {
		fmt.Fprintf(w, "  resolution rate:    %s\n", formatResolutionRate(stats.PerSource, perSourceResolved))
	}

	// Sample of unresolved candidates so the output shows WHAT
	// didn't resolve and judge whether the input is even
	// resolvable (e.g. "BANK INT 010100-020100" is not a security
	// → expected to skip; "Reg.shs Foo Corp" without a ticker
	// could be a real instrument the model couldn't place →
	// worth re-running or checking). Stratified across
	// (source, kind) groups so the sample shows the full mix,
	// not just the head of the list.
	if len(unresolved) > 0 {
		key := func(c candidate) string {
			return c.SilverSourceID + "\x00" + c.LookupKind + "\x00" + c.LookupValue
		}
		line := func(c candidate) string {
			label := c.LookupValue
			if c.LookupKind == "instrument_external_id" && c.HintName != "" {
				label = fmt.Sprintf("%s (%s)", c.LookupValue, c.HintName)
			}
			return fmt.Sprintf("    %s [%s] %s", c.SilverSourceID, c.LookupKind, label)
		}
		sample := stratifiedSample(unresolved, 10, candidateGroup)
		fmt.Fprintf(w, "  sample of unresolved candidates (%d of %d, mixed across source × kind):\n", len(sample), len(unresolved))
		inSample := make(map[string]bool, len(sample))
		for _, c := range sample {
			inSample[key(c)] = true
			fmt.Fprintln(w, line(c))
		}
		// The tail is in memory, so it is printed rather than pointed
		// at: the dry-run plan walks what the model RESOLVED, and
		// symbol_resolutions holds the resolved rows — neither is this
		// list.
		if len(unresolved) > len(sample) {
			fmt.Fprintf(w, "    ... and %d more:\n", len(unresolved)-len(sample))
			for _, c := range unresolved {
				if !inSample[key(c)] {
					fmt.Fprintln(w, line(c))
				}
			}
		}
		fmt.Fprintln(w, "  notes:")
		fmt.Fprintln(w, "    - Some unresolved rows are expected: cash-interest descriptions, currency/FX placeholders,")
		fmt.Fprintln(w, "      bullion, structured products, and private funds don't have tickers.")
		fmt.Fprintln(w, "    - To improve coverage: load more silver data (more anchors in the prompt), try --max-attempts 4-5,")
		fmt.Fprintln(w, "      or fix upstream silvers so the relevant rows carry an instrument_external_id at load time.")
	}
}

// candidateGroup is the stratification key for the unresolved sample:
// source × lookup kind, the two dimensions with different failure
// modes.
func candidateGroup(c candidate) string { return c.SilverSourceID + "/" + c.LookupKind }

// stratifiedSample picks up to maxN items by round-robin'ing across
// the groups `group` assigns, keeping the head-of-list bias out so the
// sample shows every group present in the input rather than the front
// of the biggest one. Both LLM commands' run reports sample this way —
// resolve-symbols by source × lookup kind, categorize by a merchant's
// dominant source.
func stratifiedSample[T any](items []T, maxN int, group func(T) string) []T {
	if maxN >= len(items) {
		return items
	}
	byKey := map[string][]T{}
	var keys []string
	for _, it := range items {
		k := group(it)
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], it)
	}
	sort.Strings(keys)

	out := make([]T, 0, maxN)
	for len(out) < maxN {
		progressed := false
		for _, k := range keys {
			if len(byKey[k]) == 0 {
				continue
			}
			out = append(out, byKey[k][0])
			byKey[k] = byKey[k][1:]
			progressed = true
			if len(out) == maxN {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

func formatResolutionRate(total, resolved map[string]int) string {
	keys := sortedKeys(total)
	parts := make([]string, len(keys))
	for i, k := range keys {
		t := total[k]
		r := resolved[k]
		pct := 0.0
		if t > 0 {
			pct = 100.0 * float64(r) / float64(t)
		}
		parts[i] = fmt.Sprintf("%s=%.1f%% (%d/%d)", k, pct, r, t)
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- retry loop -----------------------------------------------------------

// resolveWithLLM runs the candidate set through the model. On each
// attempt, it parses the response, partitions into valid + invalid,
// merges valid rows into the running set (deduped by key), and
// re-prompts with targeted feedback if any invalid rows remain.
// Returns the union of valid resolutions across all attempts, the
// number of attempts spent, and the total number of invalid rows
// rejected.
func resolveWithLLM(
	ctx context.Context,
	cfg *config.ModelConfig,
	candidates []candidate,
	anchors []anchor,
	candKeyset map[string]bool,
	configuredSources map[string]bool,
	maxAttempts int,
	noCurrency, showPrompt bool,
	stdout, stderr io.Writer,
) (validUnion []resolution, attempts int, totalInvalid int, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	seenValid := map[string]bool{}
	var lastInvalid []invalidRow

	systemPrompt := buildSystemPrompt()
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		userPrompt := buildUserPrompt(candidates, anchors, noCurrency, lastInvalid)
		if showPrompt {
			fmt.Fprintf(stderr, "--- LLM prompt (attempt %d) ---\n%s\n--- end prompt ---\n", attempts, userPrompt)
		}
		raw, err := callLLM(ctx, cfg, systemPrompt, userPrompt)
		if err != nil {
			return validUnion, attempts, totalInvalid, fmt.Errorf("LLM call (attempt %d): %w", attempts, err)
		}
		body := stripThinkingBlocks(raw)
		fmt.Fprintf(stdout, "resolve-symbols: attempt %d: %d chars of CSV response (after stripping reasoning)\n",
			attempts, len(body))

		fresh, invalid := parseAndValidate(body, candKeyset, configuredSources)
		totalInvalid += len(invalid)

		for _, r := range fresh {
			k := candKey(r.SilverSourceID, r.LookupKind, r.LookupValue)
			if seenValid[k] {
				continue
			}
			seenValid[k] = true
			validUnion = append(validUnion, r)
		}

		if len(invalid) == 0 {
			return validUnion, attempts, totalInvalid, nil
		}
		fmt.Fprintf(stdout, "resolve-symbols: attempt %d: %d invalid row(s) — sample:\n", attempts, len(invalid))
		sampleN := 5
		if len(invalid) < sampleN {
			sampleN = len(invalid)
		}
		for _, iv := range invalid[:sampleN] {
			fmt.Fprintf(stdout, "    %v — %s\n", iv.Raw, iv.Reason)
		}
		if attempts == maxAttempts {
			break
		}
		lastInvalid = invalid
	}
	return validUnion, attempts, totalInvalid, nil
}

// ---- prompt assembly -------------------------------------------------------

func buildSystemPrompt() string {
	return `You are a financial data assistant. You map instrument descriptions and instrument identifiers (ISIN, CUSIP) to their commonly-listed ticker symbols. You output CSV only — no prose, no markdown, no explanations.`
}

// buildUserPrompt assembles the per-attempt user message. The
// anchor list teaches the model the broker-specific ticker
// conventions in this portfolio; the candidate list is what to
// resolve. feedback, when non-empty, names rows from the previous
// attempt that failed validation so the model drops them on the
// retry.
func buildUserPrompt(candidates []candidate, anchors []anchor, noCurrency bool, feedback []invalidRow) string {
	var b strings.Builder
	b.WriteString(`A user maintains a portfolio across several brokers. Their database has rows where the ticker symbol is missing. Given the unresolved rows below, emit one CSV row per row you can confidently resolve.

Broker context:
- schwab: US brokerage; expect US-listed tickers (e.g. SCHD, TFLO, BABA, VTI).
- ubs: Swiss bank with global UCITS / European coverage; expect Xetra / LSE / SIX tickers for ETFs (e.g. XDEW, EQQE, MEUD); native-exchange tickers for individual stocks (e.g. NOVN.SW, KPN.AS, INGA.AS, DBK.DE).
- swissquote: Swiss broker; expect SIX / Xetra / LSE tickers.

`)
	b.WriteString("Reference examples — same portfolio, already resolved. Each example shows the INPUT row (same shape as the unresolved candidates below) followed by the correct OUTPUT row (the 4-column response format).\n\n")
	b.WriteString("INPUT rows (with hint columns):\n")
	b.WriteString(formatAnchorInputCSV(anchors, noCurrency))
	b.WriteString("\nCORRECT OUTPUT rows (4 columns, this is the format your response must use):\n")
	b.WriteString(formatAnchorOutputCSV(anchors))
	b.WriteString("\nUnresolved rows that need a ticker (same INPUT shape as above):\n")
	b.WriteString(formatCandidateCSV(candidates, noCurrency))

	b.WriteString(`
Output format:
- CSV with columns: silver_source_id,lookup_kind,lookup_value,symbol
- No header row. One row per resolved item.
- Double-quote any field containing a comma; backslash-escape inner quotes.
- silver_source_id and lookup_kind must match the input row exactly.
- lookup_value must appear verbatim in the input set above.
- symbol must be the security's commonly-listed ticker (uppercase letters/digits/dots/hyphens, no spaces, 1-12 characters).

Skip any row you cannot confidently resolve. In particular skip:
- cash interest credits like "BANK INT 010100-020100 SCHWAB BANK", "INTEREST 01/01THRU 02/01", "SCHWAB1 INT 01/01-02/01";
- bank journal / fee / tax rows with no security in the description;
- descriptions so mangled that multiple unrelated tickers would plausibly fit.

Do not include explanatory prose. Output CSV only.
`)
	if len(feedback) > 0 {
		b.WriteString("\nYour previous response contained the following rows that I rejected:\n")
		for _, iv := range feedback {
			fmt.Fprintf(&b, "  %v — reason: %s\n", iv.Raw, iv.Reason)
		}
		b.WriteString("\nRe-emit your full response with those invalid rows dropped. Keep the rows that WERE valid; do not add new invalid ones. Output CSV only.\n")
	}
	return b.String()
}

// formatAnchorInputCSV renders the anchor set in the same shape
// as the candidate input: silver_source_id, lookup_kind,
// lookup_value, hint_name, [currency]. Used to demonstrate to the
// model what the candidate input rows look like.
func formatAnchorInputCSV(anchors []anchor, noCurrency bool) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		row := []string{a.SilverSourceID, "instrument_external_id", a.InstrumentExternalID, a.Name}
		if !noCurrency {
			row = append(row, a.Currency)
		}
		_ = w.Write(row)
	}
	w.Flush()
	return b.String()
}

// formatAnchorOutputCSV renders the anchor set in the strict
// 4-column output shape: silver_source_id, lookup_kind,
// lookup_value, symbol. This is the format the model's response
// must follow exactly.
func formatAnchorOutputCSV(anchors []anchor) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		_ = w.Write([]string{a.SilverSourceID, "instrument_external_id", a.InstrumentExternalID, a.Symbol})
	}
	w.Flush()
	return b.String()
}

// formatCandidateCSV renders the unresolved input set. Columns:
// silver_source_id, lookup_kind, lookup_value, hint_name, currency.
// For name-keyed rows hint_name is empty (the lookup_value IS the
// descriptive label).
func formatCandidateCSV(cands []candidate, noCurrency bool) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, c := range cands {
		row := []string{c.SilverSourceID, c.LookupKind, c.LookupValue, c.HintName}
		if !noCurrency {
			row = append(row, c.Currency)
		}
		_ = w.Write(row)
	}
	w.Flush()
	return b.String()
}

// ---- response parsing + validation -----------------------------------------

// parseAndValidate parses the model's CSV body and partitions rows
// into valid resolutions (every field passes the per-field check)
// and invalid rows (carried into the next attempt's feedback).
// Tolerates leading prose, code fences, and trailing blank lines —
// finds the first row that looks like a valid CSV row and parses
// forward from there.
func parseAndValidate(body string, candKeyset map[string]bool, configuredSources map[string]bool) ([]resolution, []invalidRow) {
	body = stripCodeFences(body)
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1 // tolerate ragged rows; we validate explicitly
	r.LazyQuotes = true

	var valid []resolution
	var invalid []invalidRow
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip the offending record but keep parsing — the
			// next valid row may be fine.
			invalid = append(invalid, invalidRow{Raw: nil, Reason: fmt.Sprintf("CSV parse error: %v", err)})
			continue
		}
		if len(row) < 4 {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fmt.Sprintf("expected at least 4 columns, got %d", len(row))})
			continue
		}
		// First 3 columns are the lookup key (silver_source_id,
		// lookup_kind, lookup_value); the LAST column is the
		// emitted symbol. The model occasionally replays the full
		// candidate row (hint_name, currency) in the middle when
		// it pattern-matches the anchor format — taking the last
		// column tolerates that pattern without losing strictness
		// on the key.
		source := strings.TrimSpace(row[0])
		kind := strings.TrimSpace(row[1])
		value := row[2] // do NOT trim — lookup_value must match verbatim
		symbol := strings.TrimSpace(row[len(row)-1])

		if !configuredSources[source] {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fmt.Sprintf("unknown silver_source_id %q", source)})
			continue
		}
		if kind != "instrument_external_id" && kind != "name" {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fmt.Sprintf("invalid lookup_kind %q", kind)})
			continue
		}
		if !candKeyset[candKey(source, kind, value)] {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fmt.Sprintf("lookup_value %q not in input set for (%s,%s)", value, source, kind)})
			continue
		}
		if symbol == value {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fmt.Sprintf("symbol %q equals lookup_value (model echoed input)", symbol)})
			continue
		}
		if problem := symbolProblem(symbol); problem != "" {
			invalid = append(invalid, invalidRow{Raw: row, Reason: problem})
			continue
		}
		valid = append(valid, resolution{
			SilverSourceID: source,
			LookupKind:     kind,
			LookupValue:    value,
			Symbol:         symbol,
		})
	}
	return valid, invalid
}

// symbolProblem says why symbol does not have a ticker's shape, or ""
// when it does. An answer must pass it to be stored, and an existing
// instrument's symbol to serve as an anchor: an anchor showing an
// identifier as a symbol teaches the model to answer with identifiers.
func symbolProblem(symbol string) string {
	switch {
	case !tickerShapeRe.MatchString(symbol):
		return fmt.Sprintf("symbol %q does not look ticker-shaped", symbol)
	case isinShapeRe.MatchString(symbol):
		return fmt.Sprintf("symbol %q is ISIN-shaped (not a real ticker)", symbol)
	case cusipShapeRe.MatchString(symbol):
		return fmt.Sprintf("symbol %q is CUSIP-shaped (not a real ticker)", symbol)
	}
	return ""
}

// ---- persistence -----------------------------------------------------------

// manualOverrideModelName is the model_name string written into
// symbol_resolutions for rows that came from cfg.symbol_resolution.overrides
// rather than an LLM. Distinguishes them in the resolutions dump
// and lets syncSymbolOverrides target just those rows when
// reconciling cfg ↔ DB.
const manualOverrideModelName = "manual-override"

// syncSymbolOverrides reconciles cfg.symbol_resolution.overrides into the
// symbol_resolutions table. Three phases, all in one transaction:
//
//  1. Remove every existing row tagged model_name='manual-override'
//     (so previously-synced corrections that aren't in cfg anymore
//     disappear from the DB).
//  2. For each cfg override entry:
//     - if `delete: true`, DELETE the matching PK row regardless
//     of model_name (suppresses an LLM result marked
//     as garbage — US Treasury CUSIPs, private products etc.);
//     - otherwise UPSERT it as a manual-override row, replacing
//     any prior LLM result for the same key.
//
// Returns (purged, upserted, suppressed) counts:
//   - purged: rows wiped by phase 1 (previous-run manual overrides)
//   - upserted: rows written by cfg-driven corrections
//   - suppressed: rows wiped by `delete: true` cfg entries
//
// Note on precedence: removing an override from cfg in a later
// edit removes its row from the DB. Any LLM result the override
// was hiding is also gone (the override overwrote it). The next
// `wealthdb resolve-symbols` (without --overrides-only) re-derives.
func syncSymbolOverrides(ctx context.Context, db *sql.DB, overrides []config.SymbolOverride) (purged, upserted, suppressed int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM symbol_resolutions WHERE model_name = ?`, manualOverrideModelName)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("delete stale manual-override rows: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		purged = int(n)
	}

	if len(overrides) > 0 {
		upsertStmt, err := tx.PrepareContext(ctx, `
INSERT INTO symbol_resolutions
    (silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, lookup_kind, lookup_value) DO UPDATE SET
    symbol      = EXCLUDED.symbol,
    resolved_at = EXCLUDED.resolved_at,
    model_name  = EXCLUDED.model_name`)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("prepare override upsert: %w", err)
		}
		defer upsertStmt.Close()
		deleteStmt, err := tx.PrepareContext(ctx,
			`DELETE FROM symbol_resolutions WHERE silver_source_id = ? AND lookup_kind = ? AND lookup_value = ?`)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("prepare override delete: %w", err)
		}
		defer deleteStmt.Close()

		now := time.Now().Unix()
		for _, o := range overrides {
			if o.Delete {
				if _, err := deleteStmt.ExecContext(ctx,
					o.SilverSourceID, o.LookupKind, o.LookupValue,
				); err != nil {
					return 0, 0, 0, fmt.Errorf("delete override (%s,%s,%s): %w", o.SilverSourceID, o.LookupKind, o.LookupValue, err)
				}
				suppressed++
				continue
			}
			if _, err := upsertStmt.ExecContext(ctx,
				o.SilverSourceID, o.LookupKind, o.LookupValue, o.Symbol,
				now, manualOverrideModelName,
			); err != nil {
				return 0, 0, 0, fmt.Errorf("upsert override (%s,%s,%s): %w", o.SilverSourceID, o.LookupKind, o.LookupValue, err)
			}
			upserted++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return purged, upserted, suppressed, nil
}

// persistResolutions upserts the validated rows into the
// symbol_resolutions table. Returns a per-source count of rows
// INSERTed (NB: ON CONFLICT DO UPDATE doesn't distinguish insert
// vs update at the row level, so the "new" counter here is the
// count of touched rows, which is fine for the user-facing summary)
// and the total row count of the table after the upsert.
func persistResolutions(ctx context.Context, db *sql.DB, rows []resolution, resolvedAt int64, modelName string) (map[string]int, int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("begin tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO symbol_resolutions
    (silver_source_id, lookup_kind, lookup_value, symbol, resolved_at, model_name)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (silver_source_id, lookup_kind, lookup_value) DO UPDATE SET
    symbol      = EXCLUDED.symbol,
    resolved_at = EXCLUDED.resolved_at,
    model_name  = EXCLUDED.model_name`)
	if err != nil {
		_ = tx.Rollback()
		return nil, 0, fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	perSource := map[string]int{}
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.SilverSourceID, r.LookupKind, r.LookupValue, r.Symbol, resolvedAt, modelName); err != nil {
			_ = tx.Rollback()
			return nil, 0, fmt.Errorf("upsert (%s,%s,%s): %w", r.SilverSourceID, r.LookupKind, r.LookupValue, err)
		}
		perSource[r.SilverSourceID]++
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit: %w", err)
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_resolutions`).Scan(&total); err != nil {
		return perSource, 0, fmt.Errorf("count rows: %w", err)
	}
	return perSource, total, nil
}

// resolveSymbolsUsage is the long-form help text printed by -h.
func resolveSymbolsUsage() string {
	return `usage: wealthdb resolve-symbols [-n | --dry-run] [--batch N] [--max-attempts N] [--no-currency] [--max-anchors N] [--show-prompt] [--overrides-only]

Back-fill missing instrument ticker symbols by consulting the LLM
configured in wealthdb.cfg's "symbol_resolution.model" block.
Reads candidates from:

  - instruments rows where symbol IS NULL but name IS NOT NULL
    (typical: European-listed ETFs whose descriptions don't
    carry a ticker, keyed by ISIN);
  - transactions rows with no instrument_external_id but a
    free-text description (typical: Schwab DIVIDEND_OR_INTEREST
    payloads whose transferItems only have the cash leg).

The candidates are asked about in batches of --batch, one model call
each, and each batch's answers are stored as it completes, so a batch
that fails costs only itself.

Resolved tickers are upserted into the symbol_resolutions side
table; the base instruments and transactions tables are not
touched. The read path in 'positions' and 'transactions' picks
them up via LEFT JOIN + COALESCE, so re-running the command with
better data simply overwrites stale resolutions.

cfg.symbol_resolution.overrides are synced to symbol_resolutions
on every invocation (whether or not the LLM runs), and a key they
name is never sent to the LLM, so its answer cannot displace them. Use
--overrides-only to apply cfg overrides without making an LLM call
— useful for fast correction of bad LLM resolutions.

The run report ends by listing every candidate still unresolved, one
per line, and a candidate's lookup value can be a free-text
transaction description. The listing has no -p to mask it: treat the
report as narrative data, not as a summary safe to paste.

Flags:
  -n, --dry-run         print the resolution plan, don't write
      --batch N         candidates per model call (default 100)
      --max-attempts N  retry the LLM up to N times when responses
                        contain hallucinated rows (default 3)
      --no-currency     drop the currency hint column from the prompt
                        (experiment / ablation)
      --max-anchors N   cap the in-context anchor examples per silver
                        source (default 30); a batch sees only its own
                        sources' anchors
      --show-prompt     print the full LLM prompt to stderr (debugging)
      --overrides-only  apply cfg.symbol_resolution.overrides and exit; skip the
                        LLM round-trip entirely`
}
