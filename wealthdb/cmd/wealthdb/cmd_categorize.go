package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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

// defaultCategorizeBatch is how many merchant signatures one model
// call carries (see splitBatches). It is this command's own, because a
// merchant prompt carries the whole taxonomy. At forty, a model served
// locally answers in well under a minute — the taxonomy and anchor
// blocks dominate the prompt and are paid once per batch regardless of
// its size, while the answer is forty short rows.
const defaultCategorizeBatch = 40

// cmdCategorize asks the configured model for a counterparty's name
// and its category, for every signature the deterministic tiers left
// unplaced, and stores the verdicts in that family's global store. An
// optional positional selects one family; with none, both run in order.
// The read path picks the verdicts up through each family's resolution
// macro.
func cmdCategorize(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb categorize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var mf modelFlags
	registerModelFlags(fs, &mf, defaultCategorizeBatch)
	all := fs.Bool("all", false, "re-ask every signature candidacy admits, including ones already in the verdict store")
	refine := fs.Bool("refine", false, "re-ask only the counterparties the model itself could place no better than a catch-all")
	fs.Usage = func() {
		fmt.Fprintln(stderr, categorizeUsage())
	}
	if err := parseFlags(fs, subargs, "categorize"); err != nil {
		return err
	}
	families, err := categorizeFamilyArg(fs, "categorize")
	if err != nil {
		return err
	}
	if err := mf.check(fs, "categorize"); err != nil {
		return err
	}
	// Beside the other flag check, and before the pass takes the gold
	// write lock: a usage error must not cost a full overlay rewrite.
	if *all && *refine {
		return errs.Newf(errs.ExitUsage, "categorize: --all and --refine choose different backlogs; pass one")
	}

	cfg, err := g.loadConfig()
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
			return errs.Newf(errs.ExitUsage, "categorize: %s is not set; add a `%s` block to %s",
				modelKey, modelKey, g.ConfigPath)
		}
		if err := modelCfg.Check(modelKey); err != nil {
			return errs.Newf(errs.ExitUsage, "categorize: %s", err.Error())
		}
	}

	// A dry run is a PURE READ: it opens read-only, takes no write
	// lock, and a parallel `wealthdb transactions` can read the file
	// while the model is responding. A real run holds the gold write
	// mutex end to end. The verdicts it buys are the one thing in gold
	// with no other source of truth, and 'compact' / 'reload -a' carry
	// the verdict stores by a read taken at the start of a rebuild that
	// ends in a rename — anything stored after that read would be
	// swapped away and still reported as stored.
	openMode := gold.ModeReadWrite
	var in loadInputs
	if mf.dryRun {
		openMode = gold.ModeReadOnly
	} else if in, err = readLoadInputs(cfg); err != nil {
		// Only a real run stamps the config and applies the ledgers,
		// and it reads them before gold is opened read-write
		// (readLoadInputs).
		return err
	}
	db, lock, err := openGoldForOverlay(ctx, g, cfg.GoldDB, "categorize", defaultMissingGoldMsg, "see the plan", mf.dryRun)
	if err != nil {
		return err
	}
	defer lock.unlock()
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
	// in one transaction. The config stamp comes first, as on a load:
	// a rule's far account is a declared account, which reaches the
	// accounts dimension only through the stamp.
	if mf.dryRun {
		fmt.Fprintln(stdout, "categorize: dry-run — gold opened read-only, so the deterministic pass did NOT run.")
		fmt.Fprintln(stdout, "categorize: the candidate set below is AS OF THE LAST LOAD; a real run re-asserts it first.")
	} else if err := stampConfig(ctx, db, cfg, in, "categorize", stdout, stderr); err != nil {
		return err
	} else if err := runEnrichmentPass(ctx, db, cfg, in.enrichment, true, stdout); err != nil {
		return err
	}

	opts := categorizeRunOptions{modelFlags: mf, which: backlogOf(*all, *refine)}
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
	modelFlags
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
		if canaries.PairsNotStored {
			fmt.Fprintln(stderr, pairsNotStoredNote)
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

	// Verdicts are stored as each batch completes. A re-run of a run
	// that died asks only about the rest, because the backlog excludes
	// every signature the store answers. A dry run stores nothing.
	var store *batchStore[categorization]
	if !opts.dryRun {
		store = &batchStore[categorization]{cmd: "categorize", noun: "verdict", warn: stderr,
			write: func(rows []categorization) (int, error) {
				wdb, err := gold.ReopenReadWrite(cfg.GoldDB)
				if err != nil {
					return 0, err
				}
				defer wdb.Close()
				return persistCategorizations(ctx, wdb, fam, rows, time.Now().Unix(), modelCfg.Name)
			}}
	}
	call := opts.call
	if call == nil {
		call = modelCaller(modelCfg)
	}
	valid, calls, totalInvalid, runErr := categorizeWithLLM(ctx, call, fam, batches, anchors,
		level, opts.maxAttempts, opts.maxAnchors, opts.showPrompt, store, stdout, stderr)
	// The held verdicts are written whether or not the family's own run
	// stopped, and BEFORE the next family is started: a run of two
	// families must not leave the first one's last batch unwritten while
	// the second spends another model pass. A store that cannot be
	// written wins over the model's error, since its verdicts are lost
	// unless printed.
	if store != nil {
		if err := store.finish(ctx, func(held []categorization, err error) error {
			printCategorizationPlan(stdout,
				fmt.Sprintf("--- %d %s verdict(s) the model answered but gold would not accept ---", len(held), fam.name),
				held)
			return fmt.Errorf("categorize: could not store the %s verdicts above; re-run once nothing else "+
				"holds %q open, and the run asks for them again: %w", fam.name, cfg.GoldDB, err)
		}); err != nil {
			return closed, err
		}
	}
	if runErr != nil {
		if opts.dryRun {
			fmt.Fprintf(stdout, "categorize: stopped; nothing stored (dry run)\n")
		} else {
			fmt.Fprintf(stdout, "categorize: stopped; %d verdict(s) are already stored — re-run to continue with the rest\n",
				store.stored)
		}
		return closed, runErr
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Signature < valid[j].Signature })

	leftovers := uncategorisedCandidates(candidates, valid)
	printCategorizeSummary(stdout, fam, candidates, valid, leftovers, len(batches), calls, totalInvalid, canaries)

	if opts.dryRun {
		printCategorizationPlan(stdout,
			fmt.Sprintf("--- %s dry-run plan (no rows written; candidates as of the last load) ---", fam.name), valid)
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

// printCategorizationPlan renders verdicts one per line under a
// heading. One renderer for both the dry-run plan and the
// could-not-store fallback, so what a failed store leaves on stdout is
// exactly what a plan looks like.
func printCategorizationPlan(w io.Writer, heading string, rows []categorization) {
	fmt.Fprintln(w, heading)
	for _, v := range rows {
		fmt.Fprintf(w, "  %s → %s [%s]\n", v.Signature, v.MerchantName, v.Detailed)
	}
}

// ---- batching ----------------------------------------------------------------

// estimateTokens is the run's cost preview. Four characters per
// token is the usual rough ratio for English prose and CSV; the point
// is an order of magnitude before the first call, not a tokenizer.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// printCategorizeBatchPlan says what a run will cost before the first
// call is made: how many batches, how big, how many anchors the first
// carries, how large its prompt is, and how many calls the run can
// spend. On a long run the preview is what is read before deciding
// to wait. A dry run asks the model exactly as a real run does — that
// is the precedent's shape, and the only way to see verdicts without
// writing them — so there the plan is the one cost signal that
// arrives before any call.
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

// categorizeWithLLM runs the batches through the model in order
// (modelPass.run). Each batch is its own retry loop with its own
// feedback; its accepted verdicts go to store the moment the batch
// completes, and become the newest anchors for the batch after it. An
// endpoint error stops the run at that batch, and everything store
// already took stays taken. A nil store (a dry run) keeps nothing.
func categorizeWithLLM(
	ctx context.Context,
	call llmCall,
	fam categorizeFamily,
	batches [][]merchantCandidate,
	anchors []merchantAnchor,
	level string,
	maxAttempts, maxAnchors int,
	showPrompt bool,
	store *batchStore[categorization],
	stdout, stderr io.Writer,
) (validUnion []categorization, calls int, totalInvalid int, err error) {
	pass := modelPass[merchantCandidate, categorization]{
		system: fam.systemPrompt(),
		// The anchors are the batch's own: the run refreshes them once a
		// batch completes.
		prompt: func(batch []merchantCandidate, feedback []invalidRow) string {
			return buildCategorizeUserPrompt(fam, batch, anchors, level, feedback)
		},
		parse: func(body string, batch []merchantCandidate) ([]categorization, []invalidRow) {
			return parseAndValidateCategorizations(fam, body, candidateSignatures(batch))
		},
		key:         func(v categorization) string { return v.Signature },
		call:        call,
		maxAttempts: maxAttempts,
		stdout:      stdout,
		label: func(at batchAt, attempt int) string {
			return fmt.Sprintf("categorize: batch %d/%d attempt %d", at.index, at.count, attempt)
		},
	}
	if showPrompt {
		pass.showPrompt = func(at batchAt, attempt int, prompt string) {
			// The first batch's prompt in full; every later one is the
			// same shape with different rows, and fifty copies would
			// bury the one worth reading.
			if at.index == 1 {
				fmt.Fprintf(stderr, "--- LLM prompt (batch %d/%d, attempt %d) ---\n%s\n--- end prompt ---\n",
					at.index, at.count, attempt, prompt)
			} else {
				fmt.Fprintf(stderr, "--- LLM prompt (batch %d/%d, attempt %d): %d chars, ≈%d tokens ---\n",
					at.index, at.count, attempt, len(prompt), estimateTokens(prompt))
			}
		}
	}
	r, err := pass.run(ctx, batches, store, func(at batchAt, batch []merchantCandidate, accepted []categorization, attempts, rejected int) {
		fmt.Fprintf(stdout, "categorize: batch %d/%d: %d %s(s), %d accepted, %d rejected, %d attempt(s)\n",
			at.index, at.count, len(batch), fam.counterparty, len(accepted), rejected, attempts)
		anchors = refreshAnchors(anchors, accepted, maxAnchors)
	})
	if err != nil {
		err = fmt.Errorf("batch %d/%d: %w", r.stopped.index, r.stopped.count, err)
	}
	return r.answers, r.attempts, r.rejected, err
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
