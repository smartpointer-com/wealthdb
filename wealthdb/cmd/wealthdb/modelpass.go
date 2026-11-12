package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

// The model pass both model commands run: `categorize` asks for a
// counterparty's name and category per signature, `resolve-symbols`
// for an instrument's ticker. What a command brings is its prompt, its
// gauntlet and the key that makes two answers one; the retry loop, the
// store-per-batch policy and the shared flags are here, once.

// The model flags' defaults, the same for both commands so the two
// behave alike under the same flags. The batch size is each command's
// own: what one call can carry depends on what one row of it costs.
const (
	defaultModelMaxAttempts = 3
	defaultModelMaxAnchors  = 30
)

// modelFlags are the five flags every model command takes.
type modelFlags struct {
	dryRun      bool
	maxAttempts int
	maxAnchors  int
	batch       int
	showPrompt  bool
}

// modelFlagDocs are the descriptions a command registers its model
// flags under, in its own words.
type modelFlagDocs struct {
	dryRun, maxAttempts, maxAnchors, batch, showPrompt string
}

// registerModelFlags registers the five model flags on fs, with batch
// as the --batch default.
func registerModelFlags(fs *flag.FlagSet, o *modelFlags, batch int, docs modelFlagDocs) {
	fs.BoolVar(&o.dryRun, "n", false, docs.dryRun)
	fs.BoolVar(&o.dryRun, "dry-run", false, docs.dryRun)
	fs.IntVar(&o.maxAttempts, "max-attempts", defaultModelMaxAttempts, docs.maxAttempts)
	fs.IntVar(&o.maxAnchors, "max-anchors", defaultModelMaxAnchors, docs.maxAnchors)
	fs.IntVar(&o.batch, "batch", batch, docs.batch)
	fs.BoolVar(&o.showPrompt, "show-prompt", false, docs.showPrompt)
}

// batchAt is a batch's 1-based position in its run.
type batchAt struct {
	index, count int
}

// modelPass is one command's conversation with the model over batches
// of candidates C, each answer a V.
type modelPass[C, V any] struct {
	system string
	// prompt builds an attempt's user message: the batch, and the rows
	// the previous attempt rejected (none on the first).
	prompt func(batch []C, feedback []invalidRow) string
	// parse partitions a response body into answers that may be stored
	// and rows that may not, each with its reason.
	parse func(body string, batch []C) ([]V, []invalidRow)
	// key names what an answer is about. The first answer per key wins.
	key func(V) string

	call        llmCall
	maxAttempts int
	stdout      io.Writer

	// label leads every line an attempt prints.
	label func(at batchAt, attempt int) string
	// countResponses prints each response's size once its reasoning is
	// stripped.
	countResponses bool
	// showPrompt prints an attempt's prompt (--show-prompt); nil prints
	// nothing.
	showPrompt func(at batchAt, attempt int, prompt string)
}

// ask runs one batch through the model. On each attempt it parses the
// response, partitions it into valid answers and invalid rows, merges
// the valid ones into the running set (first answer per key wins), and
// re-prompts with targeted feedback while any invalid rows remain. The
// feedback names this batch's rows only: a bad row costs its own batch
// a round-trip, never the run. A failed call ends the batch with the
// answers accepted before it.
func (p modelPass[C, V]) ask(ctx context.Context, batch []C, at batchAt) (accepted []V, attempts int, rejected int, err error) {
	maxAttempts := max(p.maxAttempts, 1)
	seen := map[string]bool{}
	var lastInvalid []invalidRow
	for attempts = 1; attempts <= maxAttempts; attempts++ {
		userPrompt := p.prompt(batch, lastInvalid)
		if p.showPrompt != nil {
			p.showPrompt(at, attempts, userPrompt)
		}
		raw, callErr := p.call(ctx, p.system, userPrompt)
		if callErr != nil {
			return accepted, attempts, rejected, fmt.Errorf("LLM call (attempt %d): %w", attempts, callErr)
		}
		body := stripThinkingBlocks(raw)
		if p.countResponses {
			fmt.Fprintf(p.stdout, "%s: %d chars of CSV response (after stripping reasoning)\n",
				p.label(at, attempts), len(body))
		}

		fresh, invalid := p.parse(body, batch)
		rejected += len(invalid)
		for _, v := range fresh {
			k := p.key(v)
			if seen[k] {
				continue
			}
			seen[k] = true
			accepted = append(accepted, v)
		}
		if len(invalid) == 0 {
			return accepted, attempts, rejected, nil
		}
		fmt.Fprintf(p.stdout, "%s: %d invalid row(s) — sample:\n", p.label(at, attempts), len(invalid))
		printInvalidSample(p.stdout, invalid)
		if attempts == maxAttempts {
			break
		}
		lastInvalid = invalid
	}
	return accepted, attempts, rejected, nil
}

// batchStore is a model run's write path. Answers reach gold as each
// batch completes, not when the run ends: a run that dies at batch 30 of
// 50 has kept 29 batches' worth of paid answers, and a re-run asks only
// about the rest.
//
// A write that fails holds its answers rather than dropping them — an
// answer already paid for is never lost to a transient open failure — so
// the next batch's write, or the end-of-run retryFlush, writes them.
type batchStore[V any] struct {
	// write persists one flush's answers and reports the store's row
	// count afterwards. A field, so the bookkeeping below is exercisable
	// without a gold handle.
	write func([]V) (int, error)
	warn  io.Writer
	// cmd and noun word the warning a held write prints.
	cmd, noun string

	pending   []V // accepted, not yet written
	held      int // batches accepted, not yet written
	stored    int // answers that reached gold
	completed int // batches whose answers reached gold
	total     int // store rows after the last write
}

// accept takes one batch's answers and writes everything outstanding. A
// failed write is reported and swallowed, since the run's remaining
// batches are still worth asking for.
func (s *batchStore[V]) accept(rows []V) {
	s.pending = append(s.pending, rows...)
	s.held++
	if err := s.flush(); err != nil {
		fmt.Fprintf(s.warn, "%s: %d %s(s) held after a failed store (%s); "+
			"they are retried before the run ends\n", s.cmd, len(s.pending), s.noun, err.Error())
	}
}

// flush writes the outstanding answers. A batch that accepted nothing
// still completes — there is nothing to write and nothing to hold.
func (s *batchStore[V]) flush() error {
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
