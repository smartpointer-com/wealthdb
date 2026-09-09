package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

// ---- persistCategorizations --------------------------------------------------

// openMerchantStore is a migrated in-memory gold, which is all the
// merchant store needs: it is keyed by signature alone and joins to
// nothing.
func openMerchantStore(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate gold: %v", err)
	}
	return db, ctx
}

type storedVerdict struct {
	name, detailed, model string
	sigVersion            int
	assignedAt            int64
}

func readVerdict(t *testing.T, db *sql.DB, ctx context.Context, sig string) storedVerdict {
	t.Helper()
	var v storedVerdict
	if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed, model_name, signature_version, assigned_at
          FROM spend_merchant_categories WHERE merchant_signature = ?`, sig).
		Scan(&v.name, &v.detailed, &v.model, &v.sigVersion, &v.assignedAt); err != nil {
		t.Fatalf("read verdict %q: %v", sig, err)
	}
	return v
}

// TestPersistCategorizationsUpsertsBySignature pins the store's key: a
// re-run with a better model overwrites in place rather than accumulating
// a second answer for the same merchant, and the count returned is the
// table's, not the write's.
func TestPersistCategorizationsUpsertsBySignature(t *testing.T) {
	db, ctx := openMerchantStore(t)

	total, err := persistCategorizations(ctx, db, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Cafe", "FOOD_AND_DRINK_COFFEE"},
		{"NORTHWIND HARDWARE", "Northwind Hardware", "HOME_IMPROVEMENT_HARDWARE"},
	}, 1_700_000_000, "first-model")
	if err != nil || total != 2 {
		t.Fatalf("first write = (%d, %v), want (2, nil)", total, err)
	}
	if got := readVerdict(t, db, ctx, "BLUE HARBOUR CAFE"); got.sigVersion != spending.SignatureVersion {
		t.Errorf("signature_version = %d, want the version that produced the key (%d)",
			got.sigVersion, spending.SignatureVersion)
	}

	// Same signature, a different verdict from a different model.
	total, err = persistCategorizations(ctx, db, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Coffee", "FOOD_AND_DRINK_RESTAURANT"},
	}, 1_700_009_999, "second-model")
	if err != nil || total != 2 {
		t.Fatalf("re-write = (%d, %v), want (2, nil) — the store is keyed by signature", total, err)
	}
	want := storedVerdict{"Blue Harbour Coffee", "FOOD_AND_DRINK_RESTAURANT", "second-model",
		spending.SignatureVersion, 1_700_009_999}
	if got := readVerdict(t, db, ctx, "BLUE HARBOUR CAFE"); got != want {
		t.Errorf("verdict = %+v, want %+v", got, want)
	}
	// The merchant nobody re-asked about is untouched.
	if got := readVerdict(t, db, ctx, "NORTHWIND HARDWARE"); got.model != "first-model" {
		t.Errorf("unrelated verdict model = %q, want it left alone", got.model)
	}
}

// TestPersistCategorizationsWritesNothingWhenGoldRefuses pins the
// contract verdictStore's hold depends on: a write that cannot go through
// reports it and leaves the store exactly as it was, so the caller can
// safely keep the verdicts and try again. Read-only is the real case —
// gold is one read-write handle or many read-only ones, so a concurrent
// reader is enough to refuse the reopen.
func TestPersistCategorizationsWritesNothingWhenGoldRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gold.db")
	db, err := gold.Open(path, gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate gold: %v", err)
	}
	if _, err := persistCategorizations(ctx, db, []categorization{
		{"BLUE HARBOUR CAFE", "Blue Harbour Cafe", "FOOD_AND_DRINK_COFFEE"},
	}, 1_700_000_000, "first-model"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}

	ro, err := gold.Open(path, gold.ModeReadOnly)
	if err != nil {
		t.Fatalf("reopen read-only: %v", err)
	}
	defer ro.Close()
	if _, err := persistCategorizations(ctx, ro, []categorization{
		{"NORTHWIND HARDWARE", "Northwind Hardware", "HOME_IMPROVEMENT_HARDWARE"},
	}, 1_700_009_999, "second-model"); err == nil {
		t.Fatal("a write gold refused must report, not report success")
	}
	var n int
	if err := ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d row(s) after the refused write, want the 1 that was there before", n)
	}
}

// ---- verdictStore ------------------------------------------------------------

// recordingStore is a verdictStore whose writes are captured instead of
// made, with `fail` deciding which write attempt fails.
func recordingStore(fail func(attempt int) error) (*verdictStore, *[][]string, *bytes.Buffer) {
	var writes [][]string
	warn := &bytes.Buffer{}
	attempt := 0
	s := &verdictStore{warn: warn}
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

func acceptedBatch(sigs ...string) batchOutcome {
	o := batchOutcome{Size: len(sigs)}
	for _, s := range sigs {
		o.Accepted = append(o.Accepted, categorization{s, s, "FOOD_AND_DRINK_COFFEE"})
	}
	return o
}

// TestVerdictStoreWritesEachBatchAsItCompletes pins why the write is not
// deferred to the end of the run: a run that dies partway keeps every
// batch that finished.
func TestVerdictStoreWritesEachBatchAsItCompletes(t *testing.T) {
	s, writes, warn := recordingStore(func(int) error { return nil })
	for _, o := range []batchOutcome{acceptedBatch("A", "B"), acceptedBatch("C")} {
		if err := s.accept(o); err != nil {
			t.Fatalf("accept: %v", err)
		}
	}
	if len(*writes) != 2 || (*writes)[0][0] != "A" || (*writes)[1][0] != "C" {
		t.Errorf("writes = %v, want one per batch", *writes)
	}
	if s.stored != 3 || s.completed != 2 || s.total != 20 || len(s.pending) != 0 {
		t.Errorf("stored=%d completed=%d total=%d pending=%d, want 3/2/20/0",
			s.stored, s.completed, s.total, len(s.pending))
	}
	// Nothing outstanding: the end-of-run flush must not write again.
	if err := s.flush(); err != nil {
		t.Fatalf("idempotent flush: %v", err)
	}
	if len(*writes) != 2 {
		t.Errorf("%d write(s) after an empty flush, want 2", len(*writes))
	}
	if warn.Len() != 0 {
		t.Errorf("a clean run warned: %q", warn.String())
	}
}

// TestVerdictStoreHoldsVerdictsAFailedWriteWouldLose pins the hold: an
// answer the model was already paid for is never dropped for a transient
// store failure, and the batch it came from does not count as completed
// until it actually reaches gold.
func TestVerdictStoreHoldsVerdictsAFailedWriteWouldLose(t *testing.T) {
	s, writes, warn := recordingStore(func(attempt int) error {
		if attempt == 1 {
			return errors.New("gold is locked")
		}
		return nil
	})
	if err := s.accept(acceptedBatch("A", "B")); err != nil {
		t.Fatalf("accept must not fail the run: %v", err)
	}
	if len(s.pending) != 2 || s.stored != 0 || s.completed != 0 {
		t.Fatalf("after a failed write: pending=%d stored=%d completed=%d, want 2/0/0",
			len(s.pending), s.stored, s.completed)
	}
	if !strings.Contains(warn.String(), "2 verdict(s) held") ||
		!strings.Contains(warn.String(), "gold is locked") {
		t.Errorf("warning = %q, want the count and the cause", warn.String())
	}

	// The next batch's flush writes the held verdicts with its own, and
	// both batches complete at once.
	if err := s.accept(acceptedBatch("C")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(*writes) != 1 || strings.Join((*writes)[0], ",") != "A,B,C" {
		t.Errorf("writes = %v, want one write carrying the held verdicts too", *writes)
	}
	if s.stored != 3 || s.completed != 2 || len(s.pending) != 0 {
		t.Errorf("stored=%d completed=%d pending=%d, want 3/2/0",
			s.stored, s.completed, len(s.pending))
	}
}

// TestVerdictStoreCompletesABatchThatAcceptedNothing pins the empty case:
// a batch the gauntlet rejected outright has nothing to write, so it must
// not open gold — but it is still a batch the run finished.
func TestVerdictStoreCompletesABatchThatAcceptedNothing(t *testing.T) {
	s, writes, _ := recordingStore(func(int) error {
		t.Error("an empty batch must not open gold")
		return nil
	})
	if err := s.accept(batchOutcome{Size: 4}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if len(*writes) != 0 || s.stored != 0 || s.completed != 1 {
		t.Errorf("writes=%d stored=%d completed=%d, want 0/0/1", len(*writes), s.stored, s.completed)
	}
}

// TestVerdictStoreRetriedFlushWritesWhatTheRunHeld pins the end-of-run
// path: retryFlush is what turns a held batch into a stored one when no
// further batch follows.
func TestVerdictStoreRetriedFlushWritesWhatTheRunHeld(t *testing.T) {
	s, writes, _ := recordingStore(func(attempt int) error {
		if attempt < 3 {
			return errors.New("gold is locked")
		}
		return nil
	})
	if err := s.accept(acceptedBatch("A")); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := retryFlush(context.Background(), s.flush); err != nil {
		t.Fatalf("retryFlush: %v", err)
	}
	if len(*writes) != 1 || s.stored != 1 || s.completed != 1 {
		t.Errorf("writes=%d stored=%d completed=%d, want 1/1/1", len(*writes), s.stored, s.completed)
	}
}
