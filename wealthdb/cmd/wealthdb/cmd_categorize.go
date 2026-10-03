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
	"sort"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/pathmode"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

func init() {
	register("categorize", cmdCategorize)
}

// The model tier, for both families: the long tail of counterparties whose
// category follows from nothing but their name, asked of a model once per
// merchant (or payer) signature and stored globally, so a signature met on
// several accounts is paid for once. Spending asks who was paid and what
// they sell; income asks what kind of income a receipt is, so its kind floor
// outranks the store and only `deposit` is a candidate.
//
// What may leave the machine is decided in two independent places: the
// context level (<family>.categorization.context) sets how much of a
// candidate is described, and the candidacy fences decide whether a
// signature is a candidate at all — a transfer-shaped row
// (spending.RowTransferShaped), a bare person's name (spending.PersonShaped),
// or a key with nothing to name (spending.Uninformative, FilingOnly).
// docs/SPENDING.md §5–§6 and docs/INCOME.md argue both.

// Flag defaults. --max-attempts, --max-anchors and --batch mirror
// resolve-symbols so the two LLM commands behave the same way under
// the same flags; the batch default is this command's own, because a
// merchant prompt carries the whole taxonomy.
const (
	defaultCategorizeMaxAttempts = 3
	defaultCategorizeMaxAnchors  = 30

	// defaultCategorizeBatch is how many merchant signatures one model
	// call carries (see splitBatches). At forty, a model served locally
	// answers in well under a minute — the taxonomy and anchor blocks
	// dominate the prompt and are paid once per batch regardless of its
	// size, while the answer is forty short rows.
	defaultCategorizeBatch = 40
)

// neighbourWindowDays is how far either side of a transaction the
// `transaction` context level looks for company. One day: a purchase's
// useful neighbours are the ones on the same outing, and a wider band
// just dilutes the signal with unrelated merchants.
const neighbourWindowDays = 1

// maxNeighbours caps the nearby signatures shown per sample.
const maxNeighbours = 3

// merchantSample is one transaction standing behind a candidate
// signature, carrying the material the wider context levels send.
// Descriptor is the RAW narrative and is fenced before it is ever
// written into a prompt.
type merchantSample struct {
	Source      string
	AccountKind string
	Day         int64
	Currency    string
	Amount      float64
	Descriptor  string
	Neighbours  []string
}

// merchantCandidate is one merchant signature offered to the model,
// with the counts that let the run report say how much of the ledger
// each verdict covers.
type merchantCandidate struct {
	Signature string
	Txns      int
	PerSource map[string]int
	Samples   []merchantSample
}

// DominantSource is the source contributing the most transactions to
// this signature — the stratification key for the leftover sample, so
// a merchant met by more than one source is attributed to the one that
// met it most.
func (c merchantCandidate) DominantSource() string {
	best, bestN := "", -1
	for _, s := range sortedKeys(c.PerSource) {
		if n := c.PerSource[s]; n > bestN {
			best, bestN = s, n
		}
	}
	return best
}

// skippedSignatures counts what candidacy refused, by reason. Each is
// a count of DISTINCT signatures, not rows, and each is reported so
// a gate that fired is visible rather than leaving it unclear why
// a row stayed uncategorised.
type skippedSignatures struct {
	Fenced        int // transfer-shaped: a person or an account where a merchant would be
	PersonShaped  int // a bare person's name on a non-card account, when the option is on
	Uninformative int // nothing to name: no word at all, or nothing but the provider's own filing
	// PersonFenceOffKey is the config key that turned the person-shape
	// arm off, or empty when it is on. The run report prints it so a
	// reader can see which policy produced the list rather than infer
	// it from a zero — and prints the key the running family actually
	// resolved, which for an income run that inherited is spending's
	// and for one with a block of its own is income's.
	PersonFenceOffKey string
}

// merchantAnchor is one verdict already in the store, shown to the
// model as an in-context example. Anchors exist to steer the model
// towards its OWN prior vocabulary: left to itself it will place one
// coffee chain under FOOD_AND_DRINK_COFFEE and the next under
// FOOD_AND_DRINK_RESTAURANT, and a report that splits one habit across
// two categories is worse than one that puts it in the wrong single
// category.
type merchantAnchor struct {
	Signature string
	Name      string
	Detailed  string
}

// categorization is one validated verdict, ready to upsert into the
// running family's verdict store. MerchantName holds the counterparty's
// name whichever family that is — a merchant's or a payer's — and keeps
// the older spelling because every field of this struct is written and
// read in one file and a rename would buy nothing.
type categorization struct {
	Signature    string
	MerchantName string
	Detailed     string
}

// cmdCategorize asks the configured model for a counterparty's name
// and its category, for every signature the deterministic tiers left
// unplaced, and stores the verdicts in that family's global store. An
// optional positional selects one family; with none, both run in order.
// The read path picks the verdicts up through each family's resolution
// macro.
func cmdCategorize(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb categorize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("n", false, "show the categorisation plan without writing to gold")
	fs.BoolVar(dryRun, "dry-run", false, "show the categorisation plan without writing to gold")
	maxAttempts := fs.Int("max-attempts", defaultCategorizeMaxAttempts,
		"max LLM round-trips when responses include invalid rows")
	maxAnchors := fs.Int("max-anchors", defaultCategorizeMaxAnchors,
		"max anchor examples to include in the prompt")
	batch := fs.Int("batch", defaultCategorizeBatch,
		"signatures per model call; the default keeps a local model's answer well inside the 5-minute call timeout")
	showPrompt := fs.Bool("show-prompt", false, "print the first batch's prompt to stderr in full; later batches only its size (debugging)")
	all := fs.Bool("all", false, "re-ask every signature candidacy admits, including ones already in the verdict store")
	refine := fs.Bool("refine", false, "re-ask only the counterparties the model itself could place no better than a catch-all")
	fs.Usage = func() {
		fmt.Fprintln(stderr, categorizeUsage())
	}
	if err := fs.Parse(reorderFlagsFirst(subargs, categorizeValueFlags)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "categorize: bad flags")
	}
	families, ok := resolveCategorizeFamilies(strings.Join(fs.Args(), " "))
	if !ok {
		fs.Usage()
		return errs.Newf(2, "categorize: unknown family %q (want spending | income, or neither for both)",
			strings.Join(fs.Args(), " "))
	}
	if *batch < 1 {
		fs.Usage()
		return errs.Newf(2, "categorize: --batch must be at least 1, got %d", *batch)
	}
	// Beside the other flag check, and before the pass takes the gold
	// write lock: a usage error must not cost a full overlay rewrite.
	if *all && *refine {
		return errs.Newf(2, "categorize: --all and --refine choose different backlogs; pass one")
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	// Every family's model block is validated BEFORE the gold lock is
	// taken: a run that would fail on the second family's config must
	// not first spend a model pass on the first.
	for _, fam := range families {
		cz := fam.categorization(cfg)
		modelKey := fam.categorizationKey(cfg) + ".model"
		modelCfg := cz.CategorizationModel()
		if modelCfg == nil {
			return errs.Newf(2, "categorize: %s is not set; add a `%s` block to %s",
				modelKey, modelKey, g.ConfigPath)
		}
		if err := validateModelConfig(modelKey, modelCfg); err != nil {
			return errs.Newf(2, "categorize: %s", err.Error())
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
			"'categorize' requires write access to the gold database, but '%s' is read-only (detected: %s). "+
				"Pass --dry-run if you only want to see the plan.", cfg.GoldDB, dec.Reason)
	}
	// A dry run is a PURE READ: it opens read-only, takes no write
	// lock, and a parallel `wealthdb transactions` can read the file
	// while the model is responding.
	openMode := gold.ModeReadWrite
	var enrichment enrichmentLedgers
	if *dryRun {
		openMode = gold.ModeReadOnly
	} else {
		// Only a real run applies the ledgers, and it reads them before
		// gold is opened read-write (parseEnrichmentLedgers).
		if enrichment, err = parseEnrichmentLedgers(cfg); err != nil {
			return err
		}
		// A real run holds the gold write mutex end to end. The
		// verdicts it buys are the one thing in gold with no other
		// source of truth, and 'compact' / 'reload -a' carry the
		// verdict stores by a read taken at the start of a rebuild
		// that ends in a rename — anything stored after that read
		// would be swapped away and still reported as stored.
		lock, err := lockGoldForWrite(cfg.GoldDB, "categorize")
		if err != nil {
			return err
		}
		defer lock.unlock()
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	// The handle is released before the model round-trips begin (see
	// below) and re-taken per flush. dbOpen keeps the deferred close
	// correct on the paths that return early.
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	// The deterministic pass is a WRITE, so a dry run cannot run it —
	// and must say so rather than quietly planning against a different
	// world than a real run would see. A source loaded since the last
	// pass, or a config scope edit, changes the candidate set; the
	// plan below is as of the last load either way.
	//
	// It runs ONCE for both families, because it writes both overlays
	// in one transaction.
	if *dryRun {
		fmt.Fprintln(stdout, "categorize: dry-run — gold opened read-only, so the deterministic pass did NOT run.")
		fmt.Fprintln(stdout, "categorize: the candidate set below is AS OF THE LAST LOAD; a real run re-asserts it first.")
	} else if err := runEnrichmentPass(ctx, db, cfg, enrichment, stdout); err != nil {
		return err
	}

	opts := categorizeRunOptions{
		dryRun: *dryRun, maxAttempts: *maxAttempts, maxAnchors: *maxAnchors,
		batch: *batch, showPrompt: *showPrompt, which: backlogOf(*all, *refine),
	}
	db, dbOpen, err = runCategorizeFamilies(ctx, db, dbOpen, cfg, families, opts, openMode, stdout, stderr)
	return err
}

// runCategorizeFamilies plans and runs each family in turn, re-opening
// the gold handle between them.
//
// It is its own function so a test can drive the whole loop — two
// families, the close-and-reopen between them, the per-family flush —
// with a scripted endpoint. cmdCategorize above is flags, gates and the
// deterministic pass; this is the part with the sequencing in it.
//
// Returns the handle and whether it is still open, so the caller's
// deferred close stays correct on every path.
func runCategorizeFamilies(
	ctx context.Context,
	db *sql.DB,
	dbOpen bool,
	cfg *config.Config,
	families []categorizeFamily,
	opts categorizeRunOptions,
	openMode gold.Mode,
	stdout, stderr io.Writer,
) (*sql.DB, bool, error) {
	for i, fam := range families {
		if i > 0 {
			// Only where the previous family CLOSED the handle: a
			// family with nothing to ask about returns without
			// reaching the model pass, and DuckDB refuses a second
			// connection to the same file under a different mode.
			//
			// Retried for retryFlush's reason, and it is the same
			// race: the handle was released for the whole of the
			// first family's model pass precisely so readers could
			// use gold, and a reader still holding it at the instant
			// that family ends makes a bare open fail. Failing there
			// would abandon the second family after the first has
			// already been paid for.
			if !dbOpen {
				if err := retryFlush(ctx, func() error {
					opened, err := gold.Open(cfg.GoldDB, openMode)
					if err != nil {
						return err
					}
					db = opened
					return nil
				}); err != nil {
					return db, false, errs.Wrap(errs.ExitOpenFailed, err)
				}
				dbOpen = true
			}
			fmt.Fprintln(stdout)
		}
		closed, err := runCategorizeFamily(ctx, db, cfg, fam, opts, stdout, stderr)
		dbOpen = !closed
		if err != nil {
			return db, dbOpen, err
		}
	}
	return db, dbOpen, nil
}

// categorizeRunOptions is one invocation's flags, the same for every
// family it runs.
type categorizeRunOptions struct {
	dryRun      bool
	maxAttempts int
	maxAnchors  int
	batch       int
	showPrompt  bool
	// call overrides the model endpoint. Nil in production, where the
	// configured endpoint is used; a test sets it so the loop above can
	// be driven end to end without a model on the other end. It is the
	// same seam llm.go's llmCall already is, lifted one level so the
	// caller of the loop can supply it.
	call  llmCall
	which backlog
}

// runCategorizeFamily plans and runs one family's model pass.
//
// It reports whether it CLOSED the gold handle, which it does before
// the model round-trips begin: DuckDB is one read-write handle or many
// read-only ones, so holding it across a run of N batches at up to five
// minutes each would lock every reader out of gold for the whole run.
// The caller re-opens for the next family. The write mutex the command
// took still excludes other writers throughout.
func runCategorizeFamily(
	ctx context.Context,
	db *sql.DB,
	cfg *config.Config,
	fam categorizeFamily,
	opts categorizeRunOptions,
	stdout, stderr io.Writer,
) (closed bool, err error) {
	cz := fam.categorization(cfg)
	modelCfg := cz.CategorizationModel()
	level := cz.ContextLevel()
	// Read once and passed to both scans, so the candidate list and the
	// anchor block can never be fenced under different policies within
	// one run. Income inherits the setting with the rest of the block,
	// which is the point of inheriting whole: one household, one answer
	// to what may leave the machine.
	fencePersons := cz.PersonNameFence()

	candidates, skipped, err := collectMerchantCandidates(ctx, db, fam, level, cz.Samples(),
		opts.which, fencePersons, fam.categorizationKey(cfg))
	if err != nil {
		return false, err
	}

	// The canaries are the spending family's data-quality check and
	// are printed once, with it.
	var canaries *spendCanaries
	if fam.name == "spending" {
		if canaries, err = collectSpendCanaries(ctx, db, cfg); err != nil {
			return false, err
		}
	}

	if len(candidates) == 0 {
		// The counts inline as well as through printNeverSent below: on
		// a run with nothing to ask about, "why" is the whole of the
		// message, and printNeverSent stays silent on a gate that
		// refused nothing.
		fmt.Fprintf(stdout, "categorize: %s: nothing to categorise (%d fenced, %d person-shaped, %d uninformative)\n",
			fam.name, skipped.Fenced, skipped.PersonShaped, skipped.Uninformative)
		if skipped.PersonFenceOffKey != "" {
			fmt.Fprintf(stdout, "categorize: person-shape fence off (%s.fence_person_names)\n",
				skipped.PersonFenceOffKey)
		}
		printSpendCanaries(stdout, canaries)
		return false, nil
	}

	anchors, err := collectMerchantAnchors(ctx, db, fam, opts.maxAnchors, candidateSignatures(candidates), fencePersons)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(stdout, "categorize: %s: %d %s(s) over %d transaction(s), %d anchor(s), context %q, model %s\n",
		fam.name, len(candidates), fam.counterparty, totalCandidateTxns(candidates),
		len(anchors), level, modelCfg.Name)
	printNeverSent(stdout, skipped)

	// Candidates arrive sorted by signature, so a batch is an
	// alphabetical slice — which is fine, because every batch carries
	// the whole taxonomy and its own anchors, and nothing in a batch
	// depends on the candidates outside it.
	batches := splitBatches(candidates, opts.batch)
	printCategorizeBatchPlan(stdout, fam, batches, opts.batch, anchors, level, opts.maxAttempts, opts.dryRun)

	if err := db.Close(); err != nil {
		return true, errs.Wrap(errs.ExitOpenFailed,
			fmt.Errorf("categorize: close gold before the model pass: %w", err))
	}
	closed = true

	store := &verdictStore{warn: stderr, write: func(rows []categorization) (int, error) {
		wdb, err := gold.ReopenReadWrite(cfg.GoldDB)
		if err != nil {
			return 0, err
		}
		defer wdb.Close()
		return persistCategorizations(ctx, wdb, fam, rows, time.Now().Unix(), modelCfg.Name)
	}}
	sink := store.accept
	if opts.dryRun {
		sink = func(batchOutcome) error { return nil }
	}
	call := opts.call
	if call == nil {
		call = modelCaller(modelCfg)
	}
	valid, calls, totalInvalid, runErr := categorizeWithLLM(ctx, call, fam, batches, anchors,
		level, opts.maxAttempts, opts.maxAnchors, opts.showPrompt, sink, stdout, stderr)
	// The retry flush runs whether or not the family's own run
	// stopped, and BEFORE the next family is started: a run of two
	// families must not leave the first one's last batch unflushed
	// while the second spends another model pass.
	flushErr := retryFlush(ctx, store.flush)
	if runErr != nil {
		if opts.dryRun {
			fmt.Fprintf(stdout, "categorize: stopped; nothing stored (dry run)\n")
		} else {
			if flushErr != nil {
				fmt.Fprintf(stderr, "categorize: %d verdict(s) could not be stored: %v\n",
					len(store.pending), flushErr)
			}
			fmt.Fprintf(stdout, "categorize: stopped; %d verdict(s) from %d completed batch(es) are already stored — re-run to continue with the rest\n",
				store.stored, store.completed)
		}
		return closed, runErr
	}
	if flushErr != nil {
		return closed, fmt.Errorf("categorize: %d verdict(s) could not be stored; re-run to ask for them again: %w",
			len(store.pending), flushErr)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Signature < valid[j].Signature })

	leftovers := uncategorisedCandidates(candidates, valid)
	printCategorizeSummary(stdout, fam, candidates, valid, leftovers, len(batches), calls, totalInvalid, canaries)

	if opts.dryRun {
		fmt.Fprintf(stdout, "--- %s dry-run plan (no rows written; candidates as of the last load) ---\n", fam.name)
		for _, v := range valid {
			fmt.Fprintf(stdout, "  %s → %s [%s]\n", v.Signature, v.MerchantName, v.Detailed)
		}
		return closed, nil
	}
	if store.stored == 0 {
		fmt.Fprintf(stdout, "categorize: no valid verdicts to persist\n")
		return closed, nil
	}
	fmt.Fprintf(stdout, "categorize: %d verdict(s) upserted over %d batch(es); total %s rows now %d\n",
		store.stored, len(batches), fam.storeTable, store.total)
	return closed, nil
}

// backlog names which signatures a run asks about.
type backlog int

const (
	// backlogUnplaced is the default: signatures no tier could place.
	backlogUnplaced backlog = iota
	// backlogAll re-asks every signature candidacy admits, placed or
	// not — what a taxonomy revision or a model change wants.
	backlogAll
	// backlogRefine re-asks only where the MODEL's own verdict is a
	// catch-all: it was asked, and could say no more than the primary
	// already did. It is deliberately the narrowest re-ask, and it is
	// scoped by provenance rather than by value for one reason — a
	// catch-all placed by a RULE or a PIN is a considered decision
	// (the taxonomy has no word for a portrait photographer, so one
	// was chosen on purpose), and a pass that re-asked those would
	// undo deliberate work and push private individuals at a model.
	backlogRefine
)

func backlogOf(all, refine bool) backlog {
	switch {
	case all:
		return backlogAll
	case refine:
		return backlogRefine
	}
	return backlogUnplaced
}

// ---- candidate collection ---------------------------------------------------

// collectMerchantCandidates reads the model tier's backlog.
//
// DEFAULT: every counterparty signature with at least one row whose
// RESOLVED category is still NULL — read from the family's resolution
// macro, the lattice's one definition (SPENDING.md §3), so this command
// cannot drift from what a report would call categorised. A resolved
// category covers both halves at once: a signature the store already
// answers is paid for, and a signature every one of whose rows a rule
// or the provider map placed has nothing left to ask about.
//
// --all: every signature the family's candidacy admits, store row or
// not, deterministic verdict or not. That is what re-asks a
// counterparty after a taxonomy revision or a model change. It lifts
// the backlog filter and nothing else — the kind gate
// (fam.candidateKinds) and the fence still apply, so on the income
// side it re-asks deposits and never reaches a floor-placed row.
//
// EITHER WAY the transfer fence runs, and a fenced signature is
// excluded from candidacy — not merely described less. The fence is
// read over the whole ROW (spending.RowTransferShaped): the key alone
// under-fences, because a mobile person-to-person rail leads the
// narrative and names no payee, so a reduction that prefers the
// counterparty can leave a key that is a private individual's name and
// nothing else. So is a signature with no word in it
// (spending.Uninformative), and one that is nothing but the provider's
// own filing of the row (spending.FilingOnly): there is nothing to
// name, and a round-trip spent on it ends in the gauntlet every time.
// Both counts are returned so the run report can show each gate doing
// something rather than silently doing nothing.
func collectMerchantCandidates(ctx context.Context, db *sql.DB, fam categorizeFamily, level string, samples int, which backlog, fencePersonNames bool, czKey string) ([]merchantCandidate, skippedSignatures, error) {
	// The backlog is the RESOLVED value being NULL, which on the income
	// side is the difference between a few hundred payers and several
	// thousand pointless questions: a dividend the kind floor placed at
	// query time has a signature and no stored verdict, and asking a
	// model about it would be work with a known answer.
	backlogFilter := `
   AND c.` + fam.valueColumn + ` IS NULL`
	switch which {
	case backlogAll:
		backlogFilter = ""
	case backlogRefine:
		backlogFilter = `
   AND c.provenance = 'model'
   AND EXISTS (SELECT 1 FROM spend_categories sc
                WHERE sc.spend_detailed = c.` + fam.valueColumn + ` AND sc.catch_all)`
	}
	// The kind gate, which --all does NOT lift. The backlog filter is
	// about what is still unanswered; this is about what may be asked
	// at all, so the two are ANDed rather than alternated: on the
	// income side `--all` means every DEPOSIT signature including the
	// answered ones, never every signature in the population.
	q := `
SELECT c.` + fam.signatureColumn + `,
       p.silver_source_id, p.account_kind, p.occurred_at, p.currency,
       COALESCE(CAST(p.net_amount AS DOUBLE), 0),
       COALESCE(p.counterparty, ''), COALESCE(p.description, ''),
       COALESCE(p.provider_category, '')
  FROM ` + fam.resolutionMacro + ` c
  JOIN ` + fam.populationMacro + `(?, ?) p
    ON p.silver_source_id        = c.silver_source_id
   AND p.transaction_external_id = c.transaction_external_id
 WHERE c.` + fam.signatureColumn + ` IS NOT NULL
   AND c.` + fam.signatureColumn + ` <> ''` + fam.candidateKindFilter() + backlogFilter + `
 ORDER BY c.` + fam.signatureColumn + `, p.occurred_at, p.transaction_external_id`

	// The row fence, read once over the whole population rather than
	// per row here. Two reasons it cannot be a test on the row in
	// hand: this scan sees only the backlog by default, so a signature
	// carried by both a placed row and an unplaced one would be
	// admitted on the unplaced row while the rail sits on the placed
	// one the query never reaches; and even over the same rows, a key
	// admitted from a clean row before its fenced sibling arrives
	// would already be in the candidate set.
	refused, err := rowFencedSignatures(ctx, db, fam, fencePersonNames)
	if err != nil {
		return nil, skippedSignatures{}, err
	}

	rows, err := db.QueryContext(ctx, q, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, skippedSignatures{}, fmt.Errorf("categorize: read %s candidates: %w", fam.counterparty, err)
	}
	defer rows.Close()

	bySig := map[string]*merchantCandidate{}
	fencedSigs := map[string]bool{}
	personSigs := map[string]bool{}
	uninformativeSigs := map[string]bool{}
	wantSamples := level != config.SpendContextMerchant && samples > 0

	for rows.Next() {
		var (
			sig, source, kind, ccy, counterparty, description, providerCategory string
			occurredAt                                                          int64
			amount                                                              float64
		)
		if err := rows.Scan(&sig, &source, &kind, &occurredAt, &ccy, &amount,
			&counterparty, &description, &providerCategory); err != nil {
			return nil, skippedSignatures{}, fmt.Errorf("categorize: scan %s candidate: %w", fam.counterparty, err)
		}
		// The fence gates candidacy, identically at every context
		// level. A person-bearing narrative is not a merchant, and the
		// reading is over the whole row (rowFencedSignatures): a rail
		// the reduction dropped is still written in the provider's
		// filing and in the raw narrative.
		if refused.Transfer[sig] {
			fencedSigs[sig] = true
			continue
		}
		// The person-shape arm, which fires on a bare name that no
		// rail, IBAN or masked number accompanies — the one signature
		// shape that is PII by itself. It is read over the whole
		// population too (refused.Person), and for the same reason:
		// the account kind that exempts a card row belongs to the row,
		// and a key is what leaves the machine.
		if refused.Person[sig] {
			personSigs[sig] = true
			continue
		}
		// So does the word gate, at the same place and every level.
		// A bare code has nothing to name; the model would only echo
		// it and the gauntlet would only reject the echo. Neither has
		// a signature that is nothing but the bank's own booking type
		// — the bank filed the row and wrote nothing else — which
		// names how the row was booked, not whom it paid, and would
		// buy one verdict for every row filed that way; nor one that
		// names only how the money arrived.
		if spending.Uninformative(sig) || spending.FilingOnly(sig, providerCategory) ||
			spending.MechanismOnly(sig) {
			uninformativeSigs[sig] = true
			continue
		}
		c, ok := bySig[sig]
		if !ok {
			c = &merchantCandidate{Signature: sig, PerSource: map[string]int{}}
			bySig[sig] = c
		}
		c.Txns++
		c.PerSource[source]++
		if wantSamples && len(c.Samples) < samples {
			descriptor := strings.TrimSpace(counterparty)
			if descriptor == "" {
				// The description's memo — the payer's own words,
				// after canonical.DescriptionMemoSeparator — names no
				// merchant and is not sent.
				narrative, _ := canonical.SplitDescriptionMemo(description)
				descriptor = strings.TrimSpace(narrative)
			}
			// The same fence, applied a second time to the RAW
			// narrative. A signature is a folded, truncated view of
			// the narrative it came from, so a narrative can carry an
			// account number or a person in its tail that the
			// signature dropped. Fencing the sample drops the
			// narrative, never the candidate — candidacy stays
			// identical across context levels, which is what makes the
			// knob a description setting rather than a scope setting.
			if spending.TransferShaped(descriptor) {
				descriptor = ""
			}
			c.Samples = append(c.Samples, merchantSample{
				Source:      source,
				AccountKind: kind,
				Day:         gold.EpochDay(occurredAt),
				Currency:    ccy,
				Amount:      amount,
				Descriptor:  descriptor,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, skippedSignatures{}, err
	}

	out := make([]merchantCandidate, 0, len(bySig))
	for _, c := range bySig {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signature < out[j].Signature })

	if level == config.SpendContextTransaction {
		if err := attachNeighbours(ctx, db, fam, out, refused); err != nil {
			return nil, skippedSignatures{}, err
		}
	}
	off := ""
	if !fencePersonNames {
		off = czKey
	}
	return out, skippedSignatures{
		Fenced:            len(fencedSigs),
		PersonShaped:      len(personSigs),
		Uninformative:     len(uninformativeSigs),
		PersonFenceOffKey: off,
	}, nil
}

// printNeverSent reports what candidacy refused, one line per gate
// that fired. It prints beside the plan, so why a
// row will stay uncategorised before deciding to wait on the run.
func printNeverSent(w io.Writer, skipped skippedSignatures) {
	if skipped.Fenced > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) fenced as transfer-shaped and never sent\n", skipped.Fenced)
	}
	if skipped.PersonShaped > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) fenced as person-shaped and never sent\n", skipped.PersonShaped)
	}
	// A policy line rather than a count. With the arm off there is
	// nothing to count, and a reader of the report would otherwise
	// have to know the config to tell "nothing looked like a person"
	// from "nobody was looking".
	if skipped.PersonFenceOffKey != "" {
		fmt.Fprintf(w, "categorize: person-shape fence off (%s.fence_person_names)\n",
			skipped.PersonFenceOffKey)
	}
	if skipped.Uninformative > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) uninformative and never sent\n", skipped.Uninformative)
	}
}

// fencedSignatures is what the row fence refused, split by arm so the
// run report can name each. Both are sets of SIGNATURES, and a
// signature in either never leaves the machine.
type fencedSignatures struct {
	// Transfer is the rail, IBAN and masked-contact arm
	// (spending.RowTransferShaped), always read.
	Transfer map[string]bool
	// Person is the bare-name arm (spending.PersonShaped), read only
	// when spending.categorization.fence_person_names is on, and only
	// over rows on non-card accounts. Empty when the option is off.
	Person map[string]bool
}

// Refuses reports whether a signature is fenced by either arm. The
// three sites a signature leaves the machine ask this question and
// nothing finer; only the counters care which arm fired.
func (f fencedSignatures) Refuses(sig string) bool {
	return f.Transfer[sig] || f.Person[sig]
}

// rowFencedSignatures reads the fence over the WHOLE of a family's
// population — every row, its provider filing, its narrative and its
// account kind — and returns every signature at least one row refuses.
//
// It is a signature-level answer to a row-level question, and
// deliberately so. The reduction is many-to-one: several narratives
// share one key, and a key is sent to the model, never a row. If one
// row under a key was booked on a person-to-person rail, that key can
// be a private individual's name — so the key is refused whichever
// other row would have carried it in. The same set answers all three
// places a signature leaves the machine: the candidate list, the
// neighbour lists and the anchor block.
//
// The PERSON arm is the same construction over a different question,
// and it is read here rather than at the candidate scan for the same
// reason: it is exempt on CARD rows, the account kind belongs to the
// row, and a key carried by both a card row and a bank row must be
// refused on the bank row it also has. Reading it at the candidate
// scan would consult only the backlog's rows and miss the bank row
// sitting under a key some other row already placed.
//
// Why card rows are exempt: most card merchants are two or three plain
// words with no legal form and no trade word — a name-shaped arm over
// them would refuse half the spending model tier to fence a shape that
// does not arrive there. An inbound credit transfer's narrative IS the
// sender, which is why the arm exists and why it is the bank rows it
// reads.
//
// The population read here is exactly the one the candidate scan and
// the neighbour scan read, so a single pass covers all of them rather
// than each site re-testing its own rows and missing what is written
// on the rows it does not see.
func rowFencedSignatures(ctx context.Context, db *sql.DB, fam categorizeFamily, fencePersonNames bool) (fencedSignatures, error) {
	out := fencedSignatures{Transfer: map[string]bool{}, Person: map[string]bool{}}
	rows, err := db.QueryContext(ctx, `
SELECT c.`+fam.signatureColumn+`, p.account_kind,
       COALESCE(p.provider_category, ''), COALESCE(p.description, '')
  FROM `+fam.populationMacro+`(?, ?) p
  JOIN `+fam.resolutionMacro+` c
    ON c.silver_source_id        = p.silver_source_id
   AND c.transaction_external_id = p.transaction_external_id
 WHERE c.`+fam.signatureColumn+` IS NOT NULL
   AND c.`+fam.signatureColumn+` <> ''`, int64(0), gold.MaxEpoch)
	if err != nil {
		return fencedSignatures{}, fmt.Errorf("categorize: read the row fence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sig, accountKind, providerCategory, description string
		if err := rows.Scan(&sig, &accountKind, &providerCategory, &description); err != nil {
			return fencedSignatures{}, fmt.Errorf("categorize: scan the row fence: %w", err)
		}
		if !out.Transfer[sig] && spending.RowTransferShaped(sig, providerCategory, description) {
			out.Transfer[sig] = true
		}
		if fencePersonNames && !out.Person[sig] &&
			accountKind != string(canonical.AccountKindCard) && spending.PersonShaped(sig) {
			out.Person[sig] = true
		}
	}
	return out, rows.Err()
}

// attachNeighbours fills in the nearby-transaction context the
// `transaction` level sends: for each sample, the signatures of what
// else the same source booked within a day. It is what lets a model
// place an otherwise opaque counterparty from its company — a name
// that means nothing between an airline and a hotel means something.
//
// Per family: a run asks about one vocabulary and must send only that
// family's neighbours.
//
// Neighbours are fenced like everything else — on the row, through the
// set rowFencedSignatures read over this same population, so a rail
// written only in a neighbour's provider filing or raw narrative
// fences it here too, and the key-only reading needs no second call:
// the set already refuses everything it would. A neighbour equal to
// the sample's own signature is dropped as uninformative.
func attachNeighbours(ctx context.Context, db *sql.DB, fam categorizeFamily, cands []merchantCandidate, refused fencedSignatures) error {
	type dayedSig struct {
		day int64
		sig string
	}
	// This family's own population and its own signatures. Reading the
	// spending side here during an income run would be a privacy
	// defect rather than a cosmetic one: the fence set handed in is
	// built over the INCOME population, so a spending signature drawn
	// from a different population would be checked against a set that
	// never saw its row and could leave the machine unfenced.
	//
	// The family's KIND gate applies here too, and for the same reason
	// it applies to the candidate list: a neighbour is a signature that
	// LEAVES THE MACHINE, and a kind the family never asks about has no
	// business leaving it as context for one it does. Without this, a
	// deposit's neighbour list at the `transaction` level would carry
	// the signatures of every dividend booked within a day of it —
	// which on the income side is the holdings list, and is exactly
	// what the candidate gate exists to keep out of a prompt.
	rows, err := db.QueryContext(ctx, `
SELECT p.silver_source_id, p.occurred_at, c.`+fam.signatureColumn+`
  FROM `+fam.populationMacro+`(?, ?) p
  JOIN `+fam.resolutionMacro+` c
    ON c.silver_source_id        = p.silver_source_id
   AND c.transaction_external_id = p.transaction_external_id
 WHERE c.`+fam.signatureColumn+` IS NOT NULL
   AND c.`+fam.signatureColumn+` <> ''`+fam.candidateKindFilter()+`
 ORDER BY p.silver_source_id, p.occurred_at`, int64(0), gold.MaxEpoch)
	if err != nil {
		return fmt.Errorf("categorize: read neighbour context: %w", err)
	}
	defer rows.Close()

	bySource := map[string][]dayedSig{}
	for rows.Next() {
		var source, sig string
		var occurredAt int64
		if err := rows.Scan(&source, &occurredAt, &sig); err != nil {
			return fmt.Errorf("categorize: scan neighbour context: %w", err)
		}
		if refused.Refuses(sig) {
			continue
		}
		bySource[source] = append(bySource[source], dayedSig{gold.EpochDay(occurredAt), sig})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range cands {
		for j := range cands[i].Samples {
			s := &cands[i].Samples[j]
			seen := map[string]bool{cands[i].Signature: true}
			for _, n := range bySource[s.Source] {
				if n.day < s.Day-neighbourWindowDays || n.day > s.Day+neighbourWindowDays {
					continue
				}
				if seen[n.sig] {
					continue
				}
				seen[n.sig] = true
				s.Neighbours = append(s.Neighbours, n.sig)
				if len(s.Neighbours) == maxNeighbours {
					break
				}
			}
		}
	}
	return nil
}

// collectMerchantAnchors samples verdicts already in the store.
//
// Newest first, because the vocabulary worth converging on is the one
// most recently used. Signatures currently up for categorisation are
// skipped (under --all they are all in the store, and showing a
// candidate its own previous answer would just re-assert it), and so
// is any stored verdict outside the vendored vocabulary — an anchor
// carrying a delta value would teach the model exactly the vocabulary
// the gauntlet then rejects.
//
// The transfer fence applies here too, and this is the one path where
// it has to be re-applied rather than inherited: the store is
// append-only across signature revisions, so a verdict keyed on a
// signature the current fence would refuse outlives the transactions
// that produced it and would otherwise keep going out in every prompt.
// The fence gates what leaves the machine; the verdict itself stays
// usable locally, since the enrichment lookup applies it by signature.
//
// Two readings of it, because a stored key may have no row left in
// gold at all: the key on its own, which is all an orphaned verdict
// offers, and the row fence over the population
// (rowFencedSignatures), which catches the key whose rail is written
// in the rows rather than in the reduction.
//
// The PERSON arm is read only in the second of those, and deliberately
// so. It is exempt on card rows, and a key with no row left carries no
// account kind — so applying it to the key alone would refuse every
// two-word card merchant in the store and empty the anchor block of
// exactly the vocabulary it exists to reinforce. The residue is an
// orphaned verdict on a person-shaped key, bought before the arm
// existed or before it was turned on; `categorizations --forget` is
// what removes one.
func collectMerchantAnchors(ctx context.Context, db *sql.DB, fam categorizeFamily, maxAnchors int, exclude map[string]bool, fencePersonNames bool) ([]merchantAnchor, error) {
	if maxAnchors <= 0 {
		return nil, nil
	}
	refused, err := rowFencedSignatures(ctx, db, fam, fencePersonNames)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT `+fam.signatureColumn+`, `+fam.storeNameColumn+`, `+fam.valueColumn+`
  FROM `+fam.storeTable+`
 ORDER BY assigned_at DESC, `+fam.signatureColumn)
	if err != nil {
		return nil, fmt.Errorf("categorize: read anchors: %w", err)
	}
	defer rows.Close()
	var out []merchantAnchor
	for rows.Next() {
		var a merchantAnchor
		if err := rows.Scan(&a.Signature, &a.Name, &a.Detailed); err != nil {
			return nil, fmt.Errorf("categorize: scan anchor: %w", err)
		}
		if exclude[a.Signature] || !fam.emittable(a.Detailed) {
			continue
		}
		if refused.Refuses(a.Signature) || spending.TransferShaped(a.Signature) {
			continue
		}
		out = append(out, a)
		if len(out) == maxAnchors {
			break
		}
	}
	return out, rows.Err()
}

func candidateSignatures(cands []merchantCandidate) map[string]bool {
	m := make(map[string]bool, len(cands))
	for _, c := range cands {
		m[c.Signature] = true
	}
	return m
}

func totalCandidateTxns(cands []merchantCandidate) int {
	n := 0
	for _, c := range cands {
		n += c.Txns
	}
	return n
}

// uncategorisedCandidates returns the candidates that ended the run
// without a verdict — rows the model skipped and rows it answered
// invalidly alike. A candidate counts as categorised only if a verdict
// is about to be stored for it.
func uncategorisedCandidates(cands []merchantCandidate, valid []categorization) []merchantCandidate {
	done := make(map[string]bool, len(valid))
	for _, v := range valid {
		done[v.Signature] = true
	}
	out := make([]merchantCandidate, 0, len(cands)-len(valid))
	for _, c := range cands {
		if !done[c.Signature] {
			out = append(out, c)
		}
	}
	return out
}

// ---- batching ----------------------------------------------------------------

// batchOutcome is what one batch produced, handed to the sink as the
// batch completes so its verdicts can be stored before the next batch
// is asked.
type batchOutcome struct {
	Index    int // 1-based position in the run
	Count    int // batches in the run
	Size     int // candidates in this batch
	Accepted []categorization
	Rejected int // invalid rows across this batch's attempts
	Attempts int
}

// batchSink receives each batch's outcome, in order, as it completes.
// The write path persists here; a dry run observes and stores nothing.
type batchSink func(batchOutcome) error

// estimateTokens is the run's cost preview. Four characters per
// token is the usual rough ratio for English prose and CSV; the point
// is an order of magnitude before the first call, not a tokenizer.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// printCategorizeBatchPlan says what a run will cost before the first
// call is made: how many batches, how big, how many anchors the first
// carries, how large its prompt is, and how many calls the run can
// spend. On a run measured in tens of minutes this is what the
// preview is read before deciding to wait. A dry run asks the model
// exactly as a real run does — that is the precedent's shape, and
// the only way to see verdicts without writing them — so there the
// plan is the one cost signal that arrives before any call.
func printCategorizeBatchPlan(w io.Writer, fam categorizeFamily, batches [][]merchantCandidate, size int, anchors []merchantAnchor, level string, maxAttempts int, dryRun bool) {
	if len(batches) == 0 {
		return
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	n := 0
	for _, b := range batches {
		n += len(b)
	}
	first := buildCategorizeUserPrompt(fam, batches[0], anchors, level, nil)
	fmt.Fprintln(w, "categorize: plan")
	fmt.Fprintf(w, "  %-15s %d in %d batch(es) of up to %d (--batch)\n", fam.counterparty+"s:", n, len(batches), size)
	fmt.Fprintf(w, "  batch sizes:    %s\n", formatBatchSizes(batches))
	fmt.Fprintf(w, "  anchors:        %d in the first batch; each later batch sees the newest verdicts accepted so far, capped by --max-anchors\n", len(anchors))
	fmt.Fprintf(w, "  first prompt:   %d chars, ≈%d tokens (chars/4); later batches are the same shape\n", len(first), estimateTokens(first))
	fmt.Fprintf(w, "  model calls:    %d at best, %d at worst (--max-attempts %d)\n", len(batches), len(batches)*maxAttempts, maxAttempts)
	if dryRun {
		fmt.Fprintln(w, "  dry run:        the model IS asked, batch by batch; verdicts are printed at the end and never stored")
	}
}

// formatBatchSizes renders a run's batch sizes compactly — "50 × 40,
// 1 × 10" rather than fifty-one numbers.
func formatBatchSizes(batches [][]merchantCandidate) string {
	var parts []string
	run, size := 0, -1
	flush := func() {
		if run > 0 {
			parts = append(parts, fmt.Sprintf("%d × %d", run, size))
		}
	}
	for _, b := range batches {
		if len(b) != size {
			flush()
			run, size = 0, len(b)
		}
		run++
	}
	flush()
	return strings.Join(parts, ", ")
}

// refreshAnchors folds a batch's accepted verdicts into the anchor set
// for the batch after it. Anchors exist to steer the model towards its
// own prior vocabulary, and the verdicts it has just given are the
// freshest sample of that vocabulary there is — fresher than anything
// in the store, which is what the next run would read them from
// anyway. Newest first, as collectMerchantAnchors orders the store,
// and capped at maxAnchors, so the block never grows with the run.
// Batches partition the candidates, so a verdict folded in here is
// never a candidate of a later batch.
func refreshAnchors(anchors []merchantAnchor, accepted []categorization, maxAnchors int) []merchantAnchor {
	if maxAnchors <= 0 {
		return nil
	}
	out := make([]merchantAnchor, 0, len(accepted)+len(anchors))
	for _, v := range accepted {
		out = append(out, merchantAnchor{Signature: v.Signature, Name: v.MerchantName, Detailed: v.Detailed})
	}
	out = append(out, anchors...)
	if len(out) > maxAnchors {
		out = out[:maxAnchors]
	}
	return out
}

// categorizeWithLLM runs the batches through the model in order. Each
// batch is its own retry loop with its own feedback; its accepted
// verdicts go to the sink the moment the batch completes, and become
// the newest anchors for the batch after it. An error — from the
// endpoint or from the sink — stops the run at that batch, and
// everything the sink already took stays taken.
func categorizeWithLLM(
	ctx context.Context,
	call llmCall,
	fam categorizeFamily,
	batches [][]merchantCandidate,
	anchors []merchantAnchor,
	level string,
	maxAttempts, maxAnchors int,
	showPrompt bool,
	sink batchSink,
	stdout, stderr io.Writer,
) (validUnion []categorization, calls int, totalInvalid int, err error) {
	for i, batch := range batches {
		o := batchOutcome{Index: i + 1, Count: len(batches), Size: len(batch)}
		o.Accepted, o.Attempts, o.Rejected, err = categorizeBatch(ctx, call, fam, batch, anchors, level,
			o.Index, o.Count, maxAttempts, showPrompt, stdout, stderr)
		calls += o.Attempts
		totalInvalid += o.Rejected
		if err != nil {
			return validUnion, calls, totalInvalid, fmt.Errorf("batch %d/%d: %w", o.Index, o.Count, err)
		}
		fmt.Fprintf(stdout, "categorize: batch %d/%d: %d %s(s), %d accepted, %d rejected, %d attempt(s)\n",
			o.Index, o.Count, o.Size, fam.counterparty, len(o.Accepted), o.Rejected, o.Attempts)
		validUnion = append(validUnion, o.Accepted...)
		if sink != nil {
			if err := sink(o); err != nil {
				return validUnion, calls, totalInvalid, fmt.Errorf("batch %d/%d: %w", o.Index, o.Count, err)
			}
		}
		anchors = refreshAnchors(anchors, o.Accepted, maxAnchors)
	}
	return validUnion, calls, totalInvalid, nil
}

// ---- the retry loop ---------------------------------------------------------

// categorizeBatch runs one batch through the model. On each attempt it
// parses the response, partitions into valid verdicts and invalid
// rows, merges the valid ones into the running set (first answer per
// signature wins), and re-prompts with targeted feedback while any
// invalid rows remain. The feedback names this batch's rows only: a
// bad row costs its own batch a round-trip, never the run.
func categorizeBatch(
	ctx context.Context,
	call llmCall,
	fam categorizeFamily,
	batch []merchantCandidate,
	anchors []merchantAnchor,
	level string,
	index, count, maxAttempts int,
	showPrompt bool,
	stdout, stderr io.Writer,
) (accepted []categorization, attempts int, rejected int, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	candSet := candidateSignatures(batch)
	seen := map[string]bool{}
	var lastInvalid []invalidRow

	systemPrompt := fam.systemPrompt()
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		userPrompt := buildCategorizeUserPrompt(fam, batch, anchors, level, lastInvalid)
		if showPrompt {
			// The first batch's prompt in full; every later one is the
			// same shape with different rows, and fifty copies would
			// bury the one worth reading.
			if index == 1 {
				fmt.Fprintf(stderr, "--- LLM prompt (batch %d/%d, attempt %d) ---\n%s\n--- end prompt ---\n",
					index, count, attempts, userPrompt)
			} else {
				fmt.Fprintf(stderr, "--- LLM prompt (batch %d/%d, attempt %d): %d chars, ≈%d tokens ---\n",
					index, count, attempts, len(userPrompt), estimateTokens(userPrompt))
			}
		}
		raw, callErr := call(ctx, systemPrompt, userPrompt)
		if callErr != nil {
			return accepted, attempts, rejected, fmt.Errorf("LLM call (attempt %d): %w", attempts, callErr)
		}
		body := stripThinkingBlocks(raw)

		fresh, invalid := parseAndValidateCategorizations(fam, body, candSet)
		rejected += len(invalid)
		for _, v := range fresh {
			if seen[v.Signature] {
				continue
			}
			seen[v.Signature] = true
			accepted = append(accepted, v)
		}
		if len(invalid) == 0 {
			return accepted, attempts, rejected, nil
		}
		fmt.Fprintf(stdout, "categorize: batch %d/%d attempt %d: %d invalid row(s) — sample:\n",
			index, count, attempts, len(invalid))
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
	return accepted, attempts, rejected, nil
}

// ---- response parsing + the validation gauntlet ------------------------------

// parseAndValidateCategorizations parses the model's CSV body and
// partitions it into verdicts that may be stored and rows that may
// not. Every rejection carries a reason, which is what the next
// attempt's prompt feeds back.
//
// Unlike the resolve-symbols parser this one requires EXACTLY three
// columns. There the key is three columns and the value is one, so
// "the last column" disambiguates a padded row; here two of the three
// fields are free text and a padded row cannot be read at all without
// guessing which cell is the name and which the category.
func parseAndValidateCategorizations(fam categorizeFamily, body string, candSet map[string]bool) ([]categorization, []invalidRow) {
	body = stripCodeFences(body)
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1 // tolerate ragged rows; we validate explicitly
	r.LazyQuotes = true

	var valid []categorization
	var invalid []invalidRow
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			invalid = append(invalid, invalidRow{Reason: fmt.Sprintf("CSV parse error: %v", err)})
			continue
		}
		if len(row) != 3 {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("expected exactly 3 columns (%s), got %d", fam.outputContract(), len(row))})
			continue
		}
		signature := strings.TrimSpace(row[0])
		name := strings.TrimSpace(row[1])
		category := strings.TrimSpace(row[2])

		if !candSet[signature] {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("signature %q was not in the candidate set", signature)})
			continue
		}
		if name == "" {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fam.counterparty + "_name is empty"})
			continue
		}
		if name == signature {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("%s_name %q is the signature verbatim (model echoed input)", fam.counterparty, name)})
			continue
		}
		if fam.isDelta(category) {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("%s %q is assigned by the matcher and the rule tier, never by a model", fam.valueColumn, category)})
			continue
		}
		detailed := strings.ToUpper(category)
		if !fam.emittable(detailed) {
			reason := fmt.Sprintf("%s %q is not a value of the taxonomy", fam.valueColumn, category)
			// A spelling an earlier taxonomy held is what a model trained
			// on it reaches for, and naming its replacement is the
			// feedback that gets the next attempt right.
			if r, ok := canonical.RetiredDetailed(detailed); ok && fam.emittable(r.Successor) {
				reason = fmt.Sprintf("%s %q is retired: use %s", fam.valueColumn, category, r.Use)
			}
			invalid = append(invalid, invalidRow{Raw: row, Reason: reason})
			continue
		}
		valid = append(valid, categorization{Signature: signature, MerchantName: name, Detailed: detailed})
	}
	return valid, invalid
}

// ---- prompt assembly ---------------------------------------------------------

// buildCategorizeUserPrompt assembles the per-attempt user message.
// The taxonomy comes first (it is the closed vocabulary the answer
// must come from), then the anchors (this deployment's own prior
// answers), then the candidates at whatever depth the context level
// allows, then the output contract, then any feedback from the last
// attempt.
func buildCategorizeUserPrompt(fam categorizeFamily, candidates []merchantCandidate, anchors []merchantAnchor, level string, feedback []invalidRow) string {
	var b strings.Builder
	b.WriteString(fam.promptPreamble())
	for _, c := range fam.modelCategories() {
		fmt.Fprintf(&b, "  %s\t(%s)\t%s", c.Detailed, c.Primary, c.Description)
		if note := canonical.ModelNote(c.Detailed); note != "" {
			fmt.Fprintf(&b, "\tNote: %s", note)
		}
		b.WriteString("\n")
	}
	deltas := fam.deltaCategories()
	names := make([]string, 0, len(deltas))
	for _, d := range deltas {
		names = append(names, d.Detailed)
	}
	fmt.Fprintf(&b, `
Emit nothing outside that list. In particular NEVER emit %s: those are assigned elsewhere from information you do not have, and emitting one will be rejected.

`, strings.Join(names, ", "))
	if len(anchors) > 0 {
		fmt.Fprintf(&b, "Reference examples — %ss from this same dataset, already categorised. The INPUT row is the shape you are given; the OUTPUT row is the shape you must reply in.\n\nINPUT rows:\n", fam.counterparty)
		b.WriteString(formatAnchorSignatureCSV(anchors))
		b.WriteString("\nCORRECT OUTPUT rows (3 columns, this is the format your response must use):\n")
		b.WriteString(formatAnchorVerdictCSV(anchors))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s to categorise — %s_signature,transaction_count:\n",
		strings.ToUpper(fam.plural()[:1])+fam.plural()[1:], fam.counterparty)
	b.WriteString(formatCandidateSignatureCSV(candidates))

	if level == config.SpendContextDescriptor || level == config.SpendContextTransaction {
		if s := formatCandidateDescriptorCSV(candidates); s != "" {
			fmt.Fprintf(&b, "\nRaw statement narratives these signatures were folded from — %s_signature,narrative:\n", fam.counterparty)
			b.WriteString(s)
		}
	}
	if level == config.SpendContextTransaction {
		if s := formatCandidateTransactionCSV(candidates); s != "" {
			fmt.Fprintf(&b, "\nExample transactions — %s_signature,date,amount,currency,account_kind,nearby_signatures:\n", fam.counterparty)
			b.WriteString(s)
		}
	}

	// The output contract, in this family's nouns. fam.outputContract()
	// is the SAME string the gauntlet quotes when it rejects a row, so
	// the instruction and the complaint cannot disagree — an income
	// batch that asked for payer columns and then named merchant ones
	// three lines later told the model three different things at once.
	fmt.Fprintf(&b, `
Output format:
- CSV with columns: %[1]s
- No header row. Exactly three columns per row. One row per %[2]s you can confidently name.
- Double-quote any field containing a comma; backslash-escape inner quotes.
- %[2]s_signature must appear verbatim in the list above.
- %[2]s_name is the %[2]s's real-world name in normal casing (%[3]q, not %[4]q). Never repeat the signature unchanged.
- %[5]s must be one of the taxonomy values listed above.

Skip any signature you cannot confidently place. A skipped row costs nothing; a guessed one is invisible in a report and wrong forever.

Do not include explanatory prose. Output CSV only.
`, fam.outputContract(), fam.counterparty, fam.nameExample, strings.ToUpper(fam.nameExample), fam.valueColumn)
	if len(feedback) > 0 {
		b.WriteString("\nYour previous response contained the following rows that I rejected:\n")
		for _, iv := range feedback {
			fmt.Fprintf(&b, "  %v — reason: %s\n", iv.Raw, iv.Reason)
		}
		b.WriteString("\nRe-emit your full response with those invalid rows dropped or corrected. Keep the rows that WERE valid; do not add new invalid ones. Output CSV only.\n")
	}
	return b.String()
}

func formatAnchorSignatureCSV(anchors []merchantAnchor) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		_ = w.Write([]string{a.Signature})
	}
	w.Flush()
	return b.String()
}

func formatAnchorVerdictCSV(anchors []merchantAnchor) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		_ = w.Write([]string{a.Signature, a.Name, a.Detailed})
	}
	w.Flush()
	return b.String()
}

func formatCandidateSignatureCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, c := range cands {
		_ = w.Write([]string{c.Signature, fmt.Sprintf("%d", c.Txns)})
	}
	w.Flush()
	return b.String()
}

func formatCandidateDescriptorCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	n := 0
	for _, c := range cands {
		for _, s := range c.Samples {
			if s.Descriptor == "" {
				continue
			}
			_ = w.Write([]string{c.Signature, s.Descriptor})
			n++
		}
	}
	w.Flush()
	if n == 0 {
		return ""
	}
	return b.String()
}

func formatCandidateTransactionCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	n := 0
	for _, c := range cands {
		for _, s := range c.Samples {
			_ = w.Write([]string{
				c.Signature,
				formatEpochDay(s.Day),
				fmt.Sprintf("%.2f", s.Amount),
				s.Currency,
				s.AccountKind,
				strings.Join(s.Neighbours, " | "),
			})
			n++
		}
	}
	w.Flush()
	if n == 0 {
		return ""
	}
	return b.String()
}

func formatEpochDay(day int64) string {
	return time.Unix(day*gold.SecondsPerDay, 0).UTC().Format("2006-01-02")
}

// ---- persistence -------------------------------------------------------------

// verdictStore is a run's write path. Verdicts reach gold as each batch
// completes, not when the run ends: a run that dies at batch 30 of 50 has
// kept 29 batches' worth of paid answers, and a re-run asks only about the
// rest, because the backlog excludes every signature the store answers.
//
// A write that fails holds its verdicts rather than dropping them — an
// answer already paid for is never lost to a transient open failure — so
// the next flush, or the end-of-run retryFlush, writes them.
type verdictStore struct {
	// write persists one flush's verdicts and reports the merchant
	// store's row count afterwards. A field, not a call to
	// persistCategorizations, so the bookkeeping above it is exercisable
	// without a gold handle.
	write func([]categorization) (int, error)
	warn  io.Writer

	pending   []categorization // accepted, not yet written
	held      int              // batches accepted, not yet written
	stored    int              // verdicts that reached gold
	completed int              // batches whose verdicts reached gold
	total     int              // merchant-store rows after the last write
}

// accept is the batchSink: it takes one batch's verdicts and writes
// everything outstanding. A failed write is reported and swallowed, since
// the run's remaining batches are still worth asking for.
func (s *verdictStore) accept(o batchOutcome) error {
	s.pending = append(s.pending, o.Accepted...)
	s.held++
	if err := s.flush(); err != nil {
		fmt.Fprintf(s.warn, "categorize: %d verdict(s) held after a failed store (%s); "+
			"they are retried before the run ends\n", len(s.pending), err.Error())
	}
	return nil
}

// flush writes the outstanding verdicts. A batch that accepted nothing
// still completes — there is nothing to write and nothing to hold.
func (s *verdictStore) flush() error {
	if s.held == 0 {
		return nil
	}
	if len(s.pending) > 0 {
		total, err := s.write(s.pending)
		if err != nil {
			return err
		}
		s.stored += len(s.pending)
		s.total = total
		s.pending = nil
	}
	s.completed += s.held
	s.held = 0
	return nil
}

// persistCategorizations upserts the verdicts into the family's global
// verdict store and returns the table's row count afterwards. The store is
// keyed by signature alone, so a re-run with a better model simply
// overwrites; signature_version stamps the normalisation that produced
// the key, which is what lets a later bump carry the verdict forward.
func persistCategorizations(ctx context.Context, db *sql.DB, fam categorizeFamily, rows []categorization, assignedAt int64, modelName string) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("categorize: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO `+fam.storeTable+`
    (`+fam.signatureColumn+`, `+fam.storeNameColumn+`, `+fam.valueColumn+`, signature_version, assigned_at, model_name)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (`+fam.signatureColumn+`) DO UPDATE SET
    `+fam.storeNameColumn+` = EXCLUDED.`+fam.storeNameColumn+`,
    `+fam.valueColumn+`     = EXCLUDED.`+fam.valueColumn+`,
    signature_version       = EXCLUDED.signature_version,
    assigned_at             = EXCLUDED.assigned_at,
    model_name              = EXCLUDED.model_name`)
	if err != nil {
		return 0, fmt.Errorf("categorize: prepare upsert: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.Signature, r.MerchantName, r.Detailed,
			spending.SignatureVersion, assignedAt, modelName); err != nil {
			return 0, fmt.Errorf("categorize: upsert %q: %w", r.Signature, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("categorize: commit: %w", err)
	}
	committed = true

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+fam.storeTable).Scan(&total); err != nil {
		return 0, fmt.Errorf("categorize: count the %s store: %w", fam.counterparty, err)
	}
	return total, nil
}

// ---- usage --------------------------------------------------------------------

func categorizeUsage() string {
	return `usage: wealthdb categorize [spending | income] [-n | --dry-run] [--batch N] [--max-attempts N] [--max-anchors N] [--show-prompt] [--all | --refine]

Categorise the merchants and payers the deterministic tiers could not
place, using the LLM configured in wealthdb.cfg's
"<family>.categorization.model" block. The positional selects one
family; with none, BOTH run in order — spending, then income — as two
plans and two summaries against one model. An absent
"income.categorization" inherits "spending.categorization" whole.

Work is priced PER SIGNATURE, not per transaction, and the verdicts
land in a global store — spend_merchant_categories for spending,
income_payer_categories for income — so a merchant met by several
cards, or a payer paying into several accounts, is asked about once
and answered once. The read path picks the verdicts up through that
family's report macros; the base transactions table is not touched.

A normal run re-asserts every deterministic verdict first (the same
pass 'load' runs), then asks about what is left. A --dry-run opens
gold read-only, so it cannot run that pass: its plan is computed
against the enrichment as of the LAST LOAD and is labelled as such.

The candidates are sent in batches of --batch signatures, each call
carrying the taxonomy and the anchor examples. Every batch retries on
its own feedback, and its accepted verdicts are stored as it
completes — a run that dies part-way has kept every batch before the
failure, and a re-run asks only about what is still unanswered. The
batch plan (count, sizes, anchors, first-prompt size) prints before
the first call, on a dry run too.

Four gates keep a signature out of candidacy, at every context level,
and each prints its count with the plan:

  - money moving between accounts or between people — wires, P2P
    rails, standing orders, anything carrying an IBAN or a masked
    contact number;
  - a signature that IS a bare person's name, on a non-card account,
    unless spending.categorization.fence_person_names is false. It
    cannot tell a person from a two-word company, so it refuses both,
    and the run report says when it is off;
  - a signature with no word in it — a bare bank booking code, a
    two-digit number;
  - a signature that is nothing but the bank's own booking type, which
    names how the row was booked and not whom it paid.

On the income side, candidacy is further restricted to deposit rows:
every other admitted kind is answered by its own transaction kind, and
a model asked about one could only be wrong.

The run report ends, per family, by listing every signature still
uncategorised, one per line. A signature is a fold of the raw statement
narrative and can carry a counterparty's name or an address — on an
inbound wire it usually IS a person — and this listing has no -p to
mask it: treat the report as narrative data, not as a summary safe to
paste.

Flags:
  -n, --dry-run         print the categorisation plan, don't write
      --max-attempts N  retry the LLM up to N times when responses
                        contain invalid rows (default 3)
      --max-anchors N   cap the in-context anchor examples (default 30)
      --batch N         signatures per model call (default 40: what a
                        local model answers well inside the 5-minute
                        call timeout; must be at least 1)
      --show-prompt     print the first batch's prompt to stderr in full;
                        later batches print only its size (debugging)
      --all             re-ask every signature candidacy admits, including
                        the ones already answered in the verdict store. It
                        lifts the backlog filter only: the fence still
                        fences, and on income candidacy is still deposits
                        alone, so it never reaches a kind-placed row
      --refine          re-ask only the counterparties the model itself
                        could place no better than a catch-all. The narrow
                        re-ask: a catch-all a RULE or a PIN placed is a
                        considered decision and is never disturbed

The flags may appear before or after the family positional.`
}
