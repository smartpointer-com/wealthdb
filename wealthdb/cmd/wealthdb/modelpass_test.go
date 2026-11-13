package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// ---- batchStore --------------------------------------------------------------

// recordingStore is a batchStore whose writes are captured instead of
// made, with `fail` deciding which write attempt fails.
func recordingStore(fail func(attempt int) error) (*batchStore[categorization], *[][]string, *bytes.Buffer) {
	var writes [][]string
	warn := &bytes.Buffer{}
	attempt := 0
	s := &batchStore[categorization]{warn: warn, cmd: "categorize", noun: "verdict"}
	s.write = func(rows []categorization) (int, error) {
		attempt++
		if err := fail(attempt); err != nil {
			return 0, err
		}
		var sigs []string
		for _, r := range rows {
			sigs = append(sigs, r.Signature)
		}
		writes = append(writes, sigs)
		return len(writes) * 10, nil // a stand-in row count that moves
	}
	return s, &writes, warn
}

func acceptedBatch(sigs ...string) []categorization {
	var out []categorization
	for _, s := range sigs {
		out = append(out, categorization{s, s, "FOOD_AND_DRINK_COFFEE"})
	}
	return out
}

// shortFlushLadder makes retryFlush retry at once for the rest of a
// test. Only a serial test may call it: flushLadder is package state.
func shortFlushLadder(t *testing.T) {
	t.Helper()
	saved := flushLadder
	flushLadder = []time.Duration{0, 0, 0}
	t.Cleanup(func() { flushLadder = saved })
}

// TestBatchStoreWritesEachBatchAsItCompletes pins why the write is not
// deferred to the end of the run: a run that dies partway keeps every
// batch that finished.
func TestBatchStoreWritesEachBatchAsItCompletes(t *testing.T) {
	t.Parallel()
	s, writes, warn := recordingStore(func(int) error { return nil })
	for _, rows := range [][]categorization{acceptedBatch("A", "B"), acceptedBatch("C")} {
		s.accept(rows)
	}
	if len(*writes) != 2 || (*writes)[0][0] != "A" || (*writes)[1][0] != "C" {
		t.Errorf("writes = %v, want one per batch", *writes)
	}
	if s.stored != 3 || s.total != 20 || len(s.pending) != 0 {
		t.Errorf("stored=%d total=%d pending=%d, want 3/20/0", s.stored, s.total, len(s.pending))
	}
	// Nothing outstanding: the end-of-run finish must not write again.
	if err := s.finish(context.Background(), func([]categorization, error) error {
		t.Error("a store with nothing held dumped")
		return nil
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(*writes) != 2 {
		t.Errorf("%d write(s) after an empty finish, want 2", len(*writes))
	}
	if warn.Len() != 0 {
		t.Errorf("a clean run warned: %q", warn.String())
	}
}

// TestBatchStoreHoldsAnswersAFailedWriteWouldLose pins the hold: an
// answer the model was already paid for is never dropped for a
// transient store failure.
func TestBatchStoreHoldsAnswersAFailedWriteWouldLose(t *testing.T) {
	t.Parallel()
	s, writes, warn := recordingStore(func(attempt int) error {
		if attempt == 1 {
			return errors.New("gold is locked")
		}
		return nil
	})
	s.accept(acceptedBatch("A", "B"))
	if len(s.pending) != 2 || s.stored != 0 {
		t.Fatalf("after a failed write: pending=%d stored=%d, want 2/0", len(s.pending), s.stored)
	}
	if !strings.Contains(warn.String(), "2 verdict(s) held") ||
		!strings.Contains(warn.String(), "gold is locked") {
		t.Errorf("warning = %q, want the count and the cause", warn.String())
	}

	// The next batch's write carries the held verdicts with its own.
	s.accept(acceptedBatch("C"))
	if len(*writes) != 1 || strings.Join((*writes)[0], ",") != "A,B,C" {
		t.Errorf("writes = %v, want one write carrying the held verdicts too", *writes)
	}
	if s.stored != 3 || len(s.pending) != 0 {
		t.Errorf("stored=%d pending=%d, want 3/0", s.stored, len(s.pending))
	}
}

// TestBatchStoreOpensNothingForABatchThatAcceptedNothing pins the empty
// case: a batch the gauntlet rejected outright has nothing to write, so
// it must not open gold.
func TestBatchStoreOpensNothingForABatchThatAcceptedNothing(t *testing.T) {
	t.Parallel()
	s, writes, _ := recordingStore(func(int) error {
		t.Error("an empty batch must not open gold")
		return nil
	})
	s.accept(nil)
	if len(*writes) != 0 || s.stored != 0 {
		t.Errorf("writes=%d stored=%d, want 0/0", len(*writes), s.stored)
	}
}

// TestBatchStoreFinishWritesWhatTheRunHeld pins the end-of-run path:
// finish's retries are what turn a held batch into a stored one when
// no further batch follows.
func TestBatchStoreFinishWritesWhatTheRunHeld(t *testing.T) {
	shortFlushLadder(t)
	s, writes, _ := recordingStore(func(attempt int) error {
		if attempt < 3 {
			return errors.New("gold is locked")
		}
		return nil
	})
	s.accept(acceptedBatch("A"))
	if err := s.finish(context.Background(), func([]categorization, error) error {
		t.Error("finish dumped answers its retry stored")
		return nil
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(*writes) != 1 || s.stored != 1 {
		t.Errorf("writes=%d stored=%d, want 1/1", len(*writes), s.stored)
	}
}

// TestBatchStoreFinishDumpsWhatItCannotStore: when every retry fails,
// the held answers go to dump, and the error dump words exits
// ExitOpenFailed.
func TestBatchStoreFinishDumpsWhatItCannotStore(t *testing.T) {
	shortFlushLadder(t)
	s, _, _ := recordingStore(func(int) error { return errors.New("gold is locked") })
	s.accept(acceptedBatch("A", "B"))
	var dumped []string
	err := s.finish(context.Background(), func(held []categorization, err error) error {
		for _, v := range held {
			dumped = append(dumped, v.Signature)
		}
		return fmt.Errorf("could not store: %w", err)
	})
	var exit *errs.ExitError
	if !errors.As(err, &exit) || exit.Code != errs.ExitOpenFailed {
		t.Fatalf("err = %v, want an ExitError with code %d", err, errs.ExitOpenFailed)
	}
	if !strings.Contains(err.Error(), "could not store: gold is locked") {
		t.Errorf("err = %v, want dump's wording around the cause", err)
	}
	if strings.Join(dumped, ",") != "A,B" {
		t.Errorf("dumped %v, want the held A,B", dumped)
	}
}

// ---- modelPass.run -----------------------------------------------------------

// echoPass is a modelPass over plain strings: a candidate is valid
// unless it starts with "bad", and the endpoint is the caller's.
func echoPass(call llmCall, maxAttempts int) modelPass[string, categorization] {
	return modelPass[string, categorization]{
		prompt: func(batch []string, _ []invalidRow) string { return strings.Join(batch, "\n") },
		parse: func(body string, _ []string) ([]categorization, []invalidRow) {
			var ok []categorization
			var bad []invalidRow
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "bad") {
					bad = append(bad, invalidRow{})
					continue
				}
				ok = append(ok, categorization{Signature: line})
			}
			return ok, bad
		},
		key:         func(v categorization) string { return v.Signature },
		call:        call,
		maxAttempts: maxAttempts,
		stdout:      &bytes.Buffer{},
		label:       func(batchAt, int) string { return "test" },
	}
}

// TestModelPassRunStoresAFailedBatchsAnswers: the run hands every batch
// to the store, a batch the endpoint fails under included, and stops
// there. The batch's answers from before the failure were paid for.
func TestModelPassRunStoresAFailedBatchsAnswers(t *testing.T) {
	t.Parallel()
	n := 0
	call := func(_ context.Context, _, user string) (string, error) {
		n++
		if n == 3 {
			return "", errors.New("read response: context deadline exceeded")
		}
		return user, nil
	}
	s, writes, _ := recordingStore(func(int) error { return nil })
	var after []batchAt
	r, err := echoPass(call, 2).run(context.Background(), [][]string{{"A", "B"}, {"C", "bad D"}, {"E"}}, s,
		func(at batchAt, _ []string, _ []categorization, _, _ int) { after = append(after, at) })
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err = %v, want the endpoint's", err)
	}
	if r.stopped != (batchAt{2, 3}) {
		t.Errorf("stopped = %+v, want batch 2 of 3", r.stopped)
	}
	if got := fmt.Sprint(*writes); got != "[[A B] [C]]" {
		t.Errorf("writes = %s, want batch 1 and batch 2's answer from before the failure", got)
	}
	if len(after) != 1 || after[0] != (batchAt{1, 3}) {
		t.Errorf("after ran for %v, want batch 1 only", after)
	}
	if r.attempts != 3 || r.rejected != 1 || len(r.answers) != 3 {
		t.Errorf("attempts=%d rejected=%d answers=%d, want 3/1/3", r.attempts, r.rejected, len(r.answers))
	}
}

// TestModelPassRunWithoutAStoreKeepsNothing: a dry run passes no store
// and still collects every answer.
func TestModelPassRunWithoutAStoreKeepsNothing(t *testing.T) {
	t.Parallel()
	call := func(_ context.Context, _, user string) (string, error) { return user, nil }
	r, err := echoPass(call, 1).run(context.Background(), [][]string{{"A"}, {"B"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.answers) != 2 || r.attempts != 2 || r.stopped != (batchAt{}) {
		t.Errorf("result = %+v, want two answers over two calls", r)
	}
}
