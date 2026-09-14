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

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/pathmode"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

func init() {
	register("categorize", cmdCategorize)
}

// The spending model tier.
//
// Everything the deterministic pass could decide it has already
// decided by the time this runs — the matcher has paired the
// own-account moves, the built-in rules have placed the card payments
// and the ATM withdrawals, the provider map has translated whatever
// the issuer published. What is left is the long tail of merchants
// whose category follows from nothing but their NAME, and a model is
// the only thing that knows what a name like "Blue Harbour Hardware"
// sells.
//
// A verdict is bought PER MERCHANT SIGNATURE and stored globally, so a
// merchant met on several accounts, at several sources, is paid for
// once and answered once. That is the whole reason the enrichment pass
// records a signature even for rows it cannot categorise: the signature
// is the unit of work here.
//
// WHAT NEVER LEAVES THE MACHINE is decided in two independent places,
// and neither can be turned off by the other. The context level
// (spending.categorization.context) decides how much of a candidate
// is described; the transfer fence decides whether a signature is a
// candidate AT ALL, at every level. A wire, a P2P payment, a standing
// order — anything whose narrative carries a person rather than a
// merchant — is fenced out of candidacy and is therefore never
// described at any level.
//
// The fence is read over the ROW (spending.RowTransferShaped): the
// signature, the provider's own filing of it and the narrative half of
// the description. A reduction can drop a rail the row still carries —
// a mobile person-to-person rail leads the narrative and names no
// payee, so a reduction that prefers the counterparty leaves the
// payee's name alone as the key — and a key read on its own would
// admit exactly the shape the fence exists to stop.
//
// A third gate, spending.Uninformative, is about waste rather than
// privacy: a signature with no word in it — a bare bank code, a
// two-digit number — carries nothing to name, so a model can only echo
// it back and the gauntlet can only reject the echo. It is refused at
// candidacy beside the fence, counted beside the fenced count, and
// never costs a round-trip. spending.FilingOnly is the same refusal
// for a signature that is nothing but the provider's own booking type
// — the bank filed the row and wrote nothing else — which names how
// the row was booked, not whom it paid; it is counted with the
// uninformative.

// Flag defaults. --max-attempts and --max-anchors mirror
// resolve-symbols so the two LLM commands behave the same way under
// the same flags. --batch is this command's own: a merchant backlog
// runs to thousands of signatures where a symbol backlog runs to
// dozens, and the precedent has never needed to cut its set.
const (
	defaultCategorizeMaxAttempts = 3
	defaultCategorizeMaxAnchors  = 30

	// defaultCategorizeBatch is how many merchant signatures one model
	// call carries. A backlog sent whole in a single call dies on the
	// transport's five-minute ceiling: no local model produces a CSV that
	// size inside it, and none should be asked to. At forty, a model
	// served locally answers in well under a minute — the taxonomy and
	// anchor blocks dominate the prompt and are paid once per batch
	// regardless of its size, while the answer is forty short rows — so
	// the ceiling goes back to being a guard against a wedged serve
	// rather than something an ordinary call can reach.
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
	Uninformative int // nothing to name: no word at all, or nothing but the provider's own filing
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

// categorization is one validated verdict, ready to upsert into
// spend_merchant_categories.
type categorization struct {
	Signature    string
	MerchantName string
	Detailed     string
}

// cmdCategorize asks the configured model for a merchant name and a
// spend category for every merchant signature the deterministic tiers
// could not place, and stores the verdicts in the global merchant
// store. The read path picks them up through
// spending_lines_base's COALESCE(transaction scope, merchant scope).
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
		"merchant signatures per model call; the default keeps a local model's answer well inside the 5-minute call timeout")
	showPrompt := fs.Bool("show-prompt", false, "print the first batch's prompt to stderr in full; later batches only its size (debugging)")
	all := fs.Bool("all", false, "re-ask every signature candidacy admits, including ones already in the merchant store")
	refine := fs.Bool("refine", false, "re-ask only the merchants the model itself could place no better than a catch-all")
	fs.Usage = func() {
		fmt.Fprintln(stderr, categorizeUsage())
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "categorize: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "categorize: unexpected positional argument %q", fs.Arg(0))
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
	cz := cfg.SpendCategorization()
	const modelKey = "spending.categorization.model"
	modelCfg := cz.CategorizationModel()
	if modelCfg == nil {
		return errs.Newf(2, "categorize: %s is not set; add a `%s` block to %s", modelKey, modelKey, g.ConfigPath)
	}
	if err := validateModelConfig(modelKey, modelCfg); err != nil {
		return errs.Newf(2, "categorize: %s", err.Error())
	}
	level := cz.ContextLevel()

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
	if *dryRun {
		openMode = gold.ModeReadOnly
	} else {
		// A real run holds the gold write mutex end to end. The
		// verdicts it buys are the one thing in gold with no other
		// source of truth, and 'compact' / 'reload -a' carry the
		// merchant store by a read taken at the start of a rebuild
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
	if *dryRun {
		fmt.Fprintln(stdout, "categorize: dry-run — gold opened read-only, so the deterministic pass did NOT run.")
		fmt.Fprintln(stdout, "categorize: the candidate set below is AS OF THE LAST LOAD; a real run re-asserts it first.")
	} else if err := runEnrichmentPass(ctx, db, cfg, stdout); err != nil {
		return err
	}

	candidates, skipped, err := collectMerchantCandidates(ctx, db, level, cz.Samples(), backlogOf(*all, *refine))
	if err != nil {
		return err
	}

	canaries, err := collectSpendCanaries(ctx, db, cfg)
	if err != nil {
		return err
	}

	if len(candidates) == 0 {
		fmt.Fprintf(stdout, "categorize: nothing to categorise (%d signature(s) fenced as transfer-shaped, %d uninformative)\n",
			skipped.Fenced, skipped.Uninformative)
		printSpendCanaries(stdout, canaries)
		return nil
	}

	anchors, err := collectMerchantAnchors(ctx, db, *maxAnchors, candidateSignatures(candidates))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "categorize: %d merchant(s) over %d transaction(s), %d anchor(s), context %q, model %s\n",
		len(candidates), totalCandidateTxns(candidates), len(anchors), level, modelCfg.Name)
	printNeverSent(stdout, skipped)

	batches := splitBatches(candidates, *batch)
	printCategorizeBatchPlan(stdout, batches, *batch, anchors, level, *maxAttempts, *dryRun)

	// Everything the pass and the candidate collection needed is read;
	// release the handle before the model round-trips. DuckDB is one
	// read-write handle OR many read-only ones, so holding it across a
	// run of N batches at up to five minutes each would lock every
	// reader out of gold for the whole run. The write mutex above
	// still excludes other writers.
	if err := db.Close(); err != nil {
		return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("categorize: close gold before the model pass: %w", err))
	}
	dbOpen = false

	store := &verdictStore{warn: stderr, write: func(rows []categorization) (int, error) {
		wdb, err := gold.ReopenReadWrite(cfg.GoldDB)
		if err != nil {
			return 0, err
		}
		defer wdb.Close()
		return persistCategorizations(ctx, wdb, rows, time.Now().Unix(), modelCfg.Name)
	}}
	sink := store.accept
	if *dryRun {
		sink = func(batchOutcome) error { return nil }
	}
	valid, calls, totalInvalid, runErr := categorizeWithLLM(ctx, modelCaller(modelCfg), batches, anchors,
		level, *maxAttempts, *maxAnchors, *showPrompt, sink, stdout, stderr)
	flushErr := retryFlush(ctx, store.flush)
	if runErr != nil {
		if *dryRun {
			fmt.Fprintln(stdout, "categorize: stopped; nothing stored (dry run)")
		} else {
			// A run that both stopped and failed its last flush has
			// lost verdicts the model was paid for. That is a second,
			// independent failure and is reported as one — the stopped
			// line below only counts what did reach gold.
			if flushErr != nil {
				fmt.Fprintf(stderr, "categorize: %d verdict(s) could not be stored: %v\n",
					len(store.pending), flushErr)
			}
			fmt.Fprintf(stdout, "categorize: stopped; %d verdict(s) from %d completed batch(es) are already stored — re-run to continue with the rest\n",
				store.stored, store.completed)
		}
		return runErr
	}
	if flushErr != nil {
		return fmt.Errorf("categorize: %d verdict(s) could not be stored; re-run to ask for them again: %w",
			len(store.pending), flushErr)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Signature < valid[j].Signature })

	leftovers := uncategorisedCandidates(candidates, valid)
	printCategorizeSummary(stdout, candidates, valid, leftovers, len(batches), calls, totalInvalid, canaries)

	if *dryRun {
		fmt.Fprintln(stdout, "--- dry-run plan (no rows written; candidates as of the last load) ---")
		for _, v := range valid {
			fmt.Fprintf(stdout, "  %s → %s [%s]\n", v.Signature, v.MerchantName, v.Detailed)
		}
		return nil
	}
	if store.stored == 0 {
		fmt.Fprintln(stdout, "categorize: no valid verdicts to persist")
		return nil
	}
	fmt.Fprintf(stdout, "categorize: %d verdict(s) upserted over %d batch(es); total spend_merchant_categories rows now %d\n",
		store.stored, len(batches), store.total)
	return nil
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
// DEFAULT: every merchant signature with at least one row whose
// RESOLVED category is still NULL — read from spend_txn_categories(),
// the lattice's one definition (SPENDING.md §3), so this command
// cannot drift from what a report would call categorised. A resolved
// category covers both halves at once: a signature the store already
// answers is paid for, and a signature every one of whose rows a rule
// or the provider map placed has nothing left to ask about.
//
// --all: every signature in the spending population, store row or not,
// deterministic verdict or not. That is what re-asks a merchant after
// a taxonomy revision or a model change.
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
func collectMerchantCandidates(ctx context.Context, db *sql.DB, level string, samples int, which backlog) ([]merchantCandidate, skippedSignatures, error) {
	backlogFilter := `
   AND c.spend_detailed IS NULL`
	switch which {
	case backlogAll:
		backlogFilter = ""
	case backlogRefine:
		backlogFilter = `
   AND c.provenance = 'model'
   AND EXISTS (SELECT 1 FROM spend_categories sc
                WHERE sc.spend_detailed = c.spend_detailed AND sc.catch_all)`
	}
	q := `
SELECT c.merchant_signature,
       p.silver_source_id, p.account_kind, p.occurred_at, p.currency,
       COALESCE(CAST(p.net_amount AS DOUBLE), 0),
       COALESCE(p.counterparty, ''), COALESCE(p.description, ''),
       COALESCE(p.provider_category, '')
  FROM spend_txn_categories() c
  JOIN spend_enrichment_population(?, ?) p
    ON p.silver_source_id        = c.silver_source_id
   AND p.transaction_external_id = c.transaction_external_id
 WHERE c.merchant_signature IS NOT NULL
   AND c.merchant_signature <> ''` + backlogFilter + `
 ORDER BY c.merchant_signature, p.occurred_at, p.transaction_external_id`

	// The row fence, read once over the whole population rather than
	// per row here. Two reasons it cannot be a test on the row in
	// hand: this scan sees only the backlog by default, so a signature
	// carried by both a placed row and an unplaced one would be
	// admitted on the unplaced row while the rail sits on the placed
	// one the query never reaches; and even over the same rows, a key
	// admitted from a clean row before its fenced sibling arrives
	// would already be in the candidate set.
	rowFenced, err := rowFencedSignatures(ctx, db)
	if err != nil {
		return nil, skippedSignatures{}, err
	}

	rows, err := db.QueryContext(ctx, q, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, skippedSignatures{}, fmt.Errorf("categorize: read merchant candidates: %w", err)
	}
	defer rows.Close()

	bySig := map[string]*merchantCandidate{}
	fencedSigs := map[string]bool{}
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
			return nil, skippedSignatures{}, fmt.Errorf("categorize: scan merchant candidate: %w", err)
		}
		// The fence gates candidacy, identically at every context
		// level. A person-bearing narrative is not a merchant, and the
		// reading is over the whole row (rowFencedSignatures): a rail
		// the reduction dropped is still written in the provider's
		// filing and in the raw narrative.
		if rowFenced[sig] {
			fencedSigs[sig] = true
			continue
		}
		// So does the word gate, at the same place and every level.
		// A bare code has nothing to name; the model would only echo
		// it and the gauntlet would only reject the echo. Neither has
		// a signature that is nothing but the bank's own booking type
		// — the bank filed the row and wrote nothing else — which
		// names how the row was booked, not whom it paid, and would
		// buy one verdict for every row filed that way.
		if spending.Uninformative(sig) || spending.FilingOnly(sig, providerCategory) {
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
		if err := attachNeighbours(ctx, db, out, rowFenced); err != nil {
			return nil, skippedSignatures{}, err
		}
	}
	return out, skippedSignatures{Fenced: len(fencedSigs), Uninformative: len(uninformativeSigs)}, nil
}

// printNeverSent reports what candidacy refused, one line per gate
// that fired. It prints beside the plan, so why a
// row will stay uncategorised before deciding to wait on the run.
func printNeverSent(w io.Writer, skipped skippedSignatures) {
	if skipped.Fenced > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) fenced as transfer-shaped and never sent\n", skipped.Fenced)
	}
	if skipped.Uninformative > 0 {
		fmt.Fprintf(w, "categorize: %d signature(s) uninformative and never sent\n", skipped.Uninformative)
	}
}

// rowFencedSignatures reads the transfer fence over the WHOLE spending
// population — every row, its provider filing and its narrative — and
// returns every signature at least one row fences
// (spending.RowTransferShaped).
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
// The population read here is exactly the one the candidate scan and
// the neighbour scan read, so a single pass covers both rather than
// each site re-testing its own rows and missing the rails written on
// the rows it does not see.
func rowFencedSignatures(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
SELECT e.merchant_signature,
       COALESCE(p.provider_category, ''), COALESCE(p.description, '')
  FROM spend_enrichment_population(?, ?) p
  JOIN spend_txn_enrichment e
    ON e.silver_source_id        = p.silver_source_id
   AND e.transaction_external_id = p.transaction_external_id
 WHERE e.merchant_signature IS NOT NULL
   AND e.merchant_signature <> ''`, int64(0), gold.MaxEpoch)
	if err != nil {
		return nil, fmt.Errorf("categorize: read the row fence: %w", err)
	}
	defer rows.Close()
	fenced := map[string]bool{}
	for rows.Next() {
		var sig, providerCategory, description string
		if err := rows.Scan(&sig, &providerCategory, &description); err != nil {
			return nil, fmt.Errorf("categorize: scan the row fence: %w", err)
		}
		if fenced[sig] {
			continue
		}
		if spending.RowTransferShaped(sig, providerCategory, description) {
			fenced[sig] = true
		}
	}
	return fenced, rows.Err()
}

// attachNeighbours fills in the nearby-transaction context the
// `transaction` level sends: for each sample, the signatures of what
// else was bought on the same source within a day. It is what lets a
// model place an otherwise opaque merchant from its company — a name
// that means nothing between an airline and a hotel means something.
//
// Neighbours are fenced like everything else — on the row, through the
// set rowFencedSignatures read over this same population, so a rail
// written only in a neighbour's provider filing or raw narrative
// fences it here too, and the key-only reading needs no second call:
// the set already refuses everything it would. A neighbour equal to
// the sample's own signature is dropped as uninformative.
func attachNeighbours(ctx context.Context, db *sql.DB, cands []merchantCandidate, rowFenced map[string]bool) error {
	type dayedSig struct {
		day int64
		sig string
	}
	rows, err := db.QueryContext(ctx, `
SELECT p.silver_source_id, p.occurred_at, e.merchant_signature
  FROM spend_enrichment_population(?, ?) p
  JOIN spend_txn_enrichment e
    ON e.silver_source_id        = p.silver_source_id
   AND e.transaction_external_id = p.transaction_external_id
 WHERE e.merchant_signature IS NOT NULL
   AND e.merchant_signature <> ''
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
		if rowFenced[sig] {
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
func collectMerchantAnchors(ctx context.Context, db *sql.DB, maxAnchors int, exclude map[string]bool) ([]merchantAnchor, error) {
	if maxAnchors <= 0 {
		return nil, nil
	}
	rowFenced, err := rowFencedSignatures(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT merchant_signature, merchant_name, spend_detailed
  FROM spend_merchant_categories
 ORDER BY assigned_at DESC, merchant_signature`)
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
		if exclude[a.Signature] || !canonical.ModelSpendDetailed(a.Detailed) {
			continue
		}
		if rowFenced[a.Signature] || spending.TransferShaped(a.Signature) {
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

// splitBatches cuts the candidate set into consecutive runs of at most
// size. Candidates arrive sorted by signature, so a batch is an
// alphabetical slice — which is fine, because every batch carries the
// whole taxonomy and its own anchors, and nothing in a batch depends
// on the candidates outside it.
func splitBatches(cands []merchantCandidate, size int) [][]merchantCandidate {
	if size < 1 {
		size = 1
	}
	var out [][]merchantCandidate
	for start := 0; start < len(cands); start += size {
		end := start + size
		if end > len(cands) {
			end = len(cands)
		}
		out = append(out, cands[start:end])
	}
	return out
}

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
func printCategorizeBatchPlan(w io.Writer, batches [][]merchantCandidate, size int, anchors []merchantAnchor, level string, maxAttempts int, dryRun bool) {
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
	first := buildCategorizeUserPrompt(batches[0], anchors, level, nil)
	fmt.Fprintln(w, "categorize: plan")
	fmt.Fprintf(w, "  merchants:      %d in %d batch(es) of up to %d (--batch)\n", n, len(batches), size)
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
		o.Accepted, o.Attempts, o.Rejected, err = categorizeBatch(ctx, call, batch, anchors, level,
			o.Index, o.Count, maxAttempts, showPrompt, stdout, stderr)
		calls += o.Attempts
		totalInvalid += o.Rejected
		if err != nil {
			return validUnion, calls, totalInvalid, fmt.Errorf("batch %d/%d: %w", o.Index, o.Count, err)
		}
		fmt.Fprintf(stdout, "categorize: batch %d/%d: %d merchant(s), %d accepted, %d rejected, %d attempt(s)\n",
			o.Index, o.Count, o.Size, len(o.Accepted), o.Rejected, o.Attempts)
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

	systemPrompt := buildCategorizeSystemPrompt()
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		userPrompt := buildCategorizeUserPrompt(batch, anchors, level, lastInvalid)
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

		fresh, invalid := parseAndValidateCategorizations(body, candSet)
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
func parseAndValidateCategorizations(body string, candSet map[string]bool) ([]categorization, []invalidRow) {
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
				Reason: fmt.Sprintf("expected exactly 3 columns (merchant_signature,merchant_name,spend_detailed), got %d", len(row))})
			continue
		}
		signature := strings.TrimSpace(row[0])
		name := strings.TrimSpace(row[1])
		category := strings.TrimSpace(row[2])

		if !candSet[signature] {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("merchant_signature %q was not in the candidate set", signature)})
			continue
		}
		if name == "" {
			invalid = append(invalid, invalidRow{Raw: row, Reason: "merchant_name is empty"})
			continue
		}
		if name == signature {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("merchant_name %q is the signature verbatim (model echoed input)", name)})
			continue
		}
		if isDeltaSpendCategory(category) {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("spend_detailed %q is assigned by the matcher and the rule tier, never by a model", category)})
			continue
		}
		detailed := strings.ToUpper(category)
		if !canonical.ModelSpendDetailed(detailed) {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("spend_detailed %q is not a value of the taxonomy", category)})
			continue
		}
		valid = append(valid, categorization{Signature: signature, MerchantName: name, Detailed: detailed})
	}
	return valid, invalid
}

// isDeltaSpendCategory reports whether s names one of the delta
// values. It is derived rather than restated: a value the taxonomy
// recognises but the vendored subset does not IS a delta, so a delta
// added to canonical is refused here without this file changing.
// Case is folded first — a model shouting INTERNAL_TRANSFER must get
// the specific rejection, not the generic one.
func isDeltaSpendCategory(s string) bool {
	folded := strings.ToLower(strings.TrimSpace(s))
	return canonical.ValidSpendDetailed(folded) && !canonical.ModelSpendDetailed(folded)
}

// ---- prompt assembly ---------------------------------------------------------

func buildCategorizeSystemPrompt() string {
	return `You are a personal-finance data assistant. You are given merchant signatures — short upper-cased fragments of card and bank statement narratives — and you name the merchant and pick its spending category from a fixed taxonomy. You output CSV only — no prose, no markdown, no explanations.`
}

// buildCategorizeUserPrompt assembles the per-attempt user message.
// The taxonomy comes first (it is the closed vocabulary the answer
// must come from), then the anchors (this deployment's own prior
// answers), then the candidates at whatever depth the context level
// allows, then the output contract, then any feedback from the last
// attempt.
func buildCategorizeUserPrompt(candidates []merchantCandidate, anchors []merchantAnchor, level string, feedback []invalidRow) string {
	var b strings.Builder
	b.WriteString(`Merchant signatures below come from card and bank statements. For each one, emit the merchant's real-world name and the single best category from the taxonomy.

Taxonomy — spend_detailed values you may emit, with the primary bucket each belongs to and what it covers:
`)
	for _, c := range canonical.ModelSpendCategories() {
		fmt.Fprintf(&b, "  %s\t(%s)\t%s\n", c.Detailed, c.Primary, c.Description)
	}
	deltas := canonical.DeltaSpendCategories()
	names := make([]string, 0, len(deltas))
	for _, d := range deltas {
		names = append(names, d.Detailed)
	}
	fmt.Fprintf(&b, `
Emit nothing outside that list. In particular NEVER emit %s: those are assigned elsewhere from information you do not have, and emitting one will be rejected.

`, strings.Join(names, ", "))
	if len(anchors) > 0 {
		b.WriteString("Reference examples — merchants from this same dataset, already categorised. The INPUT row is the shape you are given; the OUTPUT row is the shape you must reply in.\n\nINPUT rows:\n")
		b.WriteString(formatAnchorSignatureCSV(anchors))
		b.WriteString("\nCORRECT OUTPUT rows (3 columns, this is the format your response must use):\n")
		b.WriteString(formatAnchorVerdictCSV(anchors))
		b.WriteString("\n")
	}
	b.WriteString("Merchants to categorise — merchant_signature,transaction_count:\n")
	b.WriteString(formatCandidateSignatureCSV(candidates))

	if level == config.SpendContextDescriptor || level == config.SpendContextTransaction {
		if s := formatCandidateDescriptorCSV(candidates); s != "" {
			b.WriteString("\nRaw statement narratives these signatures were folded from — merchant_signature,narrative:\n")
			b.WriteString(s)
		}
	}
	if level == config.SpendContextTransaction {
		if s := formatCandidateTransactionCSV(candidates); s != "" {
			b.WriteString("\nExample transactions — merchant_signature,date,amount,currency,account_kind,nearby_signatures:\n")
			b.WriteString(s)
		}
	}

	b.WriteString(`
Output format:
- CSV with columns: merchant_signature,merchant_name,spend_detailed
- No header row. Exactly three columns per row. One row per merchant you can confidently name.
- Double-quote any field containing a comma; backslash-escape inner quotes.
- merchant_signature must appear verbatim in the list above.
- merchant_name is the merchant's real-world name in normal casing ("Corner Market", not "CORNER MARKET"). Never repeat the signature unchanged.
- spend_detailed must be one of the taxonomy values listed above.

Skip any signature you cannot confidently place. A skipped row costs nothing; a guessed one is invisible in a report and wrong forever.

Do not include explanatory prose. Output CSV only.
`)
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

// persistCategorizations upserts the verdicts into the global merchant
// store and returns the table's row count afterwards. The store is
// keyed by signature alone, so a re-run with a better model simply
// overwrites; signature_version stamps the normalisation that produced
// the key, which is what lets a later bump carry the verdict forward.
func persistCategorizations(ctx context.Context, db *sql.DB, rows []categorization, assignedAt int64, modelName string) (int, error) {
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
INSERT INTO spend_merchant_categories
    (merchant_signature, merchant_name, spend_detailed, signature_version, assigned_at, model_name)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (merchant_signature) DO UPDATE SET
    merchant_name     = EXCLUDED.merchant_name,
    spend_detailed    = EXCLUDED.spend_detailed,
    signature_version = EXCLUDED.signature_version,
    assigned_at       = EXCLUDED.assigned_at,
    model_name        = EXCLUDED.model_name`)
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&total); err != nil {
		return 0, fmt.Errorf("categorize: count merchant store: %w", err)
	}
	return total, nil
}

// ---- usage --------------------------------------------------------------------

func categorizeUsage() string {
	return `usage: wealthdb categorize [-n | --dry-run] [--batch N] [--max-attempts N] [--max-anchors N] [--show-prompt] [--all | --refine]

Categorise the merchants the deterministic spending tiers could not
place, using the LLM configured in wealthdb.cfg's
"spending.categorization.model" block.

Work is priced PER MERCHANT SIGNATURE, not per transaction, and the
verdicts land in the global spend_merchant_categories table — a
merchant met by several cards is asked about once and answered once.
The read path picks the verdicts up through the spending report
macros; the base transactions table is not touched.

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

Signatures whose narrative looks like money moving between accounts or
between people — wires, P2P rails, standing orders, anything carrying
an IBAN — are fenced out of candidacy and are never sent, at any
context level. Neither is a signature with no word in it — a bare
bank booking code, a two-digit number — nor one that is nothing but
the bank's own booking type; neither carries anything to name. Both
counts print with the plan.

The run report ends by listing every signature still uncategorised,
one per line. A signature is a fold of the raw statement narrative
and can carry a payee's name or an address, and this listing has no
-p to mask it: treat the report as narrative data, not as a summary
safe to paste.

Flags:
  -n, --dry-run         print the categorisation plan, don't write
      --max-attempts N  retry the LLM up to N times when responses
                        contain invalid rows (default 3)
      --max-anchors N   cap the in-context anchor examples (default 30)
      --batch N         merchant signatures per model call (default 40:
                        what a local model answers well inside the
                        5-minute call timeout; must be at least 1)
      --show-prompt     print the first batch's prompt to stderr in full;
                        later batches print only its size (debugging)
      --all             re-ask every signature candidacy admits, including
                        the ones already answered in the merchant store
      --refine          re-ask only the merchants the model itself could
                        place no better than a catch-all. The narrow
                        re-ask: a catch-all a RULE or a PIN placed is a
                        considered decision and is never disturbed`
}
